package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

// TestOverlayAt covers the floating-overlay splicer that the action
// menu uses. The contract: same line count and per-line cell width as
// the base, with panel content substituted at the (col, row) corner.
func TestOverlayAt(t *testing.T) {
	t.Run("plain narrower panel splices into base", func(t *testing.T) {
		base := strings.Repeat("X", 20) + "\n" + strings.Repeat("Y", 20) + "\n" + strings.Repeat("Z", 20)
		panel := "[ABC]\n[DEF]"
		got := overlayAt(base, panel, 5, 1)
		// Row 0 untouched.
		lines := strings.Split(got, "\n")
		if lines[0] != strings.Repeat("X", 20) {
			t.Errorf("row 0 changed unexpectedly: %q", lines[0])
		}
		// Row 1 has panel at col 5: 5 Y's, [ABC], remaining Y's.
		// `overlayAt` brackets the panel with resets — the leading one
		// keeps the panel opaque to any SGR state open at the cut, the
		// trailing one stops the panel bleeding into the rest.
		wantRow1 := strings.Repeat("Y", 5) + "\x1b[0m" + "[ABC]" + "\x1b[0m" + strings.Repeat("Y", 10)
		if lines[1] != wantRow1 {
			t.Errorf("row 1: got %q, want %q", lines[1], wantRow1)
		}
		// Row 2 has panel at col 5: same shape for "[DEF]" + Z's.
		wantRow2 := strings.Repeat("Z", 5) + "\x1b[0m" + "[DEF]" + "\x1b[0m" + strings.Repeat("Z", 10)
		if lines[2] != wantRow2 {
			t.Errorf("row 2: got %q, want %q", lines[2], wantRow2)
		}
	})

	t.Run("panel rows past base are clipped", func(t *testing.T) {
		base := "AAA"
		panel := "X\nY\nZ"
		got := overlayAt(base, panel, 0, 1)
		// Base has one line; panel rows 1 and 2 land past EOF and are dropped.
		if got != base {
			t.Errorf("expected base unchanged for out-of-bounds panel rows, got %q", got)
		}
	})

	t.Run("empty inputs are no-ops", func(t *testing.T) {
		if got := overlayAt("", "X", 0, 0); got != "" {
			t.Errorf("empty base should return empty, got %q", got)
		}
		if got := overlayAt("AAA", "", 0, 0); got != "AAA" {
			t.Errorf("empty panel should return base unchanged, got %q", got)
		}
	})

	t.Run("ANSI styled base outside splice region is preserved", func(t *testing.T) {
		red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
		base := red.Render("REDREDRED") + "PLAIN" // 9 styled + 5 plain = 14 cells
		panel := "[X]"                            // 3 cells
		got := overlayAt(base, panel, 6, 0)
		// Visible width of result should match base.
		if w := lipgloss.Width(got); w != lipgloss.Width(base) {
			t.Errorf("width changed: got %d, want %d", w, lipgloss.Width(base))
		}
		// Panel marker must be present somewhere.
		if !strings.Contains(got, "[X]") {
			t.Errorf("panel not in output: %q", got)
		}
		// Red SGR should still be present (some red text remained left of panel).
		if !strings.Contains(got, "\x1b[") {
			t.Errorf("ANSI escapes stripped: %q", got)
		}
	})

	t.Run("panel past EOL pads with spaces", func(t *testing.T) {
		base := "ABC" // 3 cells
		panel := "X"  // 1 cell
		got := overlayAt(base, panel, 10, 0)
		// ABC, 7 spaces of padding, then the reset-bracketed panel.
		want := "ABC" + strings.Repeat(" ", 7) + "\x1b[0m" + "X" + "\x1b[0m"
		if got != want {
			t.Errorf("padding wrong: got %q, want %q", got, want)
		}
	})
}

