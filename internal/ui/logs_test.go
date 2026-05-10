package ui

import (
	"strings"
	"testing"
)

// highlightMatches must wrap exactly the matched span and leave any
// pre-existing ANSI escape codes in the surrounding text intact.
// Reverse-on/off (`\x1b[7m`/`\x1b[27m`) is the only attribute we
// touch; the source line's foreground/background colours must keep
// rendering across the highlight boundary.
func TestHighlightMatches_PreservesEmbeddedANSI(t *testing.T) {
	// Log line with a coloured "world" — `\x1b[31m` red, `\x1b[0m` reset.
	line := "hello \x1b[31mworld\x1b[0m of clusters"
	out := highlightMatches(line, "world", false)

	// The red code before the match must still be present *before*
	// the highlight on; otherwise the keyword loses the line's colour.
	if !strings.Contains(out, "\x1b[31m") {
		t.Errorf("output dropped the line's red colour: %q", out)
	}
	// The match must be wrapped with reverse-on / reverse-off, not a
	// full reset (`\x1b[0m`). A full reset would terminate the red.
	if !strings.Contains(out, "\x1b[7m") {
		t.Errorf("output missing reverse-on: %q", out)
	}
	if !strings.Contains(out, "\x1b[27m") {
		t.Errorf("output missing reverse-off: %q", out)
	}
	// The keyword itself must appear inside the reverse pair.
	idxOn := strings.Index(out, "\x1b[7m")
	idxOff := strings.Index(out, "\x1b[27m")
	if idxOn < 0 || idxOff < 0 || idxOn >= idxOff {
		t.Fatalf("reverse markers not in order: %q", out)
	}
	span := out[idxOn:idxOff]
	if !strings.Contains(span, "world") {
		t.Errorf("highlighted span doesn't contain the keyword: %q", span)
	}
}

// Case-insensitive match: searching "ERROR" must find "Error" and
// "error" alike.
func TestHighlightMatches_CaseInsensitive(t *testing.T) {
	line := "Error: connection lost. error count = 3"
	out := highlightMatches(line, "ERROR", false)
	count := strings.Count(out, "\x1b[7m")
	if count != 2 {
		t.Errorf("expected 2 reverse-on markers, got %d in %q", count, out)
	}
}

// Empty needle or no match returns the original string unchanged.
func TestHighlightMatches_NoMatch(t *testing.T) {
	cases := []struct {
		name, line, needle string
	}{
		{"empty needle", "hello world", ""},
		{"no match", "hello world", "xyzzy"},
		{"empty line", "", "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out := highlightMatches(tc.line, tc.needle, false); out != tc.line {
				t.Errorf("expected unchanged %q, got %q", tc.line, out)
			}
		})
	}
}

// Bold variant uses `\x1b[1;7m` so the current n/N target stands out
// from other matches.
func TestHighlightMatches_BoldVariant(t *testing.T) {
	out := highlightMatches("foo bar baz", "bar", true)
	if !strings.Contains(out, "\x1b[1;7m") {
		t.Errorf("bold variant missing combined SGR: %q", out)
	}
	if !strings.Contains(out, "\x1b[27;22m") {
		t.Errorf("bold variant missing combined unset SGR: %q", out)
	}
}

// A match that falls between two ANSI escape sequences must still be
// found at the visible-text level. The escape itself must not count
// as part of the visible string.
func TestHighlightMatches_MatchAcrossANSI(t *testing.T) {
	// "match" split by an irrelevant escape inside it.
	line := "ma\x1b[1mtch ends"
	out := highlightMatches(line, "match", false)
	// Should still wrap something — the original "ma" + escape + "tch"
	// is "match" at visible level.
	if !strings.Contains(out, "\x1b[7m") || !strings.Contains(out, "\x1b[27m") {
		t.Errorf("expected highlight wrapping a split match: %q", out)
	}
}

// summariseStreamErr collapses common kubelet/apiserver stream errors
// to short labels and length-caps unfamiliar ones.
func TestSummariseStreamErr(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"connection reset", "read tcp 192.168.1.52:58707->192.168.1.40:16443: read: connection reset by peer", "connection reset (stream ended)"},
		{"i/o timeout", "read tcp ...: i/o timeout", "stream timed out"},
		{"EOF", "stream error EOF here", "stream closed (EOF)"},
		{"context canceled", "context canceled", "stream cancelled"},
		{"DNS", "dial tcp: lookup foo: no such host", "DNS lookup failed"},
		{"short unknown", "boom", "boom"},
		{"long unknown truncated", strings.Repeat("a", 100), strings.Repeat("a", 57) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseStreamErr(tc.in); got != tc.want {
				t.Errorf("summariseStreamErr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
