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

// renderSelected must keep reverse mode on across inner ANSI resets,
// otherwise the highlight visibly drops after the first coloured cell
// (the reported pod-row bug). Every `\x1b[0m` in the input must be
// followed by `\x1b[7m`, and lipgloss.Width() must be unchanged.
func TestRenderSelected_ReverseSurvivesInnerResets(t *testing.T) {
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	// Two coloured cells with plain text between/around them — the
	// shape a real pod row has after warnGlyph + padCol(phase) +
	// padCellANSI(dots).
	line := "ns  " + red.Render("Running") + "  " + red.Render("●") + "  rest"

	got := renderSelected(line)

	if !strings.HasPrefix(got, "\x1b[7m") {
		t.Fatalf("renderSelected must start with reverse-on, got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("renderSelected must end with full reset, got %q", got)
	}
	// Every inner `\x1b[0m` (cell terminator) must be immediately
	// followed by a fresh `\x1b[7m` — that's the whole point of the
	// helper. Count: inner resets in `line` is 2 (one per styled cell);
	// total resets in `got` is 3 (2 inner + 1 trailing); each inner
	// must have `\x1b[7m` right after.
	innerResets := strings.Count(line, "\x1b[0m")
	if got, want := strings.Count(got, "\x1b[0m\x1b[7m"), innerResets; got != want {
		t.Fatalf("inner resets re-armed with reverse: got %d, want %d", got, want)
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
	want := "\x1b[7m" + line + "\x1b[0m"
	if got != want {
		t.Fatalf("plain row wrap mismatch:\ngot=%q\nwant=%q", got, want)
	}
}
