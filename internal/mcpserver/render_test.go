package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

func text(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(result.Content))
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("expected text content")
	}
	return content.Text
}

// Every result must be a YAML-frontmatter document, because that is the shape
// the whole contract promises and agents are told to expect.
func requireDocument(t *testing.T, body string) {
	t.Helper()
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("result is not a frontmatter document:\n%s", body)
	}
	if strings.Count(body, "\n---\n") < 1 {
		t.Fatalf("frontmatter is not closed:\n%s", body)
	}
}

func sampleSession() ipc.SessionInfo {
	return ipc.SessionInfo{
		ID: "t-k3f9qa", Name: "build", Active: true, Running: true, PID: 48213,
		Command: []string{"/bin/bash"}, Cwd: "/home/user/project",
		Cols: 120, Rows: 30,
		CreatedAt:       time.Date(2026, 7, 27, 9, 12, 3, 0, time.UTC),
		LastActivityAt:  time.Date(2026, 7, 27, 9, 31, 44, 0, time.UTC),
		TranscriptLines: 1842, LogsRetained: true,
		LogPath: "/home/user/.interactive-terminal-mcp/sessions/t-k3f9qa/transcript.log",
	}
}

func TestScreenResultShape(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"lael@host:~/project$ make", "  CC   src/parser.o"},
		Cursor:  [2]int{3, 1}, Settled: true, WaitedMS: 820,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))
	requireDocument(t, body)

	for _, want := range []string{
		"session: t-k3f9qa", "name: build", "running: true",
		"size: [120, 30]", "cursor: [3, 1]", "settled: true", "waited_ms: 820",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "~~~text\nlael@host:~/project$ make") {
		t.Errorf("the screen should be fenced verbatim:\n%s", body)
	}
	// The agent must be told that history exists above the visible screen,
	// otherwise it has no way to know 1842 lines scrolled past.
	if !strings.Contains(body, "1842 earlier lines have scrolled off") {
		t.Errorf("missing the scrollback pointer:\n%s", body)
	}
}

// An unsettled screen is the single most important thing to report: the command
// may still be running and the output may be incomplete.
func TestUnsettledScreenWarnsAndOffersARetry(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"  CC   src/render.o"},
		Cursor:  [2]int{1, 1}, Settled: false, WaitedMS: 5000,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if !strings.Contains(body, "settled: false") {
		t.Errorf("frontmatter should report settled: false:\n%s", body)
	}
	if !strings.Contains(body, "still arriving") {
		t.Errorf("body should warn the screen may be incomplete:\n%s", body)
	}
	if !strings.Contains(body, `it_read({"session":"t-k3f9qa","wait":10})`) {
		t.Errorf("body should offer an exact retry call:\n%s", body)
	}
}

// A full-screen program's display is the whole story, and its output never
// reaches the log, so the guidance must say so rather than pointing at it_tail.
func TestAlternateScreenGuidance(t *testing.T) {
	info := sampleSession()
	info.AltScreen = true
	screen := ipc.Screen{Session: info, Lines: []string{"-- INSERT --"}, Settled: true}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if !strings.Contains(body, "alt_screen: true") {
		t.Errorf("frontmatter should report the alternate screen:\n%s", body)
	}
	if !strings.Contains(body, "keys` argument") {
		t.Errorf("body should point at the keys argument:\n%s", body)
	}
	if strings.Contains(body, "scrolled off this screen") {
		t.Errorf("alternate-screen output does not scroll, so it must not claim it did:\n%s", body)
	}
}

// A session that lived entirely inside a full-screen program has an empty log,
// so sending the caller to it_tail would waste a whole round trip.
func TestEndedSessionWithNoLogDoesNotSuggestTail(t *testing.T) {
	info := sampleSession()
	info.Running = false
	code := 0
	info.ExitCode = &code
	info.TranscriptLines = 0

	screen := ipc.Screen{Session: info, Lines: []string{}, Settled: true}
	body := text(t, successResult(screenFront(screen), screenBody(screen, sendGuidance(screen))))

	if strings.Contains(body, "it_tail") {
		t.Errorf("an empty log should not be advertised:\n%s", body)
	}
	if !strings.Contains(body, "everything it left behind") {
		t.Errorf("body should say the screen is all there is:\n%s", body)
	}
	if !strings.Contains(body, "(the screen is blank)") {
		t.Errorf("a blank screen should be stated, not silently empty:\n%s", body)
	}
}

