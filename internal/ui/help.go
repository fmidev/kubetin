package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleHelpKey traps input while the help overlay is on screen.
// Without this, every keybinding (j/k/d/Enter/Tab/etc.) leaks through
// to the underlying view and silently mutates state the user can't
// see. Only ?, Esc, and Ctrl-C are honoured here; everything else is
// a no-op.
func (m Model) handleHelpKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "?", "esc", "q":
		m.helpOpen = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	}
	return m, nil
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
			{"j / ↓", "next row"},
			{"k / ↑", "previous row"},
			{"g", "first row"},
			{"G", "last row"},
		},
	},
	{
		Title: "Cluster",
		Bindings: [][2]string{
			{"Tab", "next reachable cluster"},
			{"Shift-Tab", "previous reachable cluster"},
		},
	},
	{
		Title: "View",
		Bindings: [][2]string{
			{"F1", "fleet overview"},
			{"1", "pods"},
			{"2", "deployments"},
			{"3", "nodes"},
			{"4", "events"},
			{"5", "namespaces"},
		},
	},
	{
		Title: "Filter",
		Bindings: [][2]string{
			{"/", "filter pods by name / namespace"},
			{"n", "namespace picker"},
			{"0", "all namespaces"},
			{"Esc", "clear filter / namespace"},
		},
	},
	{
		Title: "Sort",
		Bindings: [][2]string{
			{"s", "cycle sort column"},
			{"S", "reverse sort direction"},
		},
	},
	{
		Title: "Inspect",
		Bindings: [][2]string{
			{"Enter", "action menu (Describe / Logs / Exec / Events / Cordon / Drain / Delete)"},
			{"d", "describe selected resource"},
			{"Shift-Y", "(inside Secret describe) reveal data"},
		},
	},
	{
		Title: "Logs (when open)",
		Bindings: [][2]string{
			{"/", "search log buffer"},
			{"n / N", "next / previous match"},
			{"f", "toggle follow"},
			{"g / G", "top / bottom"},
		},
	},
	{
		Title: "System",
		Bindings: [][2]string{
			{"?", "this help"},
			{"R", "RBAC permissions overlay"},
			{"F2", "debug overlay"},
			{"q / Ctrl-C", "quit"},
		},
	},
}

// renderHelp draws the help overlay. canvasWidth/Height are the body
// region inside which we centre the box.
func (m Model) renderHelp(canvasWidth, canvasHeight int) string {
	if !m.helpOpen {
		return ""
	}

	// Build content as a 2-column layout: groups stacked vertically,
	// each group's bindings rendered as "key  description" rows.
	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" kubetin · keybindings ") + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", 56)) + "\n")
	for _, g := range helpGroups {
		b.WriteString("\n " + m.Theme.Header.Render(g.Title) + "\n")
		for _, kv := range g.Bindings {
			key := lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7dd3fc")).
				Width(14).
				Render(" " + kv[0])
			desc := lipgloss.NewStyle().
				Width(40).
				Render(kv[1])
			b.WriteString(key + " " + desc + "\n")
		}
	}
	if m.Build != "" {
		b.WriteString("\n " + m.Theme.Dim.Render(m.Build) + "\n")
	}
	b.WriteString("\n" + m.Theme.Footer.Render(" press ? or Esc to close"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 2).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
