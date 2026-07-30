// Package keys implements the key-chord language accepted by it_send's `keys`
// argument, and encodes parsed chords into the byte sequences a terminal
// program expects.
//
// Encoding is mode-aware. A terminal does not have one fixed byte sequence per
// key: when a program enables application cursor keys (DECCKM), the arrows and
// Home/End switch from CSI to SS3 encoding. Honouring that is the difference
// between arrow keys working inside vim, less, and htop and working only at a
// shell prompt.
package keys

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sairaph/interactive-terminal-mcp/internal/vterm"
)

// Modifier is a bitmask of the modifiers applied to one chord.
type Modifier uint8

// Modifier values.
const (
	ModShift Modifier = 1 << iota
	ModAlt
	ModCtrl
)

// Named identifies a non-printable key.
type Named string

// Named keys. These are the canonical spellings; Aliases maps the accepted
// alternatives onto them.
const (
	KeyEnter     Named = "ENTER"
	KeyTab       Named = "TAB"
	KeyEscape    Named = "ESC"
	KeySpace     Named = "SPACE"
	KeyBackspace Named = "BACKSPACE"
	KeyDelete    Named = "DELETE"
	KeyInsert    Named = "INSERT"
	KeyHome      Named = "HOME"
	KeyEnd       Named = "END"
	KeyPageUp    Named = "PAGE_UP"
	KeyPageDown  Named = "PAGE_DOWN"
	KeyUp        Named = "UP"
	KeyDown      Named = "DOWN"
	KeyLeft      Named = "LEFT"
	KeyRight     Named = "RIGHT"
)

// Aliases are the alternative spellings accepted for named keys. They exist
// because an agent will reasonably write RETURN, PGUP, or ESCAPE, and failing
// on a synonym wastes a whole tool call.
var Aliases = map[string]Named{
	"ENTER": KeyEnter, "RETURN": KeyEnter, "CR": KeyEnter, "NEWLINE": KeyEnter,
	"TAB": KeyTab,
	"ESC": KeyEscape, "ESCAPE": KeyEscape,
	"SPACE": KeySpace, "SPACEBAR": KeySpace,
	"BACKSPACE": KeyBackspace, "BS": KeyBackspace,
	"DELETE": KeyDelete, "DEL": KeyDelete,
	"INSERT": KeyInsert, "INS": KeyInsert,
	"HOME":    KeyHome,
	"END":     KeyEnd,
	"PAGE_UP": KeyPageUp, "PAGEUP": KeyPageUp, "PGUP": KeyPageUp, "PRIOR": KeyPageUp,
	"PAGE_DOWN": KeyPageDown, "PAGEDOWN": KeyPageDown, "PGDN": KeyPageDown, "NEXT": KeyPageDown,
	"UP": KeyUp, "ARROW_UP": KeyUp,
	"DOWN": KeyDown, "ARROW_DOWN": KeyDown,
	"LEFT": KeyLeft, "ARROW_LEFT": KeyLeft,
	"RIGHT": KeyRight, "ARROW_RIGHT": KeyRight,
}

// Kind distinguishes the three chord shapes.
type Kind int

// Chord kinds.
const (
	// KindNamed is a named key, optionally with modifiers.
	KindNamed Kind = iota
	// KindRune is a single printable character, optionally with modifiers.
	KindRune
	// KindLiteral is a double-quoted run typed verbatim.
	KindLiteral
)

// Chord is one parsed element of a key sequence.
type Chord struct {
	Kind     Kind
	Named    Named
	Function int // 1-20 for F1-F20; 0 otherwise
	Rune     rune
	Literal  string
	Modifier Modifier
	Repeat   int
	// Source is the token as written, used in error messages so the agent sees
	// its own spelling rather than a normalized form.
	Source string
}

// Parse converts a key-sequence string into chords.
//
// Parsing is total: an invalid chord anywhere fails the whole call and nothing
// is sent. A partially applied key sequence would leave the terminal in a state
// neither the agent nor the user can predict.
func Parse(input string) ([]Chord, error) {
	tokens, err := split(input)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("keys is empty; %s", Syntax())
	}
	chords := make([]Chord, 0, len(tokens))
	for _, token := range tokens {
		chord, err := parseChord(token)
		if err != nil {
			return nil, err
		}
		chords = append(chords, chord)
	}
	return chords, nil
}

