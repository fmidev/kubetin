package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// padCol must guarantee a consistent visible width regardless of
// whether the input is plain text or has been wrapped in a styled
// (ANSI-coloured) lipgloss render.
func TestPadCol_VisibleWidthInvariant(t *testing.T) {
	plain := lipgloss.NewStyle()
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	cases := []struct {
		name  string
		input string
		width int
		style lipgloss.Style
	}{
		{"plain short", "ok", 10, plain},
		{"plain exact", "exactnowex", 10, plain},
		{"plain truncate", "verylonglongtext", 10, plain},
		{"styled short", "ok", 10, red},
		{"styled truncate", "verylonglongtext", 10, red},
		{"empty", "", 8, plain},
		{"width zero", "anything", 0, plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := padCol(tc.input, tc.width, tc.style)
			vw := lipgloss.Width(got)
			if tc.width <= 0 {
				if got != "" {
					t.Fatalf("width<=0 should return empty, got %q", got)
				}
				return
			}
			if vw != tc.width {
				t.Fatalf("padCol(%q, %d, _): visible width = %d, want %d (got %q)",
					tc.input, tc.width, vw, tc.width, got)
			}
		})
	}
}

func TestPadColRight_VisibleWidthInvariant(t *testing.T) {
	plain := lipgloss.NewStyle()
	got := padColRight("42", 6, plain)
	vw := lipgloss.Width(got)
	if vw != 6 {
		t.Fatalf("padColRight visible width = %d, want 6", vw)
	}
	if !strings.HasPrefix(got, "    42") {
		t.Fatalf("padColRight should be right-justified, got %q", got)
	}
}

// bgOn is the SGR sequence renderSelected emits — kept here so a
// drift between the production code and the tests fails loudly
// rather than silently passing both sides on the wrong colour.
const selectedBgOn = "\x1b[48;2;58;58;58m"

// renderSelected must keep the background-on SGR alive across inner
// ANSI resets, otherwise the highlight visibly drops after the first
// coloured cell (the reported pod-row bug). Every `\x1b[0m` in the
// input must be followed by the bg-on sequence, and lipgloss.Width()
// must be unchanged.
func TestRenderSelected_BgSurvivesInnerResets(t *testing.T) {
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	// Two coloured cells with plain text between/around them — the
	// shape a real pod row has after warnGlyph + padCol(phase) +
	// padCellANSI(dots).
	line := "ns  " + red.Render("Running") + "  " + red.Render("●") + "  rest"

	got := renderSelected(line)

	if !strings.HasPrefix(got, selectedBgOn) {
		t.Fatalf("renderSelected must start with bg-on, got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("renderSelected must end with full reset, got %q", got)
	}
	// Every inner `\x1b[0m` (cell terminator) must be immediately
	// followed by a fresh bg-on. Count: inner resets in `line` is 2
	// (one per styled cell); each must have bg-on right after.
	innerResets := strings.Count(line, "\x1b[0m")
	if got, want := strings.Count(got, "\x1b[0m"+selectedBgOn), innerResets; got != want {
		t.Fatalf("inner resets re-armed with bg-on: got %d, want %d", got, want)
	}
	// Per-cell foregrounds must be preserved (the whole point of
	// using bg instead of reverse — the green dot stays green).
	if !strings.Contains(got, "Running") || !strings.Contains(got, "●") {
		t.Fatalf("cell content not preserved end-to-end: %q", got)
	}
	// No reverse-mode SGR anywhere — that's the prior approach and we
	// explicitly moved away from it.
	if strings.Contains(got, "\x1b[7m") {
		t.Fatalf("reverse-mode SGR leaked into output: %q", got)
	}
	// Visible width is preserved (no garbage glyphs, no widening).
	if lw, rw := lipgloss.Width(line), lipgloss.Width(got); lw != rw {
		t.Fatalf("visible width changed: line=%d, selected=%d", lw, rw)
	}
}

// Cheap path: plain input with no inner resets gets the simple wrap.
func TestRenderSelected_PlainRowCheapWrap(t *testing.T) {
	line := "namespace      name        Running   0  5m"
	got := renderSelected(line)
	want := selectedBgOn + line + "\x1b[0m"
	if got != want {
		t.Fatalf("plain row wrap mismatch:\ngot=%q\nwant=%q", got, want)
	}
}