// withColour forces a colour profile so styling assertions are
// deterministic — lipgloss strips all escapes when it can't detect a
// TTY, which silently turns colour tests into no-ops.
func withColour(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

// A spliced panel must be opaque: `left` carries whatever SGR state was
// open at the cut, and without a leading reset the panel's *unstyled*
// text inherits it. That's how dashboard log lines came out grey —
// they were spliced into a dim border frame and picked up its
// foreground.
func TestSpliceLineDoesNotLeakBaseColour(t *testing.T) {
	withColour(t)

	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	base := dim.Render("│" + strings.Repeat(" ", 20) + "│")

	out := spliceLine(base, "PLAIN", 1)

	i := strings.Index(out, "PLAIN")
	if i < 0 {
		t.Fatalf("panel text missing from %q", out)
	}
	if !strings.HasSuffix(out[:i], "\x1b[0m") {
		t.Errorf("panel text is not preceded by a reset, so it inherits the base colour.\n"+
			"prefix = %q\nfull   = %q", out[:i], out)
	}

	// The base's own trailing cells must still be dim — the reset
	// applies to the panel, not to the rest of the line.
	if !strings.Contains(out[i+len("PLAIN"):], "38;5;244") {
		t.Errorf("base lost its colour after the splice: %q", out[i+len("PLAIN"):])
	}
	// And the splice must not change the line's visible width.
	if got, want := lipgloss.Width(out), lipgloss.Width(base); got != want {
		t.Errorf("width = %d, want %d", got, want)
	}
}

// A styled panel keeps its own colours through the splice — the
// leading reset must not flatten the panel's styling.
func TestSpliceLinePreservesPanelStyle(t *testing.T) {
	withColour(t)

	base := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).
		Render("│" + strings.Repeat(" ", 20) + "│")
	panel := lipgloss.NewStyle().Foreground(lipgloss.Color("#4ade80")).Render("OK")

	out := spliceLine(base, panel, 2)
	if !strings.Contains(out, panel) {
		t.Errorf("panel did not survive the splice verbatim.\npanel = %q\nout   = %q", panel, out)
	}
}

// truncate measures terminal cells, not runes. A CJK ideograph or an
// emoji occupies two columns, so rune-counting let a 44-rune string
// claim 88 cells and overflow the column it was sized for.
func TestTruncateCountsCellsNotRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
	}{
		{"ascii long", strings.Repeat("a", 90), 20},
		{"cjk", strings.Repeat("世界", 40), 20},
		{"emoji", strings.Repeat("🔥", 30), 10},
		{"mixed", "pod-" + strings.Repeat("日本語", 20) + "-suffix", 24},
		{"cjk at 1 cell", strings.Repeat("世", 5), 1},
		{"cjk at 2 cells", strings.Repeat("世", 5), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if w := lipgloss.Width(got); w > tc.n {
				t.Errorf("truncate(_, %d) = %d cells (%q)", tc.n, w, got)
			}
		})
	}
}

// Content that already fits comes back untouched — no gratuitous
// ellipsis, and the wide-character path must not trip on it.
func TestTruncateLeavesFittingContentAlone(t *testing.T) {
	for _, s := range []string{"", "abc", "世界", "🔥", strings.Repeat("世", 10)} {
		n := lipgloss.Width(s)
		if n == 0 {
			n = 1
		}
		if got := truncate(s, n); got != s && lipgloss.Width(s) <= n {
			t.Errorf("truncate(%q, %d) = %q, want it unchanged", s, n, got)
		}
	}
	if got := truncate("anything", 0); got != "" {
		t.Errorf("truncate(_, 0) = %q, want empty", got)
	}
}

// truncateHead keeps the tail — the image tag at the end of a ref is
// the part worth preserving, so the cut and the "…" land at the front.
func TestTruncateHeadKeepsTail(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"fmidev/smartmet-server-backend:25.3.14-2", 20, "…r-backend:25.3.14-2"},
		{"fmidev/api:1.2", 20, "fmidev/api:1.2"},
		{"fmidev/api:1.2", 14, "fmidev/api:1.2"},
		{"abcdef", 3, "…ef"},
		{"anything", 0, ""},
	}
	for _, tc := range cases {
		if got := truncateHead(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateHead(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	for _, in := range []string{strings.Repeat("世界", 40), strings.Repeat("🔥", 30), "pod-" + strings.Repeat("日本語", 20)} {
		for _, n := range []int{1, 2, 10, 21} {
			if w := lipgloss.Width(truncateHead(in, n)); w > n {
				t.Errorf("truncateHead(_, %d) = %d cells", n, w)
			}
		}
	}
}

// padCol is the primitive every table cell goes through, so a
// rune-counting truncate made whole tables overflow their pane — 39
// cells when asked for 20.
func TestPadColExactWidthWithWideRunes(t *testing.T) {
	plain := lipgloss.NewStyle()
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	cases := []struct {
		name  string
		in    string
		width int
		style lipgloss.Style
	}{
		{"cjk plain", strings.Repeat("世界", 40), 20, plain},
		{"cjk styled", strings.Repeat("世界", 40), 20, red},
		{"emoji", strings.Repeat("🔥", 30), 10, plain},
		{"cjk short", "世界", 12, plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := lipgloss.Width(padCol(tc.in, tc.width, tc.style)); w != tc.width {
				t.Errorf("padCol = %d cells, want exactly %d", w, tc.width)
			}
			if w := lipgloss.Width(padColRight(tc.in, tc.width, tc.style)); w != tc.width {
				t.Errorf("padColRight = %d cells, want exactly %d", w, tc.width)
			}
		})
	}
}