func TestEndedSessionWithLogSuggestsTail(t *testing.T) {
	info := sampleSession()
	info.Running = false
	code := 1
	info.ExitCode = &code

	screen := ipc.Screen{Session: info, Lines: []string{"make: *** Error 1"}, Settled: true}
	body := text(t, successResult(screenFront(screen), screenBody(screen, sendGuidance(screen))))

	if !strings.Contains(body, "exit_code: 1") {
		t.Errorf("frontmatter should carry the exit code:\n%s", body)
	}
	if !strings.Contains(body, "with exit code 1") {
		t.Errorf("body should state the exit code in prose:\n%s", body)
	}
	if !strings.Contains(body, `it_tail({"session":"t-k3f9qa"})`) {
		t.Errorf("body should offer an exact tail call:\n%s", body)
	}
}

func TestNoActiveSessionGuidance(t *testing.T) {
	empty := ipc.ActiveResult{LiveSessions: 0, TotalSessions: 0}
	body := text(t, successResult(noActiveFront(empty), noActiveBody(empty)))
	requireDocument(t, body)
	if !strings.Contains(body, "active: null") {
		t.Errorf("frontmatter should report no active session:\n%s", body)
	}
	if !strings.Contains(body, "it_new({})") {
		t.Errorf("body should offer to create one:\n%s", body)
	}

	// With sessions present but none active, the advice is different: select
	// one rather than pile up another.
	existing := ipc.ActiveResult{LiveSessions: 2, TotalSessions: 3}
	body = text(t, successResult(noActiveFront(existing), noActiveBody(existing)))
	if !strings.Contains(body, "it_list({})") {
		t.Errorf("body should offer to list the existing sessions:\n%s", body)
	}
	if !strings.Contains(body, `it_active({"session":"<id>"})`) {
		t.Errorf("body should offer to select one:\n%s", body)
	}
}

func TestListResultShapeAndPagination(t *testing.T) {
	code := 0
	sessions := []ipc.SessionInfo{sampleSession(), {
		ID: "t-p2m8wd", Running: false, ExitCode: &code,
		Command: []string{"/bin/bash"}, Cols: 120, Rows: 30,
		CreatedAt:       time.Date(2026, 7, 27, 8, 58, 2, 0, time.UTC),
		LastActivityAt:  time.Date(2026, 7, 27, 9, 5, 10, 0, time.UTC),
		TranscriptLines: 96,
	}}
	front, body, err := renderList(ipc.ListResult{Active: "t-k3f9qa", Sessions: sessions}, 1, 2000)
	if err != nil {
		t.Fatal(err)
	}
	document := text(t, successResult(front, body))
	requireDocument(t, document)

	for _, want := range []string{"page: 1", "total: 2", "id: t-k3f9qa", "id: t-p2m8wd", "exit_code: 0"} {
		if !strings.Contains(document, want) {
			t.Errorf("list frontmatter missing %q:\n%s", want, document)
		}
	}
	if !strings.Contains(body, "2 sessions, 1 running.") {
		t.Errorf("body should summarise the page:\n%s", body)
	}
}

func TestEmptyListSuggestsCreating(t *testing.T) {
	front, body, err := renderList(ipc.ListResult{}, 1, 2000)
	if err != nil {
		t.Fatal(err)
	}
	document := text(t, successResult(front, body))
	if !strings.Contains(document, "it_new({})") {
		t.Errorf("an empty list should say how to create a session:\n%s", document)
	}
}

