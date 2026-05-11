package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// handleRBACKey traps input while the RBAC overlay is on screen. Only
// R, Esc, q, and Ctrl-C are honoured; everything else is a no-op so
// table keybindings can't accidentally mutate state behind the modal.
func (m Model) handleRBACKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "R", "esc", "q":
		m.rbacOpen = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	}
	return m, nil
}

// renderRBAC draws the RBAC overview. Each row shows what we asked the
// apiserver (verb + resource), the answer at cluster scope and at the
// active namespace scope (when set), and a denial reason if any. The
// goal is to give the user a single screen that explains "why is X
// missing from the action menu?"
func (m Model) renderRBAC(canvasWidth, canvasHeight int) string {
	if !m.rbacOpen {
		return ""
	}

	w := 80
	if canvasWidth-6 < w {
		w = canvasWidth - 6
	}
	if w < 50 {
		w = 50
	}

	probes := rbacProbeSet()
	showNs := m.namespace != ""

	// Two-column layout: action label (variable) + ✓/✗/? per scope +
	// trailing dim reason. Reserve a fixed column for the label so the
	// glyphs align across all rows.
	const (
		labelCol    = 26
		scopeCol    = 12 // "cluster ✓" or "ns ✓" — fits the longest token
		reasonStart = labelCol + 2 + scopeCol + 2 + scopeCol + 2
	)

	var b strings.Builder
	title := m.Theme.Title.Render(" RBAC ") +
		m.Theme.Dim.Render(" cluster "+m.WatchedContext)
	if showNs {
		title += m.Theme.Dim.Render(" · ns " + m.namespace)
	}
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")

	currentSection := ""
	for _, p := range probes {
		if p.Group != currentSection {
			if currentSection != "" {
				b.WriteByte('\n')
			}
			b.WriteString(" " + m.Theme.Header.Render(p.Group) + "\n")
			currentSection = p.Group
		}

		clusterKey := cluster.PermissionKey(m.WatchedContext, p.Verb, p.APIGroup, p.Resource, "")
		clusterCell, clusterReason := m.rbacCell("cluster", clusterKey)

		nsCell := ""
		nsReason := ""
		if showNs {
			nsKey := cluster.PermissionKey(m.WatchedContext, p.Verb, p.APIGroup, p.Resource, m.namespace)
			nsCell, nsReason = m.rbacCell("ns", nsKey)
		}

		label := p.Action + m.Theme.Dim.Render("  "+verbResourceSummary(p))
		row := "  " + padCellANSI(label, labelCol) + "  " +
			padCellANSI(clusterCell, scopeCol)
		if showNs {
			row += "  " + padCellANSI(nsCell, scopeCol)
		}

		// Prefer the cluster reason, fall back to the ns reason. Either
		// one tells the user the same story ("forbidden by RBAC: …");
		// duplicating both would just bloat the row.
		reason := clusterReason
		if reason == "" {
			reason = nsReason
		}
		if reason != "" {
			budget := w - reasonStart - 4
			if budget < 8 {
				budget = 8
			}
			row += "  " + m.Theme.Dim.Render(truncate(reason, budget))
		}
		b.WriteString(row + "\n")
	}

	b.WriteString("\n")
	b.WriteString(m.Theme.Footer.Render(" press R or Esc to close"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 2).
		Width(w).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

// rbacCell renders one (scope, key) pair as a styled glyph + scope
// label. Returns the cell and any denial reason so the caller can lay
// the reason out separately on the row.
func (m Model) rbacCell(scope, key string) (string, string) {
	if state, ok := m.permissions[key]; ok {
		switch {
		case state.Err != "":
			return scope + " " + m.Theme.StatusWrn.Render("!"), state.Err
		case state.Allowed:
			return scope + " " + m.Theme.StatusOK.Render("✓"), ""
		default:
			return scope + " " + m.Theme.StatusBad.Render("✗"), state.Reason
		}
	}
	if _, busy := m.permissionsInFlight[key]; busy {
		return scope + " " + m.Theme.Dim.Render("?"), ""
	}
	return scope + " " + m.Theme.Dim.Render("—"), ""
}

// verbResourceSummary renders the SSAR coordinates compactly, mirroring
// the form `kubectl auth can-i` shows: "<verb> <group/resource>".
func verbResourceSummary(p rbacProbe) string {
	res := p.Resource
	if p.APIGroup != "" {
		res = p.APIGroup + "/" + res
	}
	return p.Verb + " " + res
}
