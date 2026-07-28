package session

import "testing"

// The reported failure: a wait_for on a marker inside the submitted command
// line returned matched at 551ms with no output produced, because the echo was
// still being drawn. The screen held the marker but not yet the closing quote,
// so a rule that looks for the whole submitted line did not recognise the echo
// and counted it as a result.
func TestEchoRuleIgnoresAHalfDrawnEcho(t *testing.T) {
	typed := `1..6000 | % { $_ }; "BULKDONE"`
	rule := newEchoRule("BULKDONE", typed, 0)

	// Every prefix of the echo, drawn one character at a time. None of them is
	// a result: nothing has run yet.
	for length := 1; length <= len(typed); length++ {
		screen := "PS C:\\> " + typed[:length]
		if rule.matches(screen) {
			t.Fatalf("matched while the echo was still drawing, at %q", typed[:length])
		}
	}

	// The command finally prints it. That is the result.
	if !rule.matches("PS C:\\> " + typed + "BULKDONE") {
		t.Error("the command's own output should match")
	}
}

// A marker that is not part of the command line has no echo to discount, so it
// matches the moment it appears -- including in the same burst as the echo,
// which is what every fast command does.
func TestEchoRuleMatchesOutputThatArrivesWithTheEcho(t *testing.T) {
	rule := newEchoRule("BULK-DONE", "printf 'BULK%s\\n' -DONE", 0)
	if !rule.matches("$ printf 'BULK%s\\n' -DONE\nBULK-DONE") {
		t.Error("output printed immediately must still match")
	}
}

// Once a long command's output has scrolled the echo away, the occurrences it
// contributed are gone too, so the real marker has to match on its own.
func TestEchoRuleMatchesAfterTheEchoScrollsAway(t *testing.T) {
	rule := newEchoRule("BULKDONE", `1..6000 | % { $_ }; "BULKDONE"`, 0)
	if !rule.matches("5998\n5999\n6000\nBULKDONE") {
		t.Error("the marker should match once the echo has scrolled off")
	}
}

// Text already on screen before the input was typed is not a result of it.
func TestEchoRuleIgnoresTheBaseline(t *testing.T) {
	rule := newEchoRule("READY", "sleep 30", 1)
	if rule.matches("READY\n$ sleep 30") {
		t.Error("text that was already there is not a new result")
	}
	if !rule.matches("READY\n$ sleep 30\nREADY") {
		t.Error("a second occurrence is a result")
	}
}

// A wait with no input of its own is asking whether the text is on screen.
func TestEchoRuleWithNoInputMatchesWhatIsThere(t *testing.T) {
	rule := newEchoRule("ceiling-test", "", 0)
	if !rule.matches("$ echo ceiling-test\nceiling-test") {
		t.Error("with nothing typed, any occurrence answers the question")
	}
}
