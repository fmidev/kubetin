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