// A tiny budget must still return whole records and report the extra pages,
// never silently drop sessions.
func TestListPaginatesUnderATightBudget(t *testing.T) {
	var sessions []ipc.SessionInfo
	for index := range 12 {
		info := sampleSession()
		info.ID = "t-00000" + string(rune('a'+index))
		sessions = append(sessions, info)
	}
	front, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 300)
	if err != nil {
		t.Fatal(err)
	}
	metadata := front.(listMetadata)
	if metadata.TotalPages < 2 {
		t.Fatalf("a 300-token budget should need several pages, got %d", metadata.TotalPages)
	}
	if len(metadata.Sessions) == len(sessions) {
		t.Error("a tight budget should not fit every session on one page")
	}
	if !strings.Contains(body, `it_list({"page":2})`) {
		t.Errorf("body should offer the next page:\n%s", body)
	}
}

// Truncation must drop from the far end, so tail keeps the newest lines and
// head keeps the oldest: the caller always gets the part it asked for.
func TestLogTruncationKeepsTheRequestedEnd(t *testing.T) {
	var lines []string
	for index := 1; index <= 400; index++ {
		lines = append(lines, strings.Repeat("x", 60)+" line-"+itoa(index))
	}
	result := ipc.LogResult{
		Session: sampleSession(), Lines: lines, TotalLines: 4000,
		LogPath: "/tmp/transcript.log",
	}

	front, body, err := renderLog(result, 400, true, 400)
	if err != nil {
		t.Fatal(err)
	}
	metadata := front.(logMetadata)
	if metadata.LinesOmitted == 0 {
		t.Fatal("a 400-token budget should not fit 400 long lines")
	}
	if !strings.Contains(body, "line-400") {
		t.Errorf("tail must keep the newest lines:\n%s", firstLines(body, 6))
	}
	if strings.Contains(body, "line-1\n") {
		t.Errorf("tail should have dropped the oldest lines:\n%s", firstLines(body, 6))
	}
	if !strings.Contains(body, "omitted to fit the response budget") {
		t.Errorf("the omission must be reported, not silent:\n%s", body)
	}
	if !strings.Contains(body, "/tmp/transcript.log") {
		t.Errorf("the complete log path must be offered:\n%s", body)
	}

	front, body, err = renderLog(result, 400, false, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "line-1\n") {
		t.Errorf("head must keep the oldest lines:\n%s", firstLines(body, 6))
	}
	if strings.Contains(body, "line-400") {
		t.Errorf("head should have dropped the newest lines:\n%s", firstLines(body, 6))
	}
}

// A short log is not a truncated one; saying otherwise implies output is
// missing when none is.
func TestShortLogIsNotReportedAsTruncated(t *testing.T) {
	result := ipc.LogResult{
		Session: sampleSession(), Lines: []string{"one", "two"}, TotalLines: 2,
		LogPath: "/tmp/transcript.log",
	}
	front, body, err := renderLog(result, 100, true, 4000)
	if err != nil {
		t.Fatal(err)
	}
	metadata := front.(logMetadata)
	if metadata.Truncated {
		t.Error("a log shorter than the request is not truncated")
	}
	if !strings.Contains(body, "That is the complete log") {
		t.Errorf("body should say the log is complete:\n%s", body)
	}
}

// A session in a full-screen program has a log that stops where the program
// started, so the two parts must be labelled or they read as one stream.
func TestTailLabelsTheLiveScreenSeparately(t *testing.T) {
	result := ipc.LogResult{
		Session:     sampleSession(),
		Lines:       []string{"older output"},
		TotalLines:  1,
		ScreenLines: []string{"-- INSERT --"},
		AltScreen:   true,
		LogPath:     "/tmp/transcript.log",
	}
	_, body, err := renderLog(result, 100, true, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "never scrolls into the log") {
		t.Errorf("body should explain why the screen is shown separately:\n%s", body)
	}
	if strings.Index(body, "older output") > strings.Index(body, "-- INSERT --") {
		t.Error("the log should come before the live screen")
	}
}

