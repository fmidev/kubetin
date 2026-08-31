package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleHelpKey traps input while the help overlay is on screen.
// Without this, every keybinding (j/k/d/Enter/Tab/etc.) leaks through
// to the underlying view and silently mutates state the user can't
// see.
//
// Honoured: ? / Esc / q to close, j / k and the arrows to scroll,
// ctrl-d / ctrl-u and PgUp / PgDn to page, g / G and Home / End to
// jump, ctrl-c to quit. Everything else is a no-op.
func (m Model) handleHelpKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The sheet is taller than most terminals, so it has to scroll —
	// without this the bottom half was simply unreachable.
	w, h := m.helpCanvas()
	// Normalize first: the renderer clamps a local copy, so a resize
	// that shortens the content (one column becoming two) leaves the
	// stored offset above the new maximum, and the next j or k is
	// spent snapping back to a position already on screen.
	m.helpScroll = m.clampHelpScroll(m.helpScroll, h, w)
	switch k.String() {
	case "?", "esc", "q":
		m.helpOpen = false
		m.helpScroll = 0
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		m.helpScroll = m.clampHelpScroll(m.helpScroll+1, h, w)
	case "k", "up":
		m.helpScroll = m.clampHelpScroll(m.helpScroll-1, h, w)
	case "ctrl+d", "pgdown":
		m.helpScroll = m.clampHelpScroll(m.helpScroll+helpViewport(h)/2, h, w)
	case "ctrl+u", "pgup":
		m.helpScroll = m.clampHelpScroll(m.helpScroll-helpViewport(h)/2, h, w)
	case "g", "home":
		m.helpScroll = 0
	case "G", "end":
		m.helpScroll = m.clampHelpScroll(1<<30, h, w)
	}
	return m, nil
}

// helpCanvas mirrors View()'s body arithmetic so the key handler and
// the renderer agree on how much fits, the same way dashCanvasSize
// does for the dashboard.
func (m Model) helpCanvas() (int, int) {
	headerH, footerH := m.chromeHeights()
	h := m.height - headerH - footerH
	if h < 1 {
		h = 1
	}
	return helpBoxWidth(m.width) - 2, h
}

// helpGroup is one section of the help overlay.
type helpGroup struct {
	Title    string
	Bindings [][2]string // pairs of {key, description}
}

// helpGroups is the source of truth for what kubetin binds. When you
// add a key in app.go, also add it here so ? stays accurate.
var helpGroups = []helpGroup{
	{
		Title: "Move",
		Bindings: [][2]string{
			{"j / k", "next / previous row"},
			{"g / G", "first / last row"},
		},
	},
	{
		Title: "Views",
		Bindings: [][2]string{
			{"1", "pods"},
			{"2", "deployments"},
			{"3", "services"},
			{"4", "ingresses"},
			{"5", "nodes"},
			{"6", "namespaces"},
			{"F1", "fleet dashboard (toggle)"},
		},
	},
	{
		Title: "Inspect the selected row",
		Bindings: [][2]string{
			{"i", "status dashboard"},
			{"l", "logs"},
			{"e", "events"},
			{"d", "describe"},
			{"Enter", "action menu"},
		},
	},
	{
		Title: "Narrow",
		Bindings: [][2]string{
			{"/", "filter by name / namespace"},
			{"n", "namespace picker"},
			{"0", "all namespaces"},
			{"Esc", "clear filter / namespace"},
		},
	},
	{
		Title: "Sort",
		Bindings: [][2]string{
			{"s", "cycle sort column"},
			{"S", "reverse direction"},
		},
	},
	{
		Title: "Cluster",
		Bindings: [][2]string{
			{"Tab / Shift-Tab", "next / previous cluster"},
			{"C", "show / hide cluster rail"},
		},
	},
	{
		Title: "Fleet dashboard",
		Bindings: [][2]string{
			{"j / k", "next / previous cluster"},
			{"g / G", "first / last cluster"},
			{"Enter", "expand / collapse cluster details"},
			{"r", "refresh details"},
			{"o", "open cluster (pods view)"},
			{"Esc / F1", "back to previous view"},
		},
	},
	{
		Title: "Events lens",
		Bindings: [][2]string{
			{"E", "open for namespace / cluster"},
			{"E", "(inside) widen to everything"},
			{"j / k, g / G", "(inside) scroll"},
			{"e / Esc", "(inside) close"},
		},
	},
	{
		Title: "Dashboard",
		Bindings: [][2]string{
			{"Tab / Shift-Tab", "next / previous pane"},
			{"j / k, g / G", "scroll focused pane"},
			{"f", "toggle log follow"},
			{"c", "next container"},
			{"l", "logs full-screen"},
			{"i", "(Deployment) open pod"},
			{"Enter", "actions for the focused pane"},
			{"d", "describe"},
			{"Esc", "back / close"},
		},
	},
	{
		Title: "Logs viewer",
		Bindings: [][2]string{
			{"/", "search buffer"},
			{"n / N", "next / previous match"},
			{"f", "toggle follow"},
			{"g / G", "top / bottom"},
			{"L", "load the whole log"},
		},
	},
	{
		Title: "System",
		Bindings: [][2]string{
			{"?", "this help"},
			{"R", "RBAC permissions overlay"},
			{"Shift-Y", "reveal Secret data"},
			{"F2", "debug overlay"},
			{"q / Ctrl-C", "quit"},
		},
	},
}

