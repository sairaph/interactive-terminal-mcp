package budget

import (
	"strings"
	"testing"
)

func TestCountIsStable(t *testing.T) {
	count, err := Count("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("Count: got %d, want 2", count)
	}
}

// Truncation direction is the whole point: it_tail must keep the newest lines
// and it_head the oldest, so the caller always gets the end it asked for.
func TestFitLinesKeepsTheRequestedEnd(t *testing.T) {
	lines := []string{"first", "second", "third", "fourth", "fifth"}

	kept, omitted, err := FitLines(lines, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 || kept[len(kept)-1] != "fifth" {
		t.Errorf("fromEnd should keep the newest lines, got %v", kept)
	}
	if omitted != len(lines)-len(kept) {
		t.Errorf("omitted: got %d, want %d", omitted, len(lines)-len(kept))
	}

	kept, omitted, err = FitLines(lines, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 || kept[0] != "first" {
		t.Errorf("fromStart should keep the oldest lines, got %v", kept)
	}
	if omitted != len(lines)-len(kept) {
		t.Errorf("omitted: got %d, want %d", omitted, len(lines)-len(kept))
	}
}

func TestFitLinesReturnsEverythingUnderBudget(t *testing.T) {
	lines := []string{"a", "b", "c"}
	kept, omitted, err := FitLines(lines, 10_000, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(lines) || omitted != 0 {
		t.Errorf("got %d lines and %d omitted, want all of them", len(kept), omitted)
	}
}

// Returning nothing would be a worse answer than returning one oversized line,
// so a single line larger than the budget still comes back.
func TestFitLinesAlwaysReturnsAtLeastOneLine(t *testing.T) {
	huge := strings.Repeat("word ", 5_000)
	kept, omitted, err := FitLines([]string{huge, "second"}, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("got %d lines, want exactly 1", len(kept))
	}
	if kept[0] != "second" {
		t.Errorf("fromEnd should have kept the last line")
	}
	if omitted != 1 {
		t.Errorf("omitted: got %d, want 1", omitted)
	}
}

func TestFitLinesHandlesEmptyInput(t *testing.T) {
	kept, omitted, err := FitLines(nil, 100, true)
	if err != nil || len(kept) != 0 || omitted != 0 {
		t.Errorf("empty input should be a no-op, got %v %d %v", kept, omitted, err)
	}
}

func TestTruncateRespectsBothLimits(t *testing.T) {
	text := strings.Repeat("alpha beta ", 500)

	prefix, tokens, truncated, err := Truncate(text, 50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || tokens > 50 {
		t.Errorf("token limit not honoured: %d tokens, truncated=%v", tokens, truncated)
	}
	if !strings.HasPrefix(text, prefix) {
		t.Error("the result must be a prefix of the input")
	}

	prefix, _, truncated, err = Truncate(text, 1_000_000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(prefix) > 100 {
		t.Errorf("byte limit not honoured: %d bytes, truncated=%v", len(prefix), truncated)
	}
}

func TestTruncateLeavesShortTextAlone(t *testing.T) {
	prefix, _, truncated, err := Truncate("short", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || prefix != "short" {
		t.Errorf("short text should pass through, got %q truncated=%v", prefix, truncated)
	}
}

// Pagination must pack whole records: a caller reading page 2 should never see
// half of a record it already saw on page 1.
func TestPaginateKeepsRecordsWhole(t *testing.T) {
	records := make([]string, 40)
	for index := range records {
		records[index] = strings.Repeat("record ", 10)
	}
	render := func(items []string) (string, error) { return strings.Join(items, "\n"), nil }

	seen := 0
	var totalPages int
	for page := 1; ; page++ {
		window, pages, err := Paginate(records, page, 100, render)
		if err != nil {
			t.Fatal(err)
		}
		totalPages = pages
		if len(window) == 0 {
			break
		}
		seen += len(window)
		if page > pages {
			t.Fatal("paginate returned records beyond the last page")
		}
	}
	if totalPages < 2 {
		t.Fatalf("a 100-token budget should need several pages, got %d", totalPages)
	}
	if seen != len(records) {
		t.Errorf("pagination lost records: saw %d of %d", seen, len(records))
	}
}

// A record too large for the budget must still be delivered rather than
// silently skipped, or the caller would never see it at all.
func TestPaginateDeliversOversizedRecords(t *testing.T) {
	records := []string{"small", strings.Repeat("huge ", 2_000), "small"}
	render := func(items []string) (string, error) { return strings.Join(items, "\n"), nil }

	seen := 0
	for page := 1; page <= 10; page++ {
		window, pages, err := Paginate(records, page, 20, render)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(window)
		if page >= pages {
			break
		}
	}
	if seen != len(records) {
		t.Errorf("every record must be delivered, saw %d of %d", seen, len(records))
	}
}

func TestPaginateHandlesEmptyInput(t *testing.T) {
	window, pages, err := Paginate(nil, 1, 100, func(items []string) (string, error) { return "", nil })
	if err != nil || len(window) != 0 || pages != 0 {
		t.Errorf("empty input should be a no-op, got %v %d %v", window, pages, err)
	}
}
