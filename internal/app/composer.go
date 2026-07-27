package app

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// composer is the multi-line input under a session's frame.
//
// A raw passthrough has nowhere to hold an unsubmitted line, so multi-line
// editing, paste-before-submit, and input history are impossible without a
// buffer of our own. The composer provides them for the common case of typing
// shell commands; raw mode exists for the programs that own the keyboard.
type composer struct {
	// lines holds the buffer split on newlines. There is always at least one.
	lines []string
	// row and column are the cursor position; column counts runes, not bytes.
	row, column int

	// scroll is the first visible line once the composer stops growing.
	scroll int
	// width is the usable interior width, and maxHeight the tallest the
	// composer may grow before it starts scrolling instead.
	width, maxHeight int

	history      []string
	historyIndex int
	// draft holds what was being typed before history recall started, so
	// walking back down restores it rather than losing it.
	draft string
}

func newComposer() *composer {
	return &composer{lines: []string{""}, width: 80, maxHeight: 6, historyIndex: -1}
}

func (c *composer) setSize(width, maxHeight int) {
	if width > 0 {
		c.width = width
	}
	if maxHeight > 0 {
		c.maxHeight = maxHeight
	}
	c.clampScroll()
}

// Value returns the buffer as one string.
func (c *composer) Value() string { return strings.Join(c.lines, "\n") }

// Empty reports whether nothing has been typed.
func (c *composer) Empty() bool { return len(c.lines) == 1 && c.lines[0] == "" }

// Multiline reports whether the buffer holds more than one line, which decides
// whether submission uses bracketed paste.
func (c *composer) Multiline() bool { return len(c.lines) > 1 }

// Reset clears the buffer and leaves history recall.
func (c *composer) Reset() {
	c.lines = []string{""}
	c.row, c.column, c.scroll = 0, 0, 0
	c.historyIndex, c.draft = -1, ""
}

// Remember records a submitted line for history recall.
func (c *composer) Remember(value string) {
	value = strings.TrimRight(value, "\n")
	if strings.TrimSpace(value) == "" {
		return
	}
	// Consecutive duplicates are noise; a person pressing up wants the
	// previous distinct command.
	if len(c.history) > 0 && c.history[len(c.history)-1] == value {
		return
	}
	c.history = append(c.history, value)
	const maxHistory = 500
	if len(c.history) > maxHistory {
		c.history = c.history[len(c.history)-maxHistory:]
	}
}

// Insert adds text at the cursor. Newlines in the text create real lines, so a
// paste lands as written rather than being flattened.
func (c *composer) Insert(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// A tab inside pasted text would render as a jump the cursor arithmetic
	// cannot follow, so it becomes spaces.
	text = strings.ReplaceAll(text, "\t", "    ")

	current := []rune(c.lines[c.row])
	before := string(current[:c.column])
	after := string(current[c.column:])

	pieces := strings.Split(text, "\n")
	if len(pieces) == 1 {
		c.lines[c.row] = before + pieces[0] + after
		c.column += utf8.RuneCountInString(pieces[0])
		c.clampScroll()
		return
	}

	rebuilt := make([]string, 0, len(c.lines)+len(pieces))
	rebuilt = append(rebuilt, c.lines[:c.row]...)
	rebuilt = append(rebuilt, before+pieces[0])
	rebuilt = append(rebuilt, pieces[1:len(pieces)-1]...)
	rebuilt = append(rebuilt, pieces[len(pieces)-1]+after)
	rebuilt = append(rebuilt, c.lines[c.row+1:]...)

	c.row += len(pieces) - 1
	c.column = utf8.RuneCountInString(pieces[len(pieces)-1])
	c.lines = rebuilt
	c.clampScroll()
}

// Newline splits the current line at the cursor.
func (c *composer) Newline() { c.Insert("\n") }