// split breaks the input on semicolons that are not inside a quoted literal.
func split(input string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	quoted, escaped := false, false

	for index, r := range input {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case quoted && r == '\\':
			current.WriteRune(r)
			escaped = true
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ';' && !quoted:
			if token := strings.TrimSpace(current.String()); token != "" {
				tokens = append(tokens, token)
			}
			current.Reset()
		default:
			_ = index
			current.WriteRune(r)
		}
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quoted literal in keys; every %q must be closed", `"`)
	}
	if token := strings.TrimSpace(current.String()); token != "" {
		tokens = append(tokens, token)
	}
	return tokens, nil
}

func parseChord(token string) (Chord, error) {
	chord := Chord{Repeat: 1, Source: token}

	// A repeat suffix applies to the whole chord: CTRL+B*3, DOWN*5, "hi"*2.
	body := token
	if index := strings.LastIndexByte(token, '*'); index > 0 && !insideQuotes(token, index) {
		count, err := strconv.Atoi(strings.TrimSpace(token[index+1:]))
		if err != nil {
			return chord, fmt.Errorf("invalid repeat count in %q: %s is not a number; %s", token, strings.TrimSpace(token[index+1:]), Syntax())
		}
		if count < 1 || count > 1000 {
			return chord, fmt.Errorf("invalid repeat count in %q: %d is outside 1-1000", token, count)
		}
		chord.Repeat = count
		body = strings.TrimSpace(token[:index])
	}
	if body == "" {
		return chord, fmt.Errorf("empty chord in %q; %s", token, Syntax())
	}

	// A quoted literal takes the rest of the chord verbatim.
	if strings.HasPrefix(body, `"`) {
		if len(body) < 2 || !strings.HasSuffix(body, `"`) {
			return chord, fmt.Errorf("unterminated quoted literal %q; every %q must be closed", body, `"`)
		}
		text, err := unquote(body)
		if err != nil {
			return chord, fmt.Errorf("invalid quoted literal %q: %w", body, err)
		}
		chord.Kind = KindLiteral
		chord.Literal = text
		return chord, nil
	}

	// Modifiers, then the key itself. A '+' that is the key (CTRL++) is kept.
	for {
		index := strings.IndexByte(body, '+')
		if index <= 0 {
			break
		}
		prefix := strings.ToUpper(strings.TrimSpace(body[:index]))
		modifier, ok := modifierNamed(prefix)
		if !ok {
			break
		}
		// "CTRL+" names a modifier with nothing to apply it to. Reporting that
		// beats treating the token as the literal text "CTRL+".
		if index == len(body)-1 {
			return chord, fmt.Errorf("chord %q has modifiers but no key; %s", token, Syntax())
		}
		chord.Modifier |= modifier
		body = strings.TrimSpace(body[index+1:])
	}
	if body == "" {
		return chord, fmt.Errorf("chord %q has modifiers but no key; %s", token, Syntax())
	}

	upper := strings.ToUpper(body)
	if named, ok := Aliases[upper]; ok {
		chord.Kind = KindNamed
		chord.Named = named
		return chord, nil
	}
	if strings.HasPrefix(upper, "F") && len(upper) > 1 {
		if number, err := strconv.Atoi(upper[1:]); err == nil {
			if number < 1 || number > 20 {
				return chord, fmt.Errorf("unknown function key %q: only F1-F20 exist", body)
			}
			chord.Kind = KindNamed
			chord.Function = number
			return chord, nil
		}
	}
	if count := utf8.RuneCountInString(body); count == 1 {
		r, _ := utf8.DecodeRuneInString(body)
		chord.Kind = KindRune
		chord.Rune = r
		return chord, nil
	}

	// A multi-character run is typed verbatim only when it could not have been
	// meant as a key name. A run of only letters, digits, and underscores looks
	// exactly like a key name, so "NOPE" or "PAGEUPP" is reported as a typo
	// rather than silently typed into the terminal. Anything else -- ":wq",
	// "git status", "--force" -- is unambiguous and is typed as written.
	if looksLikeKeyName(body) {
		return chord, fmt.Errorf("unknown key %q in chord %q; use one of %s, or quote it as %q to type it literally", body, token, strings.Join(NamedKeys(), ", "), body)
	}
	if suggestion, ok := modifierNotation(body); ok {
		return chord, fmt.Errorf("chord %q is a modifier written the way tmux, emacs, and screen write one; here it is %s, or quote it as %q to type those characters literally", token, suggestion, body)
	}
	if chord.Modifier != 0 {
		return chord, fmt.Errorf("modifiers cannot be applied to the multi-character run %q in chord %q; apply them to a single key instead", body, token)
	}
	chord.Kind = KindLiteral
	chord.Literal = body
	return chord, nil
}

