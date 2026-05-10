package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// deleteConfirmState holds the typed-name confirm modal's state.
type deleteConfirmState struct {
	open    bool
	ref     cluster.DescribeRef
	typed   string
	target  string // expected typed value (last 5 chars of name, or whole name if shorter)
	pending bool   // true after Enter accepted; suppress further input
}

// expectedConfirmation returns the substring the user has to retype.
// Last 5 chars (per the original UX plan), or the whole name when
// shorter than 5.
func expectedConfirmation(name string) string {
	if len(name) <= 5 {
		return name
	}
	return name[len(name)-5:]
}

func (m Model) openDeleteConfirm(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	m.deleteConfirm.open = true
	m.deleteConfirm.ref = ref
	m.deleteConfirm.typed = ""
	m.deleteConfirm.target = expectedConfirmation(ref.Name)
	m.deleteConfirm.pending = false
	return m, nil
}

func (m Model) handleDeleteConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.deleteConfirm.pending {
		// Once Enter is accepted, suppress input until result lands —
		// except ctrl+c, which must still quit the program. Treating
		// it the same as Esc here meant the user couldn't quit during
		// "deleting…", inconsistent with every other context.
		switch k.Type {
		case tea.KeyEsc:
			m.deleteConfirm.open = false
			m.deleteConfirm.pending = false
		case tea.KeyCtrlC:
			m.quitMsg = "bye"
			return m, tea.Quit
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc:
		m.deleteConfirm.open = false
		return m, nil
	case tea.KeyCtrlC:
		m.quitMsg = "bye"
		return m, tea.Quit
	case tea.KeyBackspace:
		if r := []rune(m.deleteConfirm.typed); len(r) > 0 {
			m.deleteConfirm.typed = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyEnter:
		if m.deleteConfirm.typed == m.deleteConfirm.target && m.OnDelete != nil {
			ref := m.deleteConfirm.ref
			m.deleteConfirm.pending = true
			cb := m.OnDelete
			focused := m.WatchedContext
			return m, func() tea.Msg { return cb(focused, ref) }
		}
		return m, nil
	case tea.KeyRunes:
		m.deleteConfirm.typed += string(k.Runes)
		return m, nil
	case tea.KeySpace:
		m.deleteConfirm.typed += " "
		return m, nil
	}
	return m, nil
}

func (m Model) renderDeleteConfirm(canvasWidth, canvasHeight int) string {
	if !m.deleteConfirm.open {
		return ""
	}
	const w = 56

	ref := m.deleteConfirm.ref
	matched := m.deleteConfirm.typed == m.deleteConfirm.target

	var b strings.Builder
	title := m.Theme.StatusBad.Render(" confirm delete ")
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n\n")

	subject := fmt.Sprintf(" Delete %s/%s", ref.Kind, ref.Name)
	if ref.Namespace != "" {
		subject += " in " + ref.Namespace
	}
	b.WriteString(m.Theme.Base.Render(subject) + "\n\n")

	prompt := fmt.Sprintf(" Type the last %d chars of the name to confirm:",
		len(m.deleteConfirm.target))
	b.WriteString(m.Theme.Dim.Render(prompt) + "\n\n")

	caret := m.Theme.Title.Render("█")
	if m.deleteConfirm.pending {
		caret = m.Theme.Dim.Render(" deleting…")
	}
	inputStyle := m.Theme.Base
	if matched {
		inputStyle = m.Theme.StatusOK
	}
	b.WriteString("   " + inputStyle.Render(m.deleteConfirm.typed) + caret + "\n\n")

	if matched && !m.deleteConfirm.pending {
		b.WriteString(m.Theme.StatusOK.Render(" ✓ Press Enter to delete") + "\n")
	} else if !m.deleteConfirm.pending {
		expected := m.Theme.Dim.Render(" expected: ") + m.Theme.Dim.Render(m.deleteConfirm.target)
		b.WriteString(expected + "\n")
	}
	b.WriteString("\n" + m.Theme.Footer.Render(" Esc cancel"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("9")). // red-ish border for destructive
		Padding(0, 2).
		Width(w).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
