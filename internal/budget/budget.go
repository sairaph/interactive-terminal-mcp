// Package budget provides deterministic o200k_base token accounting so tool
// output can be capped by tokens rather than by bytes or line counts.
package budget

import (
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer/codec"
)

var (
	codecOnce sync.Once
	encoder   *codec.Codec
)

func encoding() (*codec.Codec, error) {
	codecOnce.Do(func() { encoder = codec.NewO200kBase() })
	return encoder, nil
}

// Count returns the exact number of o200k_base tokens in text.
func Count(text string) (int, error) {
	enc, err := encoding()
	if err != nil {
		return 0, err
	}
	return enc.Count(text)
}

// FitLines returns the longest run of lines that fits tokenLimit.
//
// fromEnd selects which end survives: true keeps the newest lines and drops
// the oldest (it_tail), false keeps the oldest and drops the newest (it_head).
// A single line larger than the budget is still returned, because returning
// nothing would be a worse answer than returning one oversized line.
func FitLines(lines []string, tokenLimit int, fromEnd bool) (kept []string, omitted int, err error) {
	if len(lines) == 0 {
		return nil, 0, nil
	}
	if tokenLimit <= 0 {
		tokenLimit = 1
	}

	// Per-line costs, including the newline that joins them.
	costs := make([]int, len(lines))
	for i, line := range lines {
		n, err := Count(line + "\n")
		if err != nil {
			return nil, 0, err
		}
		costs[i] = n
	}

	used, count := 0, 0
	if fromEnd {
		for i := len(lines) - 1; i >= 0; i-- {
			if count > 0 && used+costs[i] > tokenLimit {
				break
			}
			used += costs[i]
			count++
		}
		return lines[len(lines)-count:], len(lines) - count, nil
	}
	for i := range lines {
		if count > 0 && used+costs[i] > tokenLimit {
			break
		}
		used += costs[i]
		count++
	}
	return lines[:count], len(lines) - count, nil
}

// Truncate returns a valid UTF-8 prefix bounded by both tokens and bytes.
// The returned token count describes the returned prefix, not either limit.
func Truncate(text string, tokenLimit, byteLimit int) (prefix string, tokens int, truncated bool, err error) {
	if !utf8.ValidString(text) {
		return "", 0, false, errors.New("token budget input is not valid UTF-8")
	}
	if tokenLimit <= 0 || byteLimit <= 0 {
		return "", 0, text != "", nil
	}

	bounded := text
	if len(bounded) > byteLimit {
		bounded = validPrefix(bounded, byteLimit)
		truncated = true
	}
	enc, err := encoding()
	if err != nil {
		return "", 0, false, err
	}

	probeLimit := 4 << 10
	if tokenLimit <= (int(^uint(0)>>1))/8 && tokenLimit*8 > probeLimit {
		probeLimit = tokenLimit * 8
	}
	probeLimit = min(probeLimit, len(bounded))
	candidate := validPrefix(bounded, probeLimit)
	var ids []uint
	var pieces []string
	for {
		ids, pieces, err = enc.Encode(candidate)
		if err != nil {
			return "", 0, false, err
		}
		if len(ids) > tokenLimit || len(candidate) == len(bounded) {
			break
		}
		next := min(len(bounded), max(len(candidate)+1, len(candidate)*2))
		candidate = validPrefix(bounded, next)
	}
	if len(ids) <= tokenLimit {
		return candidate, len(ids), truncated || len(candidate) != len(text), nil
	}

	truncated = true
	prefix = validPrefix(strings.Join(pieces[:tokenLimit], ""), len(candidate))
	tokens, err = enc.Count(prefix)
	if err != nil {
		return "", 0, false, err
	}
	for tokens > tokenLimit && prefix != "" {
		_, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
		tokens, err = enc.Count(prefix)
		if err != nil {
			return "", 0, false, err
		}
	}
	return prefix, tokens, true, nil
}

func validPrefix(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	prefix := text
	if limit < len(prefix) {
		prefix = prefix[:limit]
	}
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

// Paginate groups complete records into pages by the token count of
// render(records). A record that exceeds the budget by itself remains the sole
// record of its page rather than being dropped.
func Paginate[T any](records []T, requestedPage, tokenLimit int, render func([]T) (string, error)) ([]T, int, error) {
	if len(records) == 0 {
		return nil, 0, nil
	}
	starts := []int{0}
	start := 0
	for end := 1; end <= len(records); end++ {
		representation, err := render(records[start:end])
		if err != nil {
			return nil, 0, err
		}
		used, err := Count(representation)
		if err != nil {
			return nil, 0, err
		}
		if end-start > 1 && used > tokenLimit {
			start = end - 1
			starts = append(starts, start)
		}
	}

	totalPages := len(starts)
	if requestedPage < 1 || requestedPage > totalPages {
		return nil, totalPages, nil
	}
	start = starts[requestedPage-1]
	end := len(records)
	if requestedPage < totalPages {
		end = starts[requestedPage]
	}
	return append([]T(nil), records[start:end]...), totalPages, nil
}
