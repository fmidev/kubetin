package ui

import (
	"strings"

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
