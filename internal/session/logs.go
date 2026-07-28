package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/fsx"
)

// Metadata is the durable description of a session. It is written to
// meta.json so a restarted daemon can still list, tail, and head a session
// whose process is long gone.
type Metadata struct {
	ID             string            `json:"id"`
	Name           string            `json:"name,omitempty"`
	Command        []string          `json:"command"`
	CommandLine    string            `json:"command_line,omitempty"`
	Shell          bool              `json:"shell"`
	ShellID        string            `json:"shell_id,omitempty"`
	ShellPath      string            `json:"shell_path,omitempty"`
	ShellName      string            `json:"shell_name,omitempty"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env,omitempty"`
	Cols           int               `json:"cols"`
	Rows           int               `json:"rows"`
	PID            int               `json:"pid,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	ExitedAt       *time.Time        `json:"exited_at,omitempty"`
	ExitCode       *int              `json:"exit_code,omitempty"`
	KilledBy       string            `json:"killed_by,omitempty"`
	TranscriptLine int               `json:"transcript_lines"`
}

// Running reports whether the session's process was still alive when the
// metadata was written.
func (m Metadata) Running() bool { return m.ExitedAt == nil }

// logStore owns one session's on-disk files.
//
// Two files are kept for different readers. raw.log is the exact PTY byte
// stream, which the human application replays to reconstruct a screen
// faithfully. transcript.log is text, one line per line evicted off the top of
// the screen, which it_tail and it_head read. Neither can serve the other's
// purpose: raw bytes are meaningless as text, and text cannot restore colour
// or cursor state.
type logStore struct {
	mu sync.Mutex

	directory string
	metaPath  string

	raw       *os.File
	rawWriter *bufio.Writer
	rawBytes  int64
	rawMax    int64

	transcript       *os.File
	transcriptWriter *bufio.Writer
	transcriptLines  int
	transcriptMax    int

	closed bool
}

func openLogStore(directory string, rawMax int64, transcriptMax int) (*logStore, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	raw, err := os.OpenFile(filepath.Join(directory, "raw.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open raw log: %w", err)
	}
	transcript, err := os.OpenFile(filepath.Join(directory, "transcript.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("open transcript log: %w", err)
	}
	rawInfo, _ := raw.Stat()
	var rawBytes int64
	if rawInfo != nil {
		rawBytes = rawInfo.Size()
	}
	return &logStore{
		directory:        directory,
		metaPath:         filepath.Join(directory, "meta.json"),
		raw:              raw,
		rawWriter:        bufio.NewWriterSize(raw, 32<<10),
		rawBytes:         rawBytes,
		rawMax:           rawMax,
		transcript:       transcript,
		transcriptWriter: bufio.NewWriterSize(transcript, 32<<10),
		transcriptMax:    transcriptMax,
	}, nil
}

// writeRaw appends exact PTY bytes, rotating once when the cap is reached so
// a runaway program cannot fill the disk.
func (s *logStore) writeRaw(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.rawBytes+int64(len(p)) > s.rawMax {
		s.rotateRaw()
	}
	n, _ := s.rawWriter.Write(p)
	s.rawBytes += int64(n)
}

// rotateRaw moves the current raw log aside and starts a new one. Callers hold
// the lock. Only one generation is kept: the point is bounded disk use, and a
// human replaying a screen only ever needs the tail.
func (s *logStore) rotateRaw() {
	_ = s.rawWriter.Flush()
	_ = s.raw.Close()
	current := filepath.Join(s.directory, "raw.log")
	_ = os.Remove(current + ".1")
	_ = os.Rename(current, current+".1")
	raw, err := os.OpenFile(current, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Losing the raw log degrades human replay but must not stop the
		// session or the transcript, so writes are dropped rather than failing.
		s.raw, s.rawWriter, s.rawBytes = nil, bufio.NewWriter(io.Discard), 0
		return
	}
	s.raw, s.rawWriter, s.rawBytes = raw, bufio.NewWriterSize(raw, 32<<10), 0
}

// writeTranscript appends text lines evicted from the top of the screen.
func (s *logStore) writeTranscript(lines []string) {
	if len(lines) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, line := range lines {
		s.transcriptWriter.WriteString(line)
		s.transcriptWriter.WriteByte('\n')
		s.transcriptLines++
	}
	if s.transcriptLines > s.transcriptMax {
		s.truncateTranscript()
	}
}

// truncateTranscript drops the oldest half of the transcript when it exceeds
// its line cap. Callers hold the lock.
//
// The oldest lines go rather than the newest: a session that has produced more
// than the cap is almost always a long-running process whose recent output
// matters. it_head reports the loss instead of pretending the log is complete.
func (s *logStore) truncateTranscript() {
	_ = s.transcriptWriter.Flush()
	path := filepath.Join(s.directory, "transcript.log")
	source, err := os.Open(path)
	if err != nil {
		return
	}
	defer source.Close()

	keep := s.transcriptMax / 2
	skip := s.transcriptLines - keep
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	temporary, err := os.CreateTemp(s.directory, ".transcript-*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	writer := bufio.NewWriterSize(temporary, 64<<10)
	index, kept := 0, 0
	for scanner.Scan() {
		if index < skip {
			index++
			continue
		}
		writer.Write(scanner.Bytes())
		writer.WriteByte('\n')
		kept++
		index++
	}
	if writer.Flush() != nil || temporary.Sync() != nil || temporary.Close() != nil {
		os.Remove(name)
		return
	}
	_ = s.transcript.Close()
	if err := fsx.Replace(name, path); err != nil {
		os.Remove(name)
		// Reopen the original so writes continue.
		if reopened, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			s.transcript = reopened
			s.transcriptWriter = bufio.NewWriterSize(reopened, 32<<10)
		}
		return
	}
	reopened, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.transcript, s.transcriptWriter = nil, bufio.NewWriter(io.Discard)
		return
	}
	s.transcript = reopened
	s.transcriptWriter = bufio.NewWriterSize(reopened, 32<<10)
	s.transcriptLines = kept
}

// flush pushes buffered writes to the operating system.
func (s *logStore) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_ = s.rawWriter.Flush()
	_ = s.transcriptWriter.Flush()
}

// close flushes and releases both files.
func (s *logStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.rawWriter != nil {
		_ = s.rawWriter.Flush()
	}
	if s.transcriptWriter != nil {
		_ = s.transcriptWriter.Flush()
	}
	if s.raw != nil {
		_ = s.raw.Sync()
		_ = s.raw.Close()
	}
	if s.transcript != nil {
		_ = s.transcript.Sync()
		_ = s.transcript.Close()
	}
}

// lineCount reports how many transcript lines have been written.
func (s *logStore) lineCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcriptLines
}

// transcriptPath is the absolute path handed to an agent whose output was
// truncated, so it can always read the rest with ordinary file tools.
func (s *logStore) transcriptPath() string {
	return filepath.Join(s.directory, "transcript.log")
}

// writeMetadata atomically publishes meta.json.
func (s *logStore) writeMetadata(metadata Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeMetadataFile(s.metaPath, metadata)
}

func writeMetadataFile(path string, metadata Metadata) error {
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".meta-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return fsx.Replace(name, path)
}

// ReadMetadata loads a session's meta.json from disk.
func ReadMetadata(directory string) (Metadata, error) {
	var metadata Metadata
	raw, err := os.ReadFile(filepath.Join(directory, "meta.json"))
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return metadata, fmt.Errorf("parse session metadata: %w", err)
	}
	return metadata, nil
}

// WriteMetadata publishes a session's meta.json without needing the session
// itself, which is how a name can be taken away from one whose process is long
// gone. The write is atomic, so a daemon that dies mid-update leaves the
// previous file rather than a truncated one.
func WriteMetadata(directory string, metadata Metadata) error {
	return writeMetadataFile(filepath.Join(directory, "meta.json"), metadata)
}

// LogSlice is the result of reading part of a transcript.
type LogSlice struct {
	Lines []string
	// Total is the number of lines in the file, so a caller can report how
	// much it did not show.
	Total int
	// AtStart and AtEnd report whether the slice reaches the file boundary.
	AtStart, AtEnd bool
}

// Head reads the first n lines of a transcript.
func Head(path string, n int) (LogSlice, error) {
	if n < 1 {
		return LogSlice{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LogSlice{AtStart: true, AtEnd: true}, nil
		}
		return LogSlice{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var lines []string
	total := 0
	for scanner.Scan() {
		total++
		if len(lines) < n {
			lines = append(lines, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return LogSlice{}, err
	}
	return LogSlice{Lines: lines, Total: total, AtStart: true, AtEnd: len(lines) == total}, nil
}

// Tail reads the last n lines of a transcript.
//
// It reads backwards in bounded chunks rather than loading the file, so
// answering a 100-line request against a 64 MiB transcript stays cheap.
func Tail(path string, n int) (LogSlice, error) {
	if n < 1 {
		return LogSlice{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LogSlice{AtStart: true, AtEnd: true}, nil
		}
		return LogSlice{}, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return LogSlice{}, err
	}
	size := info.Size()
	if size == 0 {
		return LogSlice{AtStart: true, AtEnd: true}, nil
	}

	const chunk = 64 << 10
	var collected []byte
	position := size
	newlines := 0
	for position > 0 && newlines <= n {
		read := int64(chunk)
		if position < read {
			read = position
		}
		position -= read
		buffer := make([]byte, read)
		if _, err := file.ReadAt(buffer, position); err != nil && err != io.EOF {
			return LogSlice{}, err
		}
		collected = append(buffer, collected...)
		newlines = bytes.Count(collected, []byte{'\n'})
	}

	text := strings.TrimSuffix(string(collected), "\n")
	lines := strings.Split(text, "\n")
	if position > 0 && len(lines) > 0 {
		// The first line of the window may be a fragment of a longer line.
		lines = lines[1:]
	}
	atStart := position == 0
	if len(lines) > n {
		lines = lines[len(lines)-n:]
		atStart = false
	}

	total, err := countLines(path)
	if err != nil {
		return LogSlice{}, err
	}
	return LogSlice{Lines: lines, Total: total, AtStart: atStart || len(lines) == total, AtEnd: true}, nil
}

func countLines(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	buffer := make([]byte, 64<<10)
	count := 0
	for {
		n, err := file.Read(buffer)
		count += bytes.Count(buffer[:n], []byte{'\n'})
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
	}
}

// TailRaw returns up to n bytes from the end of the raw log, used by the
// interactive application to reconstruct a screen on attach.
func TailRaw(path string, n int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - n
	if start < 0 {
		start = 0
	}
	buffer := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(buffer, start); err != nil && err != io.EOF {
		return nil, err
	}
	return buffer, nil
}