func (c *composer) backspace() {
	if c.column > 0 {
		current := []rune(c.lines[c.row])
		c.lines[c.row] = string(current[:c.column-1]) + string(current[c.column:])
		c.column--
		return
	}
	if c.row == 0 {
		return
	}
	previous := []rune(c.lines[c.row-1])
	c.column = len(previous)
	c.lines[c.row-1] = string(previous) + c.lines[c.row]
	c.lines = append(c.lines[:c.row], c.lines[c.row+1:]...)
	c.row--
	c.clampScroll()
}

func (c *composer) deleteForward() {
	current := []rune(c.lines[c.row])
	if c.column < len(current) {
		c.lines[c.row] = string(current[:c.column]) + string(current[c.column+1:])
		return
	}
	if c.row+1 >= len(c.lines) {
		return
	}
	c.lines[c.row] = string(current) + c.lines[c.row+1]
	c.lines = append(c.lines[:c.row+1], c.lines[c.row+2:]...)
}

func (c *composer) moveLeft() {
	if c.column > 0 {
		c.column--
		return
	}
	if c.row > 0 {
		c.row--
		c.column = utf8.RuneCountInString(c.lines[c.row])
		c.clampScroll()
	}
}

func (c *composer) moveRight() {
	if c.column < utf8.RuneCountInString(c.lines[c.row]) {
		c.column++
		return
	}
	if c.row+1 < len(c.lines) {
		c.row++
		c.column = 0
		c.clampScroll()
	}
}

// moveUp moves the cursor up one line, reporting false when it was already on
// the first line so the caller can treat the key as history recall instead.
func (c *composer) moveUp() bool {
	if c.row == 0 {
		return false
	}
	c.row--
	c.clampColumn()
	c.clampScroll()
	return true
}

func (c *composer) moveDown() bool {
	if c.row+1 >= len(c.lines) {
		return false
	}
	c.row++
	c.clampColumn()
	c.clampScroll()
	return true
}

// page moves the cursor by a viewport, which is how a long paste is navigated
// once the composer has stopped growing and started scrolling.
func (c *composer) page(direction int) {
	height := c.visibleHeight()
	c.row += direction * height
	if c.row < 0 {
		c.row = 0
	}
	if c.row >= len(c.lines) {
		c.row = len(c.lines) - 1
	}
	c.clampColumn()
	c.clampScroll()
}

func (c *composer) toStart() {
	c.row, c.column, c.scroll = 0, 0, 0
}

func (c *composer) toEnd() {
	c.row = len(c.lines) - 1
	c.column = utf8.RuneCountInString(c.lines[c.row])
	c.clampScroll()
}

func (c *composer) clampColumn() {
	if length := utf8.RuneCountInString(c.lines[c.row]); c.column > length {
		c.column = length
	}
}

// visibleHeight is how many lines the composer currently draws: it grows with
// the content up to maxHeight, then stops and scrolls.
func (c *composer) visibleHeight() int {
	height := len(c.lines)
	if height > c.maxHeight {
		height = c.maxHeight
	}
	if height < 1 {
		height = 1
	}
	return height
}

func (c *composer) clampScroll() {
	height := c.visibleHeight()
	if c.row < c.scroll {
		c.scroll = c.row
	}
	if c.row >= c.scroll+height {
		c.scroll = c.row - height + 1
	}
	maximum := len(c.lines) - height
	if maximum < 0 {
		maximum = 0
	}
	if c.scroll > maximum {
		c.scroll = maximum
	}
	if c.scroll < 0 {
		c.scroll = 0
	}
}

// recallPrevious walks back through history. It returns false when there is
// nothing older, so the caller can leave the key alone.
func (c *composer) recallPrevious() bool {
	if len(c.history) == 0 {
		return false
	}
	if c.historyIndex == -1 {
		c.draft = c.Value()
		c.historyIndex = len(c.history)
	}
	if c.historyIndex == 0 {
		return false
	}
	c.historyIndex--
	c.load(c.history[c.historyIndex])
	return true
}

