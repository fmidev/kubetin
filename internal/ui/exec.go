package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// execState backs the container-picker modal that opens when the
// user picks `Exec` on a multi-container pod. Single-container pods
// bypass the picker entirely.
//
// The exec session itself lives outside bubbletea — tea.Exec
// releases the alt-screen, our cluster.ExecCmd takes over the
// terminal, and we never need to model the running shell here. The
// state below is only the brief pre-flight: "which container?"
type execState struct {
	pickerOpen bool
	ref        cluster.DescribeRef
	containers []string
	cursor     int
	// shell is the command to run inside the container. Hardcoded
	// for now — multi-shell support is a follow-up. /bin/sh is on
	// every reasonable container image, including Alpine, distroless
	// (well, almost), Ubuntu, etc.
	shell []string
}

// ExecDoneMsg is delivered after the exec session ends and
// bubbletea has reclaimed the terminal. Err is nil on a clean exit
// (the user typed `exit` or hit Ctrl-D), non-nil on transport /
// auth / setup failures.
type ExecDoneMsg struct {
	Err error
}

// openExec is the entry point from the action menu's Exec item. If
// the pod has a single container, exec is dispatched immediately;
// otherwise the picker opens and the user chooses.
func (m Model) openExec(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	if m.OnExec == nil {
		return m, nil
	}
	containers := m.containersFor(ref)
	shell := []string{"/bin/sh"}

	switch len(containers) {
	case 0:
		// No spec.containers in cache — shouldn't happen for a real
		// Pod row but bail safely rather than dispatching a doomed
		// exec call.
		m.toast = "Exec: no containers known for this pod"
		m.toastUntil = time.Now().Add(4 * time.Second)
		return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
	case 1:
		return m, m.dispatchExec(ref, containers[0], shell)
	default:
		m.exec.pickerOpen = true
		m.exec.ref = ref
		m.exec.containers = containers
		m.exec.cursor = 0
		m.exec.shell = shell
		return m, nil
	}
}

// containersFor returns the container names for the given ref. Only
// Pods are supported today — Deployment-level exec would have to
// pick a backing pod first, which is a future feature.
func (m Model) containersFor(ref cluster.DescribeRef) []string {
	if ref.Kind != "Pod" {
		return nil
	}
	for _, r := range m.pods {
		if r.Namespace == ref.Namespace && r.Name == ref.Name {
			out := make([]string, len(r.Containers))
			copy(out, r.Containers)
			return out
		}
	}
	return nil
}

func (m Model) dispatchExec(ref cluster.DescribeRef, container string, shell []string) tea.Cmd {
	return m.OnExec(m.WatchedContext, ref, container, shell)
}

// handleExecPickerKey routes input while the container picker is open.
func (m Model) handleExecPickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		m.exec.pickerOpen = false
		return m, nil
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.exec.cursor < len(m.exec.containers)-1 {
			m.exec.cursor++
		}
	case "k", "up":
		if m.exec.cursor > 0 {
			m.exec.cursor--
		}
	case "g":
		m.exec.cursor = 0
	case "G":
		m.exec.cursor = len(m.exec.containers) - 1
	case "enter":
		ref := m.exec.ref
		container := m.exec.containers[m.exec.cursor]
		shell := m.exec.shell
		m.exec.pickerOpen = false
		return m, m.dispatchExec(ref, container, shell)
	}
	return m, nil
}

// renderExecPicker draws the centered container-picker modal. Same
// box style as the action menu so the two read as siblings.
func (m Model) renderExecPicker(canvasWidth, canvasHeight int) string {
	if !m.exec.pickerOpen {
		return ""
	}
	const w = 42
	ref := m.exec.ref

	var b strings.Builder
	title := m.Theme.Title.Render(" exec › container ") +
		m.Theme.Dim.Render(" "+ref.Kind+"/"+ref.Name)
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")

	for i, c := range m.exec.containers {
		marker := "  "
		if i == m.exec.cursor {
			marker = m.Theme.Title.Render(" ›")
		}
		line := fmt.Sprintf("%s %s", marker, c)
		if i == m.exec.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	b.WriteString(m.Theme.Footer.Render(" j/k  enter  esc"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 1).
		Width(w).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
