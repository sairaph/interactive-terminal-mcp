package mcpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/budget"
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
		Cursor:  [2]int{3, 1}, Settled: true, Observed: true, WaitedMS: 820,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))
	requireDocument(t, body)

	for _, want := range []string{
		"session: t-k3f9qa", "name: build", "running: true",
		"size: [120, 30]", "cursor: [3, 1]", "settled: true", "waited_ms: 820",
		"logs_retained: true",
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
	if !strings.Contains(body, "1842 earlier lines from above this screen") {
		t.Errorf("missing the scrollback pointer:\n%s", body)
	}
}

// An unsettled screen is the single most important thing to report: the command
// may still be running and the output may be incomplete.
func TestUnsettledScreenWarnsAndOffersARetry(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"  CC   src/render.o"},
		Cursor:  [2]int{1, 1}, Settled: false, Observed: true,
		WaitedMS: 5000, BudgetMS: 10000,
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
	screen := ipc.Screen{Session: info, Lines: []string{"-- INSERT --"}, Settled: true, Observed: true}
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

	screen := ipc.Screen{Session: info, Lines: []string{}, Settled: true, Observed: true}
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

	screen := ipc.Screen{Session: info, Lines: []string{"make: *** Error 1"}, Settled: true, Observed: true}
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

	// With live sessions present but none active, the advice is different:
	// select one rather than pile up another.
	existing := ipc.ActiveResult{LiveSessions: 2, TotalSessions: 3}
	body = text(t, successResult(noActiveFront(existing), noActiveBody(existing)))
	if !strings.Contains(body, "it_list({})") {
		t.Errorf("body should offer to list the existing sessions:\n%s", body)
	}
	if !strings.Contains(body, `it_active({"session":"<id>"})`) {
		t.Errorf("body should offer to select one:\n%s", body)
	}

	// When every session is dead, selecting one is useless advice: nothing can
	// be run in it. What is left is finding out what they left behind, which
	// it_list answers -- including whether the logs still exist, which under
	// the default retention policy they do not.
	allDead := ipc.ActiveResult{LiveSessions: 0, TotalSessions: 11}
	body = text(t, successResult(noActiveFront(allDead), noActiveBody(allDead)))
	if strings.Contains(body, "it_active({") {
		t.Errorf("selecting a dead session is not a useful next step:\n%s", body)
	}
	if strings.Contains(body, "it_tail") {
		t.Errorf("their logs may already have been deleted; do not promise them:\n%s", body)
	}
	if !strings.Contains(body, "it_list") || !strings.Contains(body, "it_new") {
		t.Errorf("body should offer to see what is left or start fresh:\n%s", body)
	}
}

// Two creates racing can leave the first reply's active flag stale. Telling
// the caller a session "is active" when another already took over would send
// the next call to the wrong terminal.
func TestNewReportsWhenItLostTheActiveSlot(t *testing.T) {
	info := sampleSession()
	info.Active = false
	screen := ipc.Screen{Session: info, Lines: []string{"$"}, Settled: true, Observed: true}
	body := screenBody(screen, newGuidance(screen))

	if strings.Contains(body, "is active.") {
		t.Errorf("a session that lost the active slot must not claim it:\n%s", body)
	}
	if !strings.Contains(body, "another session is active now") {
		t.Errorf("the caller should be told to name the session explicitly:\n%s", body)
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
	front, body, err := renderList(ipc.ListResult{Active: "t-k3f9qa", Sessions: sessions}, 1, 2000, true)
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
	front, body, err := renderList(ipc.ListResult{}, 1, 2000, false)
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
	front, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 300, false)
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
		Settled: true, Observed: true,
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

// The list is how a caller chooses a session, and paying two thousand tokens
// to answer "which one" crowds out the work itself.
func TestListIsCompactByDefault(t *testing.T) {
	var sessions []ipc.SessionInfo
	for index := range 11 {
		info := sampleSession()
		info.ID = "t-0000" + string(rune('a'+index))
		sessions = append(sessions, info)
	}

	compactFront, compactBody, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 100_000, false)
	if err != nil {
		t.Fatal(err)
	}
	verboseFront, _, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 100_000, true)
	if err != nil {
		t.Fatal(err)
	}

	compactText := text(t, successResult(compactFront, compactBody))
	verboseText := text(t, successResult(verboseFront, ""))

	compactTokens, err := budget.Count(compactText)
	if err != nil {
		t.Fatal(err)
	}
	verboseTokens, err := budget.Count(verboseText)
	if err != nil {
		t.Fatal(err)
	}
	// Measured at roughly 55% of verbose for eleven sessions. The bound is
	// loose enough not to break on wording, tight enough to catch a field
	// creeping back into the default.
	if compactTokens*4 > verboseTokens*3 {
		t.Errorf("compact should be materially cheaper: %d vs %d tokens", compactTokens, verboseTokens)
	}
	t.Logf("eleven sessions: compact %d tokens, verbose %d", compactTokens, verboseTokens)

	// What survives must still be enough to pick a session and know whether
	// its log is worth reading.
	for _, want := range []string{"id:", "running:", "command:", "transcript_lines:"} {
		if !strings.Contains(compactText, want) {
			t.Errorf("compact list dropped %q, which a caller needs:\n%s", want, compactText)
		}
	}
	for _, gone := range []string{"cwd:", "log_path:", "created_at:"} {
		if strings.Contains(compactText, gone) {
			t.Errorf("compact list still carries %q", gone)
		}
	}
	if !strings.Contains(compactBody, `"verbose":true`) {
		t.Errorf("the caller should be told how to get the rest:\n%s", compactBody)
	}
	if !strings.Contains(verboseText, "cwd:") {
		t.Errorf("verbose should restore the detail:\n%s", verboseText)
	}
}