// recallNext walks forward through history, ending on the draft that was
// interrupted rather than on an empty buffer.
func (c *composer) recallNext() bool {
	if c.historyIndex == -1 {
		return false
	}
	c.historyIndex++
	if c.historyIndex >= len(c.history) {
		c.historyIndex = -1
		c.load(c.draft)
		c.draft = ""
		return true
	}
	c.load(c.history[c.historyIndex])
	return true
}

func (c *composer) load(value string) {
	c.lines = strings.Split(value, "\n")
	if len(c.lines) == 0 {
		c.lines = []string{""}
	}
	c.toEnd()
}

// update applies one key press, reporting whether it was consumed.
func (c *composer) update(message tea.KeyMsg) bool {
	switch message.Type {
	case tea.KeyRunes:
		c.Insert(string(message.Runes))
		return true
	case tea.KeySpace:
		c.Insert(" ")
		return true
	case tea.KeyBackspace:
		c.backspace()
		return true
	case tea.KeyDelete:
		c.deleteForward()
		return true
	case tea.KeyLeft:
		c.moveLeft()
		return true
	case tea.KeyRight:
		c.moveRight()
		return true
	case tea.KeyHome:
		c.column = 0
		return true
	case tea.KeyEnd:
		c.column = utf8.RuneCountInString(c.lines[c.row])
		return true
	case tea.KeyCtrlA:
		c.column = 0
		return true
	case tea.KeyCtrlE:
		c.column = utf8.RuneCountInString(c.lines[c.row])
		return true
	case tea.KeyCtrlU:
		// Clear to start of line, as a shell does.
		current := []rune(c.lines[c.row])
		c.lines[c.row] = string(current[c.column:])
		c.column = 0
		return true
	case tea.KeyCtrlK:
		current := []rune(c.lines[c.row])
		c.lines[c.row] = string(current[:c.column])
		return true
	case tea.KeyCtrlW:
		c.deleteWord()
		return true
	}
	return false
}

func (c *composer) deleteWord() {
	current := []rune(c.lines[c.row])
	index := c.column
	for index > 0 && current[index-1] == ' ' {
		index--
	}
	for index > 0 && current[index-1] != ' ' {
		index--
	}
	c.lines[c.row] = string(current[:index]) + string(current[c.column:])
	c.column = index
}

// view renders the composer, including the scroll indicator that appears once
// the buffer is taller than the space available.
func (c *composer) view(prompt string, focused bool) string {
	height := c.visibleHeight()
	c.clampScroll()

	var out strings.Builder
	for offset := range height {
		index := c.scroll + offset
		if index >= len(c.lines) {
			break
		}
		marker := "  "
		if offset == 0 {
			marker = prompt
		}
		line := c.lines[index]
		if focused && index == c.row {
			line = withCursor(line, c.column)
		}
		out.WriteString(styleCursor.Render(marker))
		out.WriteString(truncate(line, c.width))
		if offset < height-1 {
			out.WriteByte('\n')
		}
	}

	// The indicator only appears when content is actually hidden, so a short
	// command is never decorated with numbers that mean nothing.
	if len(c.lines) > height {
		out.WriteString(styleDim.Render(fmt.Sprintf("  [%d/%d]", c.row+1, len(c.lines))))
	}
	return out.String()
}

// withCursor draws a block cursor at the given rune offset.
func withCursor(line string, column int) string {
	runes := []rune(line)
	if column >= len(runes) {
		return line + styleSelect.Render(" ")
	}
	return string(runes[:column]) + styleSelect.Render(string(runes[column])) + string(runes[column+1:])
}

// truncate cuts a rendered string to a display width, measuring cells rather
// than bytes so wide characters and escape sequences are handled correctly.
func truncate(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(text) <= width {
		return text
	}
	return ansi.Truncate(text, width, "…")
}
