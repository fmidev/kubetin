package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// padCol returns a cell of exactly `width` visible cells: the input
// truncated when too long, padded with spaces when too short.
// `style` is applied to the visible content; padding is plain spaces.
//
// Why this exists: Go's len() counts bytes, but a styled string
// includes ANSI escape sequences that aren't visible. Using `%-*s`
// pads by byte length, so styled cells always under-pad and the next
// column shifts to the left. Routing every cell through padCol means
// alignment stops being a per-callsite responsibility.
func padCol(text string, width int, style lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	t := truncate(text, width)
	rendered := style.Render(t)
	visible := lipgloss.Width(rendered)
	if visible >= width {
		return rendered
	}
	return rendered + strings.Repeat(" ", width-visible)
}

// padColRight is padCol but the content sits flush right with leading
// space padding. Useful for numeric columns (CPU, MEM, RESTARTS).
func padColRight(text string, width int, style lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	t := truncate(text, width)
	rendered := style.Render(t)
	visible := lipgloss.Width(rendered)
	if visible >= width {
		return rendered
	}
	return strings.Repeat(" ", width-visible) + rendered
}

// padCellANSI pads (or ANSI-aware truncates) already-styled content
// to exactly width visible cells, left-aligned. Use this when the
// caller has built a string that already contains ANSI escape codes
// — padCol's byte-level truncate slices through escape sequences and
// produces bleeding garbage. Body rows should keep using padCol with
// a single style; only mixed-style cells (e.g. a header label in
// one colour with a sort arrow in another) need this helper.
func padCellANSI(content string, width int) string {
	if width <= 0 {
		return ""
	}
	visible := lipgloss.Width(content)
	if visible > width {
		return lipgloss.NewStyle().MaxWidth(width).Render(content)
	}
	return content + strings.Repeat(" ", width-visible)
}

// padCellANSIRight is padCellANSI right-aligned.
func padCellANSIRight(content string, width int) string {
	if width <= 0 {
		return ""
	}
	visible := lipgloss.Width(content)
	if visible > width {
		return lipgloss.NewStyle().MaxWidth(width).Render(content)
	}
	return strings.Repeat(" ", width-visible) + content
}

// renderSelected wraps a body row in the selection highlight: a
// uniform dark-grey background painted under the whole row, with
// per-cell foreground colours (Running/Pending/Failed phase, the
// container-state dots) preserved unchanged. Same look htop/btop use
// — the selected row is visually distinct without erasing the colour
// information the row was carrying.
//
// Implementation: every styled cell (padCol with a non-empty style,
// padCellANSI content) ends with `\x1b[0m`, which resets *all* SGR
// attributes — including the bg we just set. Splice a fresh bg-on
// after every inner reset so the highlight survives end-to-end.
//
// Why not Theme.Selected.Render(line): that wraps with the bg's SGR
// once at the start; the first inner reset clears it and the rest of
// the row renders unhighlighted (the original bug). And we want a
// background, not reverse — reverse video swaps fg/bg per cell, so
// the green container dot becomes black-on-green and the colour
// information is lost.
//
// SGR 48;2;58;58;58 == lipgloss.Color("#3a3a3a"). Keep in sync with
// Theme.Selected.
func renderSelected(line string) string {
	const (
		bgOn      = "\x1b[48;2;58;58;58m"
		fullReset = "\x1b[0m"
	)
	if !strings.Contains(line, fullReset) {
		// Plain row → cheap wrap, no inner resets to defend against.
		return bgOn + line + fullReset
	}
	return bgOn + strings.ReplaceAll(line, fullReset, fullReset+bgOn) + fullReset
}

// shortHost trims a host's domain suffix — "node-1.example.com" →
// "node-1". Used in the pod table's NODE column where FQDNs eat
// horizontal space without adding information the operator usually
// wants. Empty input returns empty so missing nodeNames stay missing.
func shortHost(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i]
	}
	return s
}