// Tool text describes the tool as it is. A model reads these with no memory of
// what the product used to do, so retrospective or apologetic phrasing is pure
// noise to it, and a description that promises a field the tool stopped
// returning is worse than noise.
func TestToolTextIsForwardFacing(t *testing.T) {
	settings := config.Default()

	// Every string a caller can read, from the tool text and the schemas.
	texts := map[string]string{
		"instructions": Instructions,
	}
	schemas := map[string]map[string]any{
		"it_active": activeSchema(), "it_list": listSchema(),
		"it_new": newSchema(settings), "it_read": readSchema(settings),
		"it_send": sendSchema(settings), "it_kill": killSchema(),
		"it_tail": logSchema(true), "it_head": logSchema(false),
	}
	for name, schema := range schemas {
		properties, _ := schema["properties"].(map[string]any)
		for property, raw := range properties {
			definition, _ := raw.(map[string]any)
			if description, _ := definition["description"].(string); description != "" {
				texts[name+"."+property] = description
			}
		}
	}

	// Phrases that describe a change rather than the thing.
	banned := []string{
		"no longer", "now correctly", "used to", "previously", "as before",
		"has been fixed", "instead of the old", "in earlier versions",
		"we now", "this now", "changed to", "renamed",
	}
	for where, text := range texts {
		lowered := strings.ToLower(text)
		for _, phrase := range banned {
			if strings.Contains(lowered, phrase) {
				t.Errorf("%s reads as a changelog (%q): %s", where, phrase, text)
			}
		}
	}
}

// A description that names a field the tool does not return sends the caller
// looking for something that is not there.
func TestListDescriptionMatchesWhatListReturns(t *testing.T) {
	sessions := []ipc.SessionInfo{sampleSession()}

	compactFront, _, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 100_000, false)
	if err != nil {
		t.Fatal(err)
	}
	verboseFront, _, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 100_000, true)
	if err != nil {
		t.Fatal(err)
	}
	compact := text(t, successResult(compactFront, ""))
	verbose := text(t, successResult(verboseFront, ""))

	// Anything the default form drops must be described as needing verbose.
	for _, field := range []string{"cwd:", "log_path:", "size:"} {
		if strings.Contains(compact, field) {
			continue
		}
		if !strings.Contains(verbose, field) {
			t.Errorf("%s appears in neither form; the description should not imply it exists", field)
		}
	}
	if strings.Contains(compact, "cwd:") {
		t.Error("the default list should not carry the working directory")
	}
}

// --- what the wait actually established ------------------------------------

// A quiet screen and a finished command look identical. Where the terminal can
// tell them apart, the reply has to say which one this is: reporting quiet as
// finished is what made a caller stop reading a command that was still going.
func TestBusyTerminalIsReportedDespiteQuietOutput(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"lael@host:~/project$ sleep 15"},
		Settled: true, Observed: true, WaitedMS: 300, BudgetMS: 5000,
		Busy: true, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if !strings.Contains(body, "busy: true") {
		t.Errorf("frontmatter should report the terminal as busy:\n%s", body)
	}
	if !strings.Contains(body, "A command is still running in this terminal") {
		t.Errorf("a quiet screen over a running command must say so:\n%s", body)
	}
}

// An idle shell is the one case where quiet really does mean finished, and
// saying so is what makes the busy field worth reading at all.
func TestIdleTerminalIsReportedWithoutAWarning(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"lael@host:~/project$"},
		Settled: true, Observed: true, WaitedMS: 260, BudgetMS: 5000,
		Busy: false, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if !strings.Contains(body, "busy: false") {
		t.Errorf("frontmatter should report the terminal as idle:\n%s", body)
	}
	if strings.Contains(body, "still running") {
		t.Errorf("an idle terminal must not be described as running something:\n%s", body)
	}
}

