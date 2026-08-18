package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Breakpoints for the dashboard's two shapes. Below either of these
// the wide frame's panes get too small to carry their own columns, so
// we fall back to the stacked column and let the canvas scroll.
const (
	dashWideMinWidth  = 120
	dashWideMinHeight = 20
)

// Minimum content rows for the two flexible regions of the wide frame.
// STATUS is fixed-height (it renders a known number of lines), so only
// the middle row and the log tail compete for what's left.
const (
	dashMidMinHeight  = 3
	dashLogsMinHeight = 3
)

// rect is a pane's interior region on the canvas, in cells. x/y are
// zero-based offsets from the canvas origin.
type rect struct{ x, y, w, h int }

// empty reports whether the rect has no drawable area. Pane renderers
// short-circuit on it rather than each guarding w/h separately.
func (r rect) empty() bool { return r.w < 1 || r.h < 1 }

// dashLayout is the resolved geometry for one render. In wide mode
// frame holds the pre-drawn border grid (canvas-sized) and the four
// rects address its interior cells. In stacked mode frame is empty and
// only the widths matter — panes are drawn as separate boxes and
// joined vertically by the caller.
type dashLayout struct {
	wide   bool
	status rect
	left   rect
	right  rect
	logs   rect
	frame  string
}

// dashLayoutFor resolves the pane geometry for a canvas of w×h cells.
// statusH is how many content rows the status banner needs — the
// caller computes it from the resource (a pod with a failing condition
// spends an extra line explaining it), so the layout math stays
// ignorant of what's being rendered.
//
// The wide frame is five horizontal bands:
//
//	top border · STATUS · separator · left|right · separator · LOGS · bottom
//
// which costs 4 rows of chrome, leaving h-4-statusH to split between
// the middle row and the log tail.
func dashLayoutFor(w, h, statusH int, th Theme) dashLayout {
	if w < dashWideMinWidth || h < dashWideMinHeight {
		return dashLayout{wide: false, status: rect{w: w}}
	}

	remaining := h - 4 - statusH
	if remaining < dashMidMinHeight+dashLogsMinHeight {
		return dashLayout{wide: false, status: rect{w: w}}
	}

	// Logs take the larger share: they're the only pane whose useful
	// content is unbounded, and the other two have natural row counts.
	midH := remaining * 40 / 100
	if midH < dashMidMinHeight {
		midH = dashMidMinHeight
	}
	logsH := remaining - midH
	if logsH < dashLogsMinHeight {
		logsH = dashLogsMinHeight
		midH = remaining - logsH
	}

	// Interior width is w-2 (side borders); the middle row spends one
	// more cell on the vertical divider between its two panes.
	innerW := w - 2
	leftW := (innerW - 1) / 2
	rightW := innerW - 1 - leftW

	statusY := 1
	midY := statusY + statusH + 1
	logsY := midY + midH + 1

	return dashLayout{
		wide:   true,
		status: rect{x: 1, y: statusY, w: innerW, h: statusH},
		left:   rect{x: 1, y: midY, w: leftW, h: midH},
		right:  rect{x: 1 + leftW + 1, y: midY, w: rightW, h: midH},
		logs:   rect{x: 1, y: logsY, w: innerW, h: logsH},
		frame:  dashFrame(w, h, statusH, midH, logsH, leftW, rightW, th),
	}
}

// dashFrame draws the border grid for the wide layout: one string of
// exactly w×h cells with blank interiors, which the caller splices
// pane content into via overlayAt. Drawing the whole grid up front is
// what lets adjacent panes share a border line and produce proper
// ┬ / ┴ junctions — rendering each pane as its own lipgloss box would
// double every interior edge.
func dashFrame(w, h, statusH, midH, logsH, leftW, rightW int, th Theme) string {
	innerW := w - 2
	var b strings.Builder

	line := func(s string) {
		b.WriteString(th.Dim.Render(s))
		b.WriteByte('\n')
	}
	blankRows := func(n int, segments ...int) {
		row := "│"
		for _, seg := range segments {
			row += strings.Repeat(" ", seg) + "│"
		}
		for i := 0; i < n; i++ {
			line(row)
		}
	}

	line("┌" + strings.Repeat("─", innerW) + "┐")
	blankRows(statusH, innerW)
	line("├" + strings.Repeat("─", leftW) + "┬" + strings.Repeat("─", rightW) + "┤")
	blankRows(midH, leftW, rightW)
	line("├" + strings.Repeat("─", leftW) + "┴" + strings.Repeat("─", rightW) + "┤")
	blankRows(logsH, innerW)
	b.WriteString(th.Dim.Render("└" + strings.Repeat("─", innerW) + "┘"))

	return b.String()
}

// dashPaneTitle splices an already-styled pane label into a frame
// border line, surrounded by a blank cell each side so it reads as a
// label on the rule rather than part of it. The label is pre-styled by
// the caller because a title like "LOGS ─ ● live ─ envoy" mixes three
// styles in one string.
func dashPaneTitle(frame, label string, col, row int) string {
	if label == "" {
		return frame
	}
	return overlayAt(frame, " "+label+" ", col, row)
}

// splicePane places already-sized pane content into the frame at r.
// Content is clamped to the rect first so a renderer that overshoots
// by a row can't push the frame's bottom border off the canvas.
func splicePane(frame, content string, r rect) string {
	if r.empty() {
		return frame
	}
	return overlayAt(frame, clampCanvas(content, r.w, r.h), r.x, r.y)
}

// dashStackedBox wraps one pane as a self-contained bordered box for
// the stacked layout. Unlike the wide frame these don't share edges —
// at narrow widths the doubled border reads fine and the junction
// math isn't worth it for a shape the user scrolls through anyway.
func dashStackedBox(title, content string, w int, focused bool, th Theme) string {
	if w < 4 {
		return ""
	}
	style := th.Header
	border := lipgloss.Color("244")
	if focused {
		style = th.Title
		border = lipgloss.Color("#7dd3fc")
	}
	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(border).
		Width(w - 2).
		Render(content)
	// Left-aligned, not centred like the modal boxes: stacked panes
	// read as a column of sections, and a ragged set of centred titles
	// gives the eye nothing to track down the left edge.
	return dashPaneTitle(box, style.Render(title), 2, 0)
}

// scrollWindow slices a rendered multi-line string down to a height-h
// viewport starting at offset, clamping the offset to the content. The
// fleet overview does the same thing inline; the dashboard's stacked
// mode needs it too, so it lives here.
func scrollWindow(s string, offset, height int) (string, int) {
	if height < 1 {
		height = 1
	}
	lines := strings.Split(s, "\n")
	maxStart := len(lines) - height
	if maxStart < 0 {
		maxStart = 0
	}
	if offset > maxStart {
		offset = maxStart
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + height
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[offset:end], "\n"), offset
}