// looksLikeKeyName reports whether a token is shaped like a key name, and so
// should be rejected as a typo rather than typed literally.
func looksLikeKeyName(body string) bool {
	for _, r := range body {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

func insideQuotes(token string, position int) bool {
	quoted := false
	for index, r := range token {
		if index >= position {
			break
		}
		if r == '"' {
			quoted = !quoted
		}
	}
	return quoted
}

func unquote(body string) (string, error) {
	inner := body[1 : len(body)-1]
	var out strings.Builder
	escaped := false
	for _, r := range inner {
		if escaped {
			switch r {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case '\\':
				out.WriteByte('\\')
			case '"':
				out.WriteByte('"')
			default:
				return "", fmt.Errorf(`unknown escape \%c; \n \r \t \\ and \" are supported`, r)
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		out.WriteRune(r)
	}
	if escaped {
		return "", fmt.Errorf(`trailing backslash`)
	}
	return out.String(), nil
}

func modifierNamed(name string) (Modifier, bool) {
	switch name {
	case "CTRL", "CONTROL", "C":
		return ModCtrl, true
	case "ALT", "META", "OPT", "OPTION", "M":
		return ModAlt, true
	case "SHIFT", "S":
		return ModShift, true
	default:
		return 0, false
	}
}

// Syntax is the one-line grammar reminder attached to every parse error, so a
// failed call tells the agent how to fix it without another round trip.
func Syntax() string {
	return `expected chords separated by ";" such as "CTRL+C", "ESC; :wq; ENTER", "DOWN*5", or 'i; "text"; ESC'`
}

// NamedKeys lists every canonical named key, for tool descriptions and errors.
func NamedKeys() []string {
	seen := map[Named]bool{}
	var names []string
	for _, named := range Aliases {
		if !seen[named] {
			seen[named] = true
			names = append(names, string(named))
		}
	}
	sort.Strings(names)
	return names
}

// Encode renders chords as the bytes a program expects, honouring the modes
// the program has actually set.
func Encode(chords []Chord, modes vterm.Modes) []byte {
	var out []byte
	for _, chord := range chords {
		encoded := encodeOne(chord, modes)
		for range chord.Repeat {
			out = append(out, encoded...)
		}
	}
	return out
}

func encodeOne(chord Chord, modes vterm.Modes) []byte {
	switch chord.Kind {
	case KindLiteral:
		return []byte(chord.Literal)
	case KindRune:
		return encodeRune(chord.Rune, chord.Modifier)
	default:
		return encodeNamed(chord, modes)
	}
}

func encodeRune(r rune, modifier Modifier) []byte {
	var out []byte
	if modifier&ModAlt != 0 {
		out = append(out, 0x1b)
	}
	if modifier&ModCtrl != 0 {
		if control, ok := controlByte(r); ok {
			return append(out, control)
		}
		// No control code exists for this character; sending the plain
		// character is closer to what a real terminal does than dropping it.
	}
	if modifier&ModShift != 0 {
		r = unicode.ToUpper(r)
	}
	return utf8.AppendRune(out, r)
}

// controlByte maps a character to its control code using xterm's conventions.
func controlByte(r rune) (byte, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r-'a') + 1, true
	case r >= 'A' && r <= 'Z':
		return byte(r-'A') + 1, true
	case r == ' ', r == '@':
		return 0x00, true
	case r == '[':
		return 0x1b, true
	case r == '\\':
		return 0x1c, true
	case r == ']':
		return 0x1d, true
	case r == '^':
		return 0x1e, true
	case r == '_', r == '?':
		return 0x1f, true
	default:
		return 0, false
	}
}

// modifierParameter is xterm's encoding of modifiers as a CSI parameter:
// 1 + 1(shift) + 2(alt) + 4(ctrl).
func modifierParameter(modifier Modifier) int {
	parameter := 1
	if modifier&ModShift != 0 {
		parameter += 1
	}
	if modifier&ModAlt != 0 {
		parameter += 2
	}
	if modifier&ModCtrl != 0 {
		parameter += 4
	}
	return parameter
}

func encodeNamed(chord Chord, modes vterm.Modes) []byte {
	modifier := chord.Modifier

	if chord.Function > 0 {
		return encodeFunction(chord.Function, modifier)
	}

	switch chord.Named {
	case KeyEnter:
		return withAlt(modifier, []byte{'\r'})
	case KeyTab:
		if modifier&ModShift != 0 {
			return []byte("\x1b[Z") // CSI Z is back-tab
		}
		if modifier&ModCtrl != 0 {
			return []byte{'\t'}
		}
		return withAlt(modifier, []byte{'\t'})
	case KeyEscape:
		return withAlt(modifier, []byte{0x1b})
	case KeySpace:
		if modifier&ModCtrl != 0 {
			return withAlt(modifier&^ModCtrl, []byte{0x00})
		}
		return withAlt(modifier, []byte{' '})
	case KeyBackspace:
		// DEL is what essentially every modern terminal sends; CTRL+Backspace
		// conventionally sends BS.
		if modifier&ModCtrl != 0 {
			return withAlt(modifier&^ModCtrl, []byte{0x08})
		}
		return withAlt(modifier, []byte{0x7f})
	case KeyDelete:
		return tilde(3, modifier)
	case KeyInsert:
		return tilde(2, modifier)
	case KeyPageUp:
		return tilde(5, modifier)
	case KeyPageDown:
		return tilde(6, modifier)
	case KeyUp:
		return cursor('A', modifier, modes.ApplicationCursor)
	case KeyDown:
		return cursor('B', modifier, modes.ApplicationCursor)
	case KeyRight:
		return cursor('C', modifier, modes.ApplicationCursor)
	case KeyLeft:
		return cursor('D', modifier, modes.ApplicationCursor)
	case KeyHome:
		return cursor('H', modifier, modes.ApplicationCursor)
	case KeyEnd:
		return cursor('F', modifier, modes.ApplicationCursor)
	default:
		return nil
	}
}

// cursor encodes an arrow, Home, or End. With no modifiers it follows the
// program's DECCKM setting: SS3 when application cursor keys are on, CSI
// otherwise. With modifiers, xterm always uses the CSI parameter form.
func cursor(final byte, modifier Modifier, application bool) []byte {
	if modifier != 0 {
		return fmt.Appendf(nil, "\x1b[1;%d%c", modifierParameter(modifier), final)
	}
	if application {
		return []byte{0x1b, 'O', final}
	}
	return []byte{0x1b, '[', final}
}

// tilde encodes the CSI <n> ~ family (Insert, Delete, Page Up, Page Down).
func tilde(number int, modifier Modifier) []byte {
	if modifier != 0 {
		return fmt.Appendf(nil, "\x1b[%d;%d~", number, modifierParameter(modifier))
	}
	return fmt.Appendf(nil, "\x1b[%d~", number)
}

func encodeFunction(number int, modifier Modifier) []byte {
	// F1-F4 use SS3 unmodified and the CSI 1;m P..S form when modified.
	if number <= 4 {
		final := byte('P' + number - 1)
		if modifier != 0 {
			return fmt.Appendf(nil, "\x1b[1;%d%c", modifierParameter(modifier), final)
		}
		return []byte{0x1b, 'O', final}
	}
	// F5-F20 use the CSI <n> ~ form with xterm's parameter numbers.
	numbers := map[int]int{
		5: 15, 6: 17, 7: 18, 8: 19, 9: 20, 10: 21, 11: 23, 12: 24,
		13: 25, 14: 26, 15: 28, 16: 29, 17: 31, 18: 32, 19: 33, 20: 34,
	}
	return tilde(numbers[number], modifier)
}

func withAlt(modifier Modifier, sequence []byte) []byte {
	if modifier&ModAlt == 0 {
		return sequence
	}
	return append([]byte{0x1b}, sequence...)
}

// Describe renders chords back as canonical source, for echoing what was sent.
func Describe(chords []Chord) string {
	parts := make([]string, 0, len(chords))
	for _, chord := range chords {
		parts = append(parts, chord.String())
	}
	return strings.Join(parts, "; ")
}

// String renders one chord in canonical form.
func (c Chord) String() string {
	var out strings.Builder
	if c.Modifier&ModCtrl != 0 {
		out.WriteString("CTRL+")
	}
	if c.Modifier&ModAlt != 0 {
		out.WriteString("ALT+")
	}
	if c.Modifier&ModShift != 0 {
		out.WriteString("SHIFT+")
	}
	switch {
	case c.Kind == KindLiteral:
		out.WriteString(strconv.Quote(c.Literal))
	case c.Kind == KindRune:
		out.WriteRune(c.Rune)
	case c.Function > 0:
		fmt.Fprintf(&out, "F%d", c.Function)
	default:
		out.WriteString(string(c.Named))
	}
	if c.Repeat > 1 {
		fmt.Fprintf(&out, "*%d", c.Repeat)
	}
	return out.String()
}

// Typed returns the characters a terminal would echo for these chords.
//
// Only unmodified printable input is included. A named key sends an escape
// sequence rather than the letters of its name, and a modified one sends a
// control byte, so neither can be mistaken for a result on screen; a plain
// literal or rune can, and that is what a later wait has to discount.
func Typed(chords []Chord) string {
	var out strings.Builder
	for _, c := range chords {
		if c.Modifier != 0 {
			continue
		}
		var piece string
		switch c.Kind {
		case KindLiteral:
			piece = c.Literal
		case KindRune:
			piece = string(c.Rune)
		default:
			continue
		}
		for i := 0; i < c.Repeat; i++ {
			out.WriteString(piece)
		}
	}
	return out.String()
}

// otherNotations maps the modifier prefixes other tools use to this one's
// spelling. Every one of these is something a person or an agent reaches for
// out of habit from tmux, emacs, screen, or a vim tutorial.
var otherNotations = map[string]string{
	"C": "CTRL", "CTRL": "CTRL", "CONTROL": "CTRL",
	"M": "ALT", "META": "ALT", "A": "ALT", "ALT": "ALT",
	"S": "SHIFT", "SHIFT": "SHIFT",
}

// modifierNotation recognises a modifier chord written in another tool's
// spelling and returns this tool's, so the caller is corrected rather than
// obeyed literally.
//
// Without this, "C-c" is punctuation as far as the parser is concerned, and a
// token that is not shaped like a key name is typed as written. The failure is
// silent and lands in whatever program is on screen: at a shell it types three
// stray characters, in vim it edits the file. The rule is deliberately narrow
// -- a known modifier prefix, then one character or a key this tool names --
// so that ":wq", "--force", "x-ray", and "git status" stay literal text, which
// is what a caller who wrote them meant.
func modifierNotation(body string) (string, bool) {
	if rest, ok := strings.CutPrefix(body, "^"); ok {
		if utf8.RuneCountInString(rest) == 1 {
			return "CTRL+" + strings.ToUpper(rest), true
		}
		return "", false
	}
	index := strings.IndexByte(body, '-')
	if index <= 0 {
		return "", false
	}
	canonical, ok := otherNotations[strings.ToUpper(body[:index])]
	if !ok {
		return "", false
	}
	rest := body[index+1:]
	if rest == "" {
		return "", false
	}
	upper := strings.ToUpper(rest)
	if _, named := Aliases[upper]; named || utf8.RuneCountInString(rest) == 1 {
		return canonical + "+" + upper, true
	}
	return "", false
}