func TestKillResultShape(t *testing.T) {
	code := 143
	result := ipc.KillResult{
		Killed: "t-k3f9qa", Name: "build", Signal: "TERM",
		ExitCode: &code, LogsRetained: false,
	}
	document := text(t, successResult(killFront(result), killBody(result)))
	requireDocument(t, document)
	if !strings.Contains(document, "killed: t-k3f9qa") {
		t.Errorf("frontmatter should name the session:\n%s", document)
	}
	// The consequence for logs must be explicit; an agent should not have to
	// know the retention setting to understand what just happened.
	if !strings.Contains(document, "logs were deleted") {
		t.Errorf("body should state what happened to the logs:\n%s", document)
	}

	escalated := ipc.KillResult{Killed: "t-k3f9qa", Signal: "TERM", Escalated: true, ExitCode: &code}
	document = text(t, successResult(killFront(escalated), killBody(escalated)))
	if !strings.Contains(document, "did not exit after TERM") {
		t.Errorf("escalation must be reported:\n%s", document)
	}

	// An interrupt is a request a program may ignore, so the report must
	// describe what was observed rather than what was asked for.
	stopped := ipc.KillResult{
		Killed: "t-k3f9qa", Signal: "INT", LogsRetained: true,
		Outcome: ipc.OutcomeQuiet, ObservedMS: 400,
	}
	document = text(t, successResult(killFront(stopped), killBody(stopped)))
	// Silence is evidence, not proof, and the wording must not overstate it.
	if !strings.Contains(document, "usually means the command stopped") {
		t.Errorf("a quiet interrupt should describe what was observed:\n%s", document)
	}
	if !strings.Contains(document, "a slow command looks the same") {
		t.Errorf("the inference should be marked as one:\n%s", document)
	}
	if !strings.Contains(document, "it_read") {
		t.Errorf("the caller should be offered a way to confirm:\n%s", document)
	}

	ignored := ipc.KillResult{
		Killed: "t-k3f9qa", Signal: "INT", LogsRetained: true,
		Outcome: ipc.OutcomeStillRunning, ObservedMS: 3000,
	}
	document = text(t, successResult(killFront(ignored), killBody(ignored)))
	if !strings.Contains(document, "did not stop") {
		t.Errorf("an ignored interrupt must say so rather than claim success:\n%s", document)
	}
	if !strings.Contains(document, `"signal":"TERM"`) {
		t.Errorf("an ignored interrupt should offer the escalation that cannot be ignored:\n%s", document)
	}
	if strings.Contains(document, "should have stopped") {
		t.Errorf("no unverified claim may survive:\n%s", document)
	}
}

func TestErrorResultShape(t *testing.T) {
	failure := &ipc.Error{
		Code: ipc.CodeSessionNotFound, Message: `no session matches "buidl"`,
		Hint:   "Call it_list() to see existing sessions, or it_new() to create one.",
		Fields: map[string]any{"session": "buidl"},
	}
	result := errorResult(failure)
	if !result.IsError {
		t.Error("a failure must set isError")
	}
	body := text(t, result)
	requireDocument(t, body)
	for _, want := range []string{
		"code: session_not_found", "buidl", "hint:", "## Error", "it_list()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("error document missing %q:\n%s", want, body)
		}
	}
}

// Program output containing tildes must not be able to close the fence and
// break out of the code block.
func TestFencesSurviveHostileOutput(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"~~~", "~~~~~ not a real fence", "still inside"},
		Settled: true,
	}
	body := screenBody(screen, nil)
	if !strings.Contains(body, "~~~~~~text\n") {
		t.Errorf("the fence should have grown past the content:\n%s", body)
	}
}