// renderHelp draws the help overlay. canvasWidth/Height are the body
// region inside which we centre the box.
// Help layout. Two columns when the terminal can hold them, which is
// what keeps the whole sheet on screen at normal sizes; scrolling then
// only matters on genuinely small terminals.
const (
	helpKeyWidth  = 16
	helpDescWidth = 34
	helpColWidth  = helpKeyWidth + 1 + helpDescWidth
	helpColGap    = 3
	// Content width two columns need. The box adds its two border
	// columns on top; conflating the two is what silently kept the
	// layout at one column.
	helpTwoColWidth = 2*helpColWidth + helpColGap
)

// helpBoxWidth is the outer box width for a given canvas: two columns
// where they fit, otherwise as much of the canvas as we'll take.
func helpBoxWidth(canvasWidth int) int {
	w := canvasWidth - 4
	if w > helpTwoColWidth+2 {
		w = helpTwoColWidth + 2
	}
	if w < 24 {
		w = 24
	}
	return w
}

// helpBlocks renders each group as a block of lines: a title, its
// bindings, then a trailing blank as the separator.
func (m Model) helpBlocks() [][]string {
	out := make([][]string, 0, len(helpGroups))
	for _, g := range helpGroups {
		lines := make([]string, 0, len(g.Bindings)+2)
		lines = append(lines, " "+m.Theme.Header.Render(g.Title))
		for _, kv := range g.Bindings {
			key := lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7dd3fc")).
				Width(helpKeyWidth).
				Render(" " + truncate(kv[0], helpKeyWidth-1))
			desc := lipgloss.NewStyle().
				Width(helpDescWidth).
				Render(truncate(kv[1], helpDescWidth))
			lines = append(lines, key+" "+desc)
		}
		lines = append(lines, "")
		out = append(out, lines)
	}
	return out
}

// packHelpColumns distributes blocks across n columns, keeping group
// order down each column and balancing height so one column doesn't
// run far past the other.
func packHelpColumns(blocks [][]string, n int) [][]string {
	if n < 2 {
		flat := make([]string, 0, 64)
		for _, b := range blocks {
			flat = append(flat, b...)
		}
		return [][]string{flat}
	}
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	target := (total + n - 1) / n

	cols := make([][]string, 0, n)
	cur := make([]string, 0, target+8)
	for _, b := range blocks {
		// Start a new column once this one has met its share — but
		// never leave the last column to soak up everything, so stop
		// splitting when only one slot remains.
		if len(cur) >= target && len(cols) < n-1 {
			cols = append(cols, cur)
			cur = make([]string, 0, target+8)
		}
		cur = append(cur, b...)
	}
	cols = append(cols, cur)
	return cols
}

// helpBody joins the packed columns side by side into one block of
// lines, padding short columns so the join stays rectangular.
func (m Model) helpBody(width int) []string {
	n := 1
	if width >= helpTwoColWidth {
		n = 2
	}
	cols := packHelpColumns(m.helpBlocks(), n)
	if len(cols) == 1 {
		// Rows are built at the fixed helpColWidth; on a terminal
		// narrower than that, Style.Width won't cap them, so the box
		// grows past the canvas and View clips the descriptions and
		// the right border.
		out := make([]string, 0, len(cols[0]))
		for _, l := range cols[0] {
			out = append(out, padCellANSI(l, width))
		}
		return out
	}

	height := 0
	for _, c := range cols {
		if len(c) > height {
			height = len(c)
		}
	}
	gap := strings.Repeat(" ", helpColGap)
	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		row := ""
		for ci, c := range cols {
			cell := ""
			if i < len(c) {
				cell = c[i]
			}
			cell = padCellANSI(cell, helpColWidth)
			if ci > 0 {
				row += gap
			}
			row += cell
		}
		out = append(out, strings.TrimRight(row, " "))
	}
	return out
}

// helpChrome is the rows the box spends on furniture: title,
// separator, footer and the two border rows.
const helpChrome = 5

// helpViewport is how many body rows the box can show.
func helpViewport(canvasHeight int) int {
	v := canvasHeight - helpChrome
	// Floor of 1, not 3: a larger floor makes the box taller than the
	// canvas on a very short terminal, and View then clips the bottom
	// border. One row of content is little use, but overflowing is
	// worse than useless.
	if v < 1 {
		v = 1
	}
	return v
}

// clampHelpScroll bounds the offset to the content, so j at the bottom
// saturates rather than running away.
func (m Model) clampHelpScroll(want, canvasHeight, canvasWidth int) int {
	if want < 0 {
		return 0
	}
	max := len(m.helpBody(canvasWidth)) - helpViewport(canvasHeight)
	if max < 0 {
		max = 0
	}
	if want > max {
		return max
	}
	return want
}

func (m Model) renderHelp(canvasWidth, canvasHeight int) string {
	if !m.helpOpen {
		return ""
	}

	w := helpBoxWidth(canvasWidth)
	innerW := w - 2

	lines := m.helpBody(innerW)
	viewport := helpViewport(canvasHeight)
	scroll := m.clampHelpScroll(m.helpScroll, canvasHeight, innerW)

	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" kubetin · keybindings ") + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", innerW)) + "\n")

	body, _ := scrollWindow(strings.Join(lines, "\n"), scroll, viewport)
	b.WriteString(body)
	for i := lipgloss.Height(body); i < viewport; i++ {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	// Say where you are only when there's something off screen —
	// otherwise the footer implies scrolling that does nothing.
	pos := ""
	if len(lines) > viewport {
		pos = fmt.Sprintf(" · %d-%d of %d · j/k scroll",
			scroll+1, min(scroll+viewport, len(lines)), len(lines))
	}
	build := ""
	if m.Build != "" {
		build = " · " + m.Build
	}
	b.WriteString(m.Theme.Footer.Render(
		truncate(" ? or Esc to close"+pos+build, innerW)))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Width(w - 2).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