// warnGlyph returns a 1-cell prefix for a row: a coloured "⚠" if
// the (kind, ns, name) tuple has a recent Warning event in the
// pre-built index, otherwise a single blank space. Caller passes the
// theme so the glyph picks up the same yellow as the other status-
// warn surfaces (deploy not-ready ratio, paused log indicator, etc.).
func warnGlyph(idx map[string]struct{}, kind, ns, name string, th Theme) string {
	if _, ok := idx[warnKey(kind, ns, name)]; ok {
		return th.StatusWrn.Render("⚠")
	}
	return " "
}

// overlayAt composites panel onto base at column col / row row, in
// place — the result has the same visible dimensions as base. ANSI
// SGR escapes in base outside the splice region are preserved, so
// colour state of the surrounding table flows through cleanly. Panel
// itself is opaque; the user sees the *rest* of the table around it,
// not through it (terminals can't alpha-blend).
//
// Used by the floating action-menu overlay so the user can still see
// the row they came from while choosing what to do with it.
func overlayAt(base, panel string, col, row int) string {
	if panel == "" || base == "" {
		return base
	}
	if col < 0 {
		col = 0
	}
	baseLines := strings.Split(base, "\n")
	panelLines := strings.Split(panel, "\n")
	for i, pl := range panelLines {
		r := row + i
		if r < 0 || r >= len(baseLines) {
			continue
		}
		baseLines[r] = spliceLine(baseLines[r], pl, col)
	}
	return strings.Join(baseLines, "\n")
}

// spliceLine returns base with panel inserted at col, replacing
// `lipgloss.Width(panel)` cells under the panel. SGR state from base
// is restored after the panel so styles on the trailing portion of
// the table line carry through.
func spliceLine(base, panel string, col int) string {
	panelW := lipgloss.Width(panel)
	left, leftCells := visiblePrefix(base, col)
	if leftCells < col {
		left += strings.Repeat(" ", col-leftCells)
	}
	rest := visibleSuffix(base, col+panelW)
	// Explicit reset between panel and rest so panel's trailing colour
	// (if any) doesn't bleed into base. `rest` already carries the
	// accumulated SGR state from before the cut, so the table picks up
	// where it left off.
	return left + panel + "\x1b[0m" + rest
}

// visiblePrefix returns the prefix of s spanning at most n visible
// cells, and the actual visible-cell width of that prefix. SGR
// escapes are zero-width and pass through verbatim. A wide character
// that would straddle the cut is dropped (the caller pads to n if
// it cares).
func visiblePrefix(s string, n int) (string, int) {
	if n <= 0 {
		return "", 0
	}
	var b strings.Builder
	cells := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				b.WriteString(s[i : j+1])
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := lipgloss.Width(string(r))
		if cells+w > n {
			break
		}
		b.WriteRune(r)
		cells += w
		i += size
	}
	return b.String(), cells
}

// visibleSuffix returns the portion of s starting at visible-cell
// offset n, prefixed with the SGR state accumulated before the cut
// so the suffix renders with the colour base would have used at that
// point. Returns empty if s has ≤ n visible cells.
func visibleSuffix(s string, n int) string {
	var sgr strings.Builder
	cells := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				sgr.WriteString(s[i : j+1])
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := lipgloss.Width(string(r))
		if cells+w > n {
			return sgr.String() + s[i:]
		}
		cells += w
		i += size
	}
	return ""
}

// clampCanvas guarantees that s occupies exactly w cells wide and h
// rows tall — short content is padded with blanks, oversized content
// is truncated. Wrap every renderer that hands a body/overlay back to
// View() through this and the layout invariants downstream
// (JoinVertical not adding phantom rows from trailing newlines, the
// terminal not scrolling because total_height > m.height) just hold.
//
// Why this lives here rather than in each renderer: every renderer we
// have either over- or under-shoots height/width by a row or column,
// in subtly different ways (trailing newline, separator wider than
// inner box area, padding loop off-by-one). Centralising the clamp
// means the only thing each renderer has to get right is what to
// SHOW; the geometric contract is enforced once, on the way out.
func clampCanvas(s string, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return lipgloss.NewStyle().
		Width(w).MaxWidth(w).
		Height(h).MaxHeight(h).
		Render(s)
}