func TestSchemasDeclareTheDocumentedShape(t *testing.T) {
	settings := config.Default()

	kill := killSchema()
	required, _ := kill["required"].([]string)
	if len(required) != 1 || required[0] != "session" {
		t.Errorf("it_kill must require session, got %v", kill["required"])
	}

	for name, schema := range map[string]map[string]any{
		"it_active": activeSchema(), "it_list": listSchema(),
		"it_new": newSchema(settings), "it_read": readSchema(settings),
		"it_send": sendSchema(settings), "it_tail": logSchema(true),
		"it_head": logSchema(false),
	} {
		if schema["additionalProperties"] != false {
			t.Errorf("%s should reject unknown arguments", name)
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("%s has no properties", name)
		}
	}

	// wait must advertise the configured ceiling so a model can see the limit
	// rather than discovering it through a failed call.
	send := sendSchema(settings)["properties"].(map[string]any)
	wait := send["wait"].(map[string]any)
	if wait["maximum"] != settings.MaximumWaitSeconds {
		t.Errorf("wait maximum: got %v, want %d", wait["maximum"], settings.MaximumWaitSeconds)
	}

	// The keys description must enumerate the key names; a model cannot guess
	// a private grammar.
	keysDescription, _ := send["keys"].(map[string]any)["description"].(string)
	for _, want := range []string{"PAGE_UP", "CTRL+", "*n", "F1-F20"} {
		if !strings.Contains(keysDescription, want) {
			t.Errorf("keys description missing %q: %s", want, keysDescription)
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}

// The validator's own wording is the only machine-generated text an agent
// would meet in a product whose every other message is written by hand.
func TestValidationMessagesAreProse(t *testing.T) {
	cases := map[string]string{
		`validating "arguments": validating root: validating /properties/cols: minimum: 10/1 is less than 20.000000`:         "cols must be at least 20 (got 10)",
		`validating "arguments": validating root: validating /properties/wait: maximum: 100000/1 is greater than 300.000000`: "wait must be at most 300 (got 100000)",
	}
	for raw, want := range cases {
		got := cleanValidationMessage(raw)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if strings.Contains(got, "validating") || strings.Contains(got, ".000000") || strings.Contains(got, "/1") {
			t.Errorf("validator internals leaked: %q", got)
		}
	}
}

// truncated_by describes a truncation. Emitting it when nothing was truncated
// said two contradictory things at once about whether output was missing.
func TestTruncatedByOnlyAppearsWithTruncation(t *testing.T) {
	short := ipc.LogResult{
		Session: sampleSession(), Lines: []string{"one", "two"}, TotalLines: 2,
		LogPath: "/tmp/transcript.log",
	}
	front, _, err := renderLog(short, 100, true, 4000)
	if err != nil {
		t.Fatal(err)
	}
	metadata := front.(logMetadata)
	if metadata.Truncated {
		t.Error("a log shorter than the request is not truncated")
	}
	if metadata.TruncatedBy != "" {
		t.Errorf("truncated_by should be absent when nothing was truncated, got %q", metadata.TruncatedBy)
	}
}

// When the budget drops lines out of the middle of the requested range, the
// opposite end of the log cannot return them. Pointing there wastes a call.
func TestBudgetTruncationPointsAtTheFileNotTheOtherEnd(t *testing.T) {
	var lines []string
	for index := 1; index <= 400; index++ {
		lines = append(lines, strings.Repeat("x", 60)+" line-"+itoa(index))
	}
	result := ipc.LogResult{
		Session: sampleSession(), Lines: lines, TotalLines: 4000,
		LogPath: "/tmp/transcript.log",
	}
	_, body, err := renderLog(result, 400, true, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "/tmp/transcript.log") {
		t.Errorf("the file is the only way to reach the omitted lines:\n%s", body)
	}
	if strings.Contains(body, "Read the other end with") {
		t.Errorf("it_head cannot reach the middle of the range:\n%s", body)
	}
}

// The escalation message must name the signal that was actually sent.
func TestEscalationNamesTheSignalAsked(t *testing.T) {
	code := 137
	result := ipc.KillResult{Killed: "t-k3f9qa", Signal: "HUP", Escalated: true, ExitCode: &code}
	body := killBody(result)
	if !strings.Contains(body, "did not exit after HUP") {
		t.Errorf("the report should name HUP, not TERM:\n%s", body)
	}
}
