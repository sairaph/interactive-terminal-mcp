package keys

import (
	"testing"

	"github.com/sairaph/interactive-terminal-mcp/internal/vterm"
)

func encode(t *testing.T, input string, modes vterm.Modes) string {
	t.Helper()
	chords, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse(%q): %v", input, err)
	}
	return string(Encode(chords, modes))
}

func TestEncodeControlAndNamedKeys(t *testing.T) {
	var normal vterm.Modes
	cases := []struct {
		input string
		want  string
	}{
		{"CTRL+C", "\x03"},
		{"CTRL+B", "\x02"},
		{"ctrl+b", "\x02"},
		{"CTRL+SPACE", "\x00"},
		{"CTRL+[", "\x1b"},
		{"CTRL+\\", "\x1c"},
		{"ENTER", "\r"},
		{"RETURN", "\r"},
		{"TAB", "\t"},
		{"SHIFT+TAB", "\x1b[Z"},
		{"ESC", "\x1b"},
		{"ESCAPE", "\x1b"},
		{"SPACE", " "},
		{"BACKSPACE", "\x7f"},
		{"CTRL+BACKSPACE", "\x08"},
		{"DELETE", "\x1b[3~"},
		{"INSERT", "\x1b[2~"},
		{"PAGE_UP", "\x1b[5~"},
		{"PGUP", "\x1b[5~"},
		{"PAGE_DOWN", "\x1b[6~"},
		{"PGDN", "\x1b[6~"},
		{"ALT+X", "\x1bX"},
		{"a", "a"},
		{"9", "9"},
		{"/", "/"},
	}
	for _, testCase := range cases {
		if got := encode(t, testCase.input, normal); got != testCase.want {
			t.Errorf("%s: got %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

// Arrow encoding must follow the program's DECCKM setting. This is what makes
// arrows work inside vim and less rather than only at a shell prompt.
func TestCursorKeysFollowApplicationMode(t *testing.T) {
	normal := vterm.Modes{}
	application := vterm.Modes{ApplicationCursor: true}

	cases := []struct {
		input                 string
		wantNormal, wantAppli string
	}{
		{"UP", "\x1b[A", "\x1bOA"},
		{"DOWN", "\x1b[B", "\x1bOB"},
		{"RIGHT", "\x1b[C", "\x1bOC"},
		{"LEFT", "\x1b[D", "\x1bOD"},
		{"HOME", "\x1b[H", "\x1bOH"},
		{"END", "\x1b[F", "\x1bOF"},
	}
	for _, testCase := range cases {
		if got := encode(t, testCase.input, normal); got != testCase.wantNormal {
			t.Errorf("%s normal: got %q, want %q", testCase.input, got, testCase.wantNormal)
		}
		if got := encode(t, testCase.input, application); got != testCase.wantAppli {
			t.Errorf("%s application: got %q, want %q", testCase.input, got, testCase.wantAppli)
		}
	}

	// A modified arrow always uses the CSI parameter form, in both modes.
	for _, modes := range []vterm.Modes{normal, application} {
		if got := encode(t, "CTRL+UP", modes); got != "\x1b[1;5A" {
			t.Errorf("CTRL+UP: got %q, want %q", got, "\x1b[1;5A")
		}
		if got := encode(t, "SHIFT+LEFT", modes); got != "\x1b[1;2D" {
			t.Errorf("SHIFT+LEFT: got %q, want %q", got, "\x1b[1;2D")
		}
	}
}

func TestFunctionKeys(t *testing.T) {
	var normal vterm.Modes
	cases := []struct{ input, want string }{
		{"F1", "\x1bOP"},
		{"F4", "\x1bOS"},
		{"F5", "\x1b[15~"},
		{"F12", "\x1b[24~"},
		{"F20", "\x1b[34~"},
		{"CTRL+F1", "\x1b[1;5P"},
	}
	for _, testCase := range cases {
		if got := encode(t, testCase.input, normal); got != testCase.want {
			t.Errorf("%s: got %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

func TestSequencesRepeatsAndLiterals(t *testing.T) {
	var normal vterm.Modes
	cases := []struct{ input, want string }{
		{"CTRL+B; PAGE_UP;", "\x02\x1b[5~"},
		{"ESC; :wq; ENTER", "\x1b:wq\r"},
		{"DOWN*5", "\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B"},
		{`i; "hello world"; ESC`, "ihello world\x1b"},
		{`"a;b"`, "a;b"},
		{`"tab\there"`, "tab\there"},
		{`"say \"hi\""`, `say "hi"`},
		{"CTRL+B*2", "\x02\x02"},
		{"  UP  ;  DOWN  ", "\x1b[A\x1b[B"},
		// Unquoted runs containing non-identifier characters are unambiguous
		// and are typed verbatim.
		{"git status; ENTER", "git status\r"},
		{"--force", "--force"},
	}
	for _, testCase := range cases {
		if got := encode(t, testCase.input, normal); got != testCase.want {
			t.Errorf("%s: got %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

// A quoted literal spanning a semicolon must not be split by the tokenizer,
// and an unbalanced quote must fail rather than silently truncating input.
func TestParseRejectsInvalidInput(t *testing.T) {
	cases := []string{
		"",
		"   ",
		`"unterminated`,
		"NOPE",
		"PAGEUPP",
		"CTRL+abc",
		"CTRL+",
		"DOWN*0",
		"DOWN*1001",
		"DOWN*abc",
		"F21",
		`"bad \q escape"`,
	}
	for _, input := range cases {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) should have failed", input)
		}
	}
}

// Parsing is total: one bad chord fails the whole sequence so nothing is sent.
func TestParseIsAllOrNothing(t *testing.T) {
	if _, err := Parse("UP; DOWN; NOT_A_KEY; ENTER"); err == nil {
		t.Fatal("a sequence with one invalid chord must fail entirely")
	}
}

// A failed parse must tell the agent how to fix it without another round trip:
// it names the offending token and offers the valid alternatives.
func TestParseErrorsAreActionable(t *testing.T) {
	typo, err := Parse("NOPE")
	_ = typo
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "NOPE") {
		t.Errorf("error should name the offending token, got %q", err)
	}
	if !contains(err.Error(), "PAGE_UP") {
		t.Errorf("error should list the valid key names, got %q", err)
	}

	_, err = Parse("CTRL+")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "CTRL+C") {
		t.Errorf("error should include the syntax reminder, got %q", err)
	}
}

func TestDescribeRoundTrip(t *testing.T) {
	chords, err := Parse("CTRL+B; PAGE_UP*3; \"hi\"")
	if err != nil {
		t.Fatal(err)
	}
	want := `CTRL+B; PAGE_UP*3; "hi"`
	if got := Describe(chords); got != want {
		t.Errorf("Describe: got %q, want %q", got, want)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
