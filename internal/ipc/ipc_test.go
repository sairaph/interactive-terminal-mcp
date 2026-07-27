package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoServer is a minimal daemon stand-in: it answers pings and echoes back
// whatever arguments it is given, so the client can be tested on its own.
func echoServer(t *testing.T, socket string) net.Listener {
	t.Helper()
	listener, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				encoder := json.NewEncoder(conn)
				for {
					line, err := readLine(reader)
					if err != nil {
						return
					}
					var request Request
					if json.Unmarshal(line, &request) != nil {
						return
					}
					response := Response{V: Version, ID: request.ID, OK: true, Result: request.Args}
					if request.Op == "boom" {
						response.OK = false
						response.Result = nil
						response.Error = &Error{Code: CodeInvalidInput, Message: "boom", Hint: "do not"}
					}
					if encoder.Encode(response) != nil {
						return
					}
				}
			}()
		}
	}()
	return listener
}

func shortSocket(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\itm-test-` + t.Name()
	}
	// A t.TempDir() path can exceed the kernel's sun_path limit, so use a
	// deliberately short directory.
	directory, err := os.MkdirTemp("", "itm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return filepath.Join(directory, "s")
}

func TestCallRoundTrip(t *testing.T) {
	socket := shortSocket(t)
	echoServer(t, socket)

	client, err := Connect(context.Background(), socket)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	var result map[string]any
	if err := client.Call(context.Background(), "session.list", map[string]any{"a": float64(1)}, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result["a"] != float64(1) {
		t.Errorf("arguments did not round trip: %v", result)
	}
}

// A typed failure must survive the wire intact, because the error code drives
// the whole agent-facing error contract.
func TestTypedErrorsSurviveTheWire(t *testing.T) {
	socket := shortSocket(t)
	echoServer(t, socket)

	client, err := Connect(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.Call(context.Background(), "boom", nil, nil)
	if err == nil {
		t.Fatal("expected a failure")
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected a typed error, got %T", err)
	}
	if typed.Code != CodeInvalidInput || typed.Hint != "do not" {
		t.Errorf("error lost detail: %+v", typed)
	}
}

// Many callers share one connection, so responses must never be crossed.
func TestConcurrentCallsStayMatched(t *testing.T) {
	socket := shortSocket(t)
	echoServer(t, socket)

	client, err := Connect(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var group sync.WaitGroup
	for index := range 40 {
		group.Add(1)
		go func() {
			defer group.Done()
			var result map[string]any
			if err := client.Call(context.Background(), "session.list", map[string]any{"n": float64(index)}, &result); err != nil {
				t.Errorf("call %d: %v", index, err)
				return
			}
			if result["n"] != float64(index) {
				t.Errorf("call %d got the wrong response: %v", index, result)
			}
		}()
	}
	group.Wait()
}

func TestConnectFailsCleanlyWithoutADaemon(t *testing.T) {
	socket := shortSocket(t)
	_, err := Connect(context.Background(), socket)
	if err == nil {
		t.Fatal("connecting with no daemon should fail")
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeDaemonUnavailable {
		t.Fatalf("expected daemon_unavailable, got %v", err)
	}
	// The message has to tell a user what to do next, not just that it failed.
	if !strings.Contains(typed.Hint, "doctor") {
		t.Errorf("hint should point at doctor, got %q", typed.Hint)
	}
}

// A socket left behind by a crashed daemon looks identical to a live one.
// Removing it is only safe when nothing answers.
func TestStaleSocketIsReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes cannot go stale on disk")
	}
	socket := shortSocket(t)
	if err := os.WriteFile(socket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen should replace a stale socket: %v", err)
	}
	listener.Close()
}

// A live daemon must never be displaced: doing so would strand it and every
// session it owns.
func TestLiveSocketIsNotReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes reject a duplicate name themselves")
	}
	socket := shortSocket(t)
	echoServer(t, socket)

	if _, err := Listen(socket); err == nil {
		t.Fatal("a second listener should refuse to displace a live daemon")
	}
}

func TestSocketPermissionsAreRestrictive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipe permissions are set through a security descriptor")
	}
	socket := shortSocket(t)
	echoServer(t, socket)

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Sessions run arbitrary commands as this user, so the control socket must
	// not be reachable by anyone else on a shared machine.
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("socket permissions are %v; no group or other access is allowed", info.Mode().Perm())
	}
}

func TestCallHonoursAContextDeadline(t *testing.T) {
	socket := shortSocket(t)

	// A listener that accepts but never answers, so the call can only end by
	// its deadline.
	listener, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		<-time.After(30 * time.Second)
		conn.Close()
	}()

	client, err := Connect(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := client.Call(ctx, "session.list", nil, nil); err == nil {
		t.Fatal("a call past its deadline should fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the call ignored its deadline, took %v", elapsed)
	}
}

func TestClosedClientRejectsCalls(t *testing.T) {
	socket := shortSocket(t)
	echoServer(t, socket)

	client, err := Connect(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(context.Background(), "session.list", nil, nil); err == nil {
		t.Error("a closed client should refuse calls rather than hang")
	}
	// Closing twice must be safe; shutdown paths can reach it more than once.
	if err := client.Close(); err != nil {
		t.Errorf("a second Close should be a no-op, got %v", err)
	}
}
