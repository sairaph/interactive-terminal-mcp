// Package render is the output-format contract shared by the MCP server and
// the human CLI: optional YAML frontmatter (strictly structured data, field
// order = struct declaration order) followed by an optional Markdown body.
// Code-like values that must appear in the body go inside tilde fences.
package render

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxBytes is the hard ceiling on one rendered document. It protects the MCP
// transport from pathological program output; the configured token budgets are
// much lower and do the real work.
const MaxBytes = 1 << 20

// Document is one rendered tool result.
type Document struct {
	Front   any
	Body    string
	IsError bool
}

// String renders the document. It fails rather than emitting a truncated
// document, so a caller never mistakes a cut-off result for a complete one.
func (d Document) String() (string, error) {
	var out strings.Builder
	if d.Front != nil {
		raw, err := yaml.Marshal(d.Front)
		if err != nil {
			return "", fmt.Errorf("render frontmatter: %w", err)
		}
		out.WriteString("---\n")
		out.Write(raw)
		out.WriteString("---\n")
		if d.Body != "" {
			out.WriteByte('\n')
		}
	}
	out.WriteString(d.Body)
	if d.Body != "" && !strings.HasSuffix(d.Body, "\n") {
		out.WriteByte('\n')
	}
	if out.Len() > MaxBytes {
		return "", fmt.Errorf("rendered output exceeds %d bytes", MaxBytes)
	}
	return out.String(), nil
}

// Fence wraps content in a tilde fence that grows past any run of tildes
// inside the content, so program output can never close it early.
func Fence(content, language string) string {
	longest := 2
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		n := 0
		for n < len(trimmed) && trimmed[n] == '~' {
			n++
		}
		if n > longest {
			longest = n
		}
	}
	fence := strings.Repeat("~", longest+1)
	var out strings.Builder
	out.WriteString(fence)
	out.WriteString(language)
	out.WriteByte('\n')
	out.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		out.WriteByte('\n')
	}
	out.WriteString(fence)
	return out.String()
}

// Screen renders captured terminal lines as a fenced block. An empty screen
// still produces a fence so the agent can see that the terminal is blank
// rather than guessing that output was dropped.
func Screen(lines []string) string {
	if len(lines) == 0 {
		return Fence("(the screen is blank)", "text")
	}
	return Fence(strings.Join(lines, "\n"), "text")
}
