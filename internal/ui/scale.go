package ui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// scaleConfirmState owns the Scale modal — single-line numeric input
// pre-populated with the deployment's current replica count.
type scaleConfirmState struct {
	open    bool
	ref     cluster.DescribeRef
	current int32
	typed   string
	pending bool
	errMsg  string // last validation error
}

// ScaleResultMsg is fired when the supervisor's Scale call returns.
type ScaleResultMsg cluster.ScaleResult

func (m Model) openScaleConfirm(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	current := int32(0)
	for _, d := range m.deployments {
		if d.Namespace == ref.Namespace && d.Name == ref.Name {
			current = d.Replicas
			break
		}
	}
	m.scaleConfirm.open = true
	m.scaleConfirm.ref = ref
	m.scaleConfirm.current = current
	m.scaleConfirm.typed = strconv.Itoa(int(current))
	m.scaleConfirm.pending = false
	m.scaleConfirm.errMsg = ""
	return m, nil
}

func (m Model) handleScaleConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.scaleConfirm.pending {
		// ctrl+c must still quit even while "scaling…" is on screen.
		switch k.Type {
		case tea.KeyEsc:
			m.scaleConfirm.open = false
			m.scaleConfirm.pending = false
		case tea.KeyCtrlC:
			m.quitMsg = "bye"
			return m, tea.Quit
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc:
		m.scaleConfirm.open = false
		return m, nil
	case tea.KeyCtrlC:
		m.quitMsg = "bye"
		return m, tea.Quit
	case tea.KeyBackspace:
		if r := []rune(m.scaleConfirm.typed); len(r) > 0 {
			m.scaleConfirm.typed = string(r[:len(r)-1])
			m.scaleConfirm.errMsg = ""
		}
		return m, nil
	case tea.KeyEnter:
		n, err := strconv.Atoi(strings.TrimSpace(m.scaleConfirm.typed))
		if err != nil {
			m.scaleConfirm.errMsg = "not a number"
			return m, nil
		}
		if n < 0 {
			m.scaleConfirm.errMsg = "replicas must be ≥ 0"
			return m, nil
		}
		if int32(n) == m.scaleConfirm.current {
			m.scaleConfirm.errMsg = "already at this size"
			return m, nil
		}
		if m.OnScale == nil {
			return m, nil
		}
		ref := m.scaleConfirm.ref
		m.scaleConfirm.pending = true
		cb := m.OnScale
		focused := m.WatchedContext
		replicas := int32(n)
		return m, func() tea.Msg { return cb(focused, ref, replicas) }
	case tea.KeyRunes:
		// Digits only — ignore stray letters so the input never
		// becomes invalid mid-typing.
		s := string(k.Runes)
		if isDigits(s) {
			m.scaleConfirm.typed += s
			m.scaleConfirm.errMsg = ""
		}
		return m, nil
	}
	return m, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (m Model) renderScaleConfirm(canvasWidth, canvasHeight int) string {
	if !m.scaleConfirm.open {
		return ""
	}
	const w = 56
	ref := m.scaleConfirm.ref

	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" scale ") + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n\n")

	subject := fmt.Sprintf(" Scale Deployment/%s in %s", ref.Name, ref.Namespace)
	b.WriteString(m.Theme.Base.Render(subject) + "\n")
	b.WriteString(m.Theme.Dim.Render(fmt.Sprintf(" current: %d replicas",
		m.scaleConfirm.current)) + "\n\n")

	prompt := " Target replica count:"
	b.WriteString(m.Theme.Dim.Render(prompt) + "\n\n")

	caret := m.Theme.Title.Render("█")
	if m.scaleConfirm.pending {
		caret = m.Theme.Dim.Render(" scaling…")
	}
	b.WriteString("   " +
		m.Theme.Title.Render(m.scaleConfirm.typed) + caret + "\n\n")

	switch {
	case m.scaleConfirm.pending:
		// nothing extra
	case m.scaleConfirm.errMsg != "":
		b.WriteString(m.Theme.StatusBad.Render(" ✕ "+m.scaleConfirm.errMsg) + "\n")
	default:
		b.WriteString(m.Theme.StatusOK.Render(" ✓ Press Enter to apply") + "\n")
	}
	b.WriteString("\n" + m.Theme.Footer.Render(" digits + Backspace · Enter apply · Esc cancel"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 2).
		Width(w).
		Render(b.String())
	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