// A full-screen program is always the foreground process, so reporting it as a
// running command would be true and useless on every single read.
func TestBusyIsNotReportedForAFullScreenProgram(t *testing.T) {
	info := sampleSession()
	info.AltScreen = true
	screen := ipc.Screen{
		Session: info, Lines: []string{"-- INSERT --"},
		Settled: true, Observed: true, Busy: true, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if strings.Contains(body, "busy:") {
		t.Errorf("a full-screen program is not a command that will finish:\n%s", body)
	}
}

// A call that did not wait established nothing. Reporting that as "output was
// still arriving" states the opposite of what happened.
func TestAnUnobservedScreenSaysNothingWasWaitedFor(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"lael@host:~/project$"},
		Settled: false, Observed: false, WaitedMS: 30,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if strings.Contains(body, "settled:") {
		t.Errorf("nothing was established, so settled must be absent rather than false:\n%s", body)
	}
	if strings.Contains(body, "still arriving") {
		t.Errorf("no wait ran, so nothing was seen arriving:\n%s", body)
	}
	if !strings.Contains(body, "captured without waiting") {
		t.Errorf("the reply should say no wait ran:\n%s", body)
	}
}

// A wait_for that timed out over an idle terminal means the command finished
// without printing that text, which is the opposite of what this used to claim.
func TestWaitForMissOverAnIdleTerminalDoesNotClaimTheCommandIsRunning(t *testing.T) {
	screen := ipc.Screen{
		Session:   sampleSession(),
		Lines:     []string{"lael@host:~/project$ ./build.sh", "done", "lael@host:~/project$"},
		Settled:   false,
		Observed:  true,
		WaitedFor: "BULK-DONE", Matched: false,
		WaitedMS: 30300, BudgetMS: 30000,
		Busy: false, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if strings.Contains(body, "still going") || strings.Contains(body, "still running") {
		t.Errorf("an idle terminal must not be reported as still working:\n%s", body)
	}
	if !strings.Contains(body, "most likely finished") {
		t.Errorf("the reply should say the command has probably finished:\n%s", body)
	}
}

// The same miss over a terminal that demonstrably still has work in it is the
// case where waiting again is the right advice.
func TestWaitForMissOverABusyTerminalSaysToKeepWaiting(t *testing.T) {
	screen := ipc.Screen{
		Session:   sampleSession(),
		Lines:     []string{"lael@host:~/project$ ./build.sh"},
		Observed:  true,
		WaitedFor: "BULK-DONE", Matched: false,
		WaitedMS: 30300, BudgetMS: 30000,
		Busy: true, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if !strings.Contains(body, "a command is still running in this terminal") {
		t.Errorf("a proven-busy terminal should be reported as such:\n%s", body)
	}
	if !strings.Contains(body, `"wait_for":"BULK-DONE"`) {
		t.Errorf("the reply should offer to keep waiting:\n%s", body)
	}
}

// --- guidance that matches the state it is describing -----------------------

// Suggesting it_send on a session this same reply reports as ended costs a
// round trip to be told what the caller already knew.
func TestListDoesNotSuggestTypingIntoEndedSessions(t *testing.T) {
	code := 0
	sessions := []ipc.SessionInfo{
		{ID: "t-aaa111", Running: false, ExitCode: &code},
		{ID: "t-bbb222", Running: false, ExitCode: &code},
	}
	_, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "it_send") {
		t.Errorf("nothing here can be typed into:\n%s", body)
	}
	if !strings.Contains(body, "it_new") {
		t.Errorf("the caller needs a way forward:\n%s", body)
	}
}

// With a mix, the suggestion has to name one that is actually running rather
// than whichever happens to be first.
func TestListSuggestsARunningSession(t *testing.T) {
	code := 0
	sessions := []ipc.SessionInfo{
		{ID: "t-aaa111", Running: false, ExitCode: &code},
		{ID: "t-bbb222", Running: true},
	}
	_, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `it_send({"session":"t-bbb222"`) {
		t.Errorf("the running session should be the one offered:\n%s", body)
	}
	if strings.Contains(body, `it_send({"session":"t-aaa111"`) {
		t.Errorf("the ended session must not be offered for input:\n%s", body)
	}
}

// Sessions disappearing from this list looks like data loss unless the rule
// behind it is stated.
func TestListExplainsWhyEndedSessionsDisappear(t *testing.T) {
	sessions := []ipc.SessionInfo{{ID: "t-aaa111", Running: true}}
	_, body, err := renderList(
		ipc.ListResult{Sessions: sessions, Retention: config.RetentionOnClose}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "deleted when a session closes") {
		t.Errorf("the retention rule should be stated:\n%s", body)
	}

	_, kept, err := renderList(
		ipc.ListResult{Sessions: sessions, Retention: config.RetentionWeek}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kept, "deleted when a session closes") {
		t.Errorf("that rule does not apply under a retention window:\n%s", kept)
	}
}

