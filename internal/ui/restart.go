package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// restartConfirmState owns the rollout-restart confirm modal. Lighter
// than delete: a rolling restart preserves availability, so we use a
// single Y/Enter confirmation rather than typed-name reauth.
type restartConfirmState struct {
	open    bool
	ref     cluster.DescribeRef
	pending bool
}

// RolloutResultMsg is fired when the supervisor's RolloutRestart returns.
type RolloutResultMsg cluster.RolloutResult

func (m Model) openRestartConfirm(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	m.restartConfirm.open = true
	m.restartConfirm.ref = ref
	m.restartConfirm.pending = false
	return m, nil
}

func (m Model) handleRestartConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.restartConfirm.pending {
		// ctrl+c must still quit even while "rolling…" is on screen.
		switch k.Type {
		case tea.KeyEsc:
			m.restartConfirm.open = false
			m.restartConfirm.pending = false
		case tea.KeyCtrlC:
			m.quitMsg = "bye"
			return m, tea.Quit
		}
		return m, nil
	}
	switch k.String() {
	case "esc", "n", "N":
		m.restartConfirm.open = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "y", "Y", "enter":
		if m.OnRolloutRestart == nil {
			return m, nil
		}
		ref := m.restartConfirm.ref
		m.restartConfirm.pending = true
		cb := m.OnRolloutRestart
		focused := m.WatchedContext
		return m, func() tea.Msg { return cb(focused, ref) }
	}
	return m, nil
}

func (m Model) renderRestartConfirm(canvasWidth, canvasHeight int) string {
	if !m.restartConfirm.open {
		return ""
	}
	const w = 56
	ref := m.restartConfirm.ref

	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" restart rollout ") + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n\n")

	subject := fmt.Sprintf(" Trigger rolling restart of Deployment/%s in %s",
		ref.Name, ref.Namespace)
	b.WriteString(m.Theme.Base.Render(subject) + "\n\n")

	b.WriteString(m.Theme.Dim.Render(
		" This patches `spec.template.metadata.annotations` to start a new\n"+
			" ReplicaSet — same effect as `kubectl rollout restart`. Pods will\n"+
			" be replaced under the deployment's update strategy.") + "\n\n")

	if m.restartConfirm.pending {
		b.WriteString(m.Theme.Dim.Render(" rolling…") + "\n")
	} else {
		b.WriteString(m.Theme.StatusOK.Render(" ✓ Press Y or Enter to confirm") + "\n")
	}
	b.WriteString("\n" + m.Theme.Footer.Render(" Y/Enter confirm · N/Esc cancel"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 2).
		Width(w).
		Render(b.String())
	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