// Offering `ls -la` to PowerShell teaches an agent a command that fails there.
func TestExampleCommandMatchesTheRunningShell(t *testing.T) {
	for shell, want := range map[string]string{
		"Windows PowerShell": "Get-ChildItem",
		"PowerShell 7":       "Get-ChildItem",
		"Command Prompt":     "dir",
		"bash":               "ls -la",
	} {
		info := sampleSession()
		info.Shell = shell
		screen := ipc.Screen{Session: info, Lines: []string{"> "}, Settled: true, Observed: true}
		body := strings.Join(activeGuidance(screen), "\n")
		if !strings.Contains(body, want) {
			t.Errorf("%s should be offered %q, got:\n%s", shell, want, body)
		}
	}
}

// Typing a command line into a pager does not run it; the keys mean something
// else entirely to the program that owns the screen.
func TestActiveGuidanceOffersKeystrokesToAFullScreenProgram(t *testing.T) {
	info := sampleSession()
	info.AltScreen = true
	screen := ipc.Screen{Session: info, Lines: []string{":"}, Settled: true, Observed: true}
	body := strings.Join(activeGuidance(screen), "\n")

	if strings.Contains(body, `"text"`) {
		t.Errorf("a command line is the wrong thing to send here:\n%s", body)
	}
	if !strings.Contains(body, `"keys"`) {
		t.Errorf("keystrokes are what this session takes:\n%s", body)
	}
}

// When the terminal itself reports nothing running, a warning that output may
// not have arrived yet is filler over a fact the reply already carries.
func TestAnUnobservedScreenOverAnIdleTerminalStaysQuiet(t *testing.T) {
	screen := ipc.Screen{
		Session: sampleSession(),
		Lines:   []string{"lael@host:~/project$"},
		Settled: false, Observed: false, WaitedMS: 30,
		Busy: false, BusyKnown: true,
	}
	body := text(t, successResult(screenFront(screen), screenBody(screen, readGuidance(screen))))

	if strings.Contains(body, "captured without waiting") {
		t.Errorf("busy: false already answers this:\n%s", body)
	}
	if !strings.Contains(body, "busy: false") {
		t.Errorf("the fact itself must still be reported:\n%s", body)
	}
}

// The compact row drops detail, not facts. Omitting this one left every row
// claiming its logs were gone, including sessions still writing to them.
func TestCompactListRowKeepsLogRetention(t *testing.T) {
	sessions := []ipc.SessionInfo{
		{ID: "t-aaa111", Running: true, LogsRetained: true, TranscriptLines: 12},
	}
	_, _, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	front, _, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := front.(listMetadata).Sessions
	if len(rows) != 1 || !rows[0].LogsRetained {
		t.Errorf("a running session's logs are retained, got %+v", rows)
	}
}

// Page size differs between compact and verbose rows, so a continuation hint
// that drops verbose points at a page that does not exist in the other mode.
func TestPaginationHintKeepsVerbose(t *testing.T) {
	sessions := make([]ipc.SessionInfo, 40)
	for index := range sessions {
		sessions[index] = ipc.SessionInfo{
			ID: fmt.Sprintf("t-%06d", index), Running: true,
			Cwd:     "/home/user/some/fairly/long/working/directory",
			LogPath: "/home/user/.interactive-terminal-mcp/sessions/x/transcript.log",
			Cols:    120, Rows: 30,
		}
	}
	front, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 1500, true)
	if err != nil {
		t.Fatal(err)
	}
	if front.(listMetadata).TotalPages < 2 {
		t.Skip("the budget did not force a second page; the hint under test never appears")
	}
	if !strings.Contains(body, `"verbose":true`) {
		t.Errorf("the continuation hint must stay in verbose mode:\n%s", body)
	}
}

// A session started as a program rather than a shell says nothing about which
// syntax it takes, so the suggestion has to be one that works everywhere.
func TestUnknownShellExampleIsPortable(t *testing.T) {
	if got := exampleCommand(ipc.SessionInfo{Shell: ""}); got != "echo hi" {
		t.Errorf("unknown shell should get a portable example, got %q", got)
	}
}

// it_list's hint had its own hard-coded POSIX example, so a Windows session
// was told to run pwd no matter what exampleCommand said.
func TestListHintUsesTheSessionsOwnSyntax(t *testing.T) {
	sessions := []ipc.SessionInfo{{ID: "t-aaa111", Running: true, Shell: "Command Prompt"}}
	_, body, err := renderList(ipc.ListResult{Sessions: sessions}, 1, 4000, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"pwd"`) {
		t.Errorf("pwd is not a cmd command:\n%s", body)
	}
	if !strings.Contains(body, `"dir"`) {
		t.Errorf("a cmd session should be offered dir:\n%s", body)
	}
}
