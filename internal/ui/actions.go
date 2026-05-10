package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// Action identifies a single menu entry.
type Action int

const (
	ActDescribe Action = iota
	ActLogs
	ActScale
	ActRestart
	ActDelete
)

func (a Action) Label() string {
	switch a {
	case ActDescribe:
		return "Describe"
	case ActLogs:
		return "Logs"
	case ActScale:
		return "Scale"
	case ActRestart:
		return "Restart (Rollout)"
	case ActDelete:
		return "Delete"
	}
	return "?"
}

// destructive returns true for actions that should require an extra
// confirmation step (currently just Delete).
func (a Action) destructive() bool { return a == ActDelete }

// actionsFor returns the menu items applicable to a given Kind.
// Kept tiny and explicit so adding a new resource is one obvious spot.
func actionsFor(kind string) []Action {
	switch kind {
	case "Pod":
		return []Action{ActDescribe, ActLogs, ActDelete}
	case "Deployment":
		return []Action{ActDescribe, ActScale, ActRestart, ActLogs, ActDelete}
	case "Node":
		return []Action{ActDescribe}
	}
	return []Action{ActDescribe}
}

// actionVerb is the (verb, group, resource) tuple needed to RBAC-gate
// one action against one resource kind. Kept separate from the global
// PermissionKey so we can express "Logs on a Deployment is actually
// `get pods/log`" without sprinkling overrides across the UI.
type actionVerb struct {
	Verb     string
	Group    string
	Resource string
}

// verbsForAction returns the SSAR query needed to gate an action,
// depending on the kind of resource the cursor points at. Describe
// is left ungated because we render it from informer cache the user
// already has access to.
func verbsForAction(a Action, ref cluster.DescribeRef) (av actionVerb, gated bool) {
	switch a {
	case ActDelete:
		return actionVerb{"delete", ref.Group, ref.Resource}, true
	case ActLogs:
		// Whether the source kind is Pod or Deployment, the read
		// happens through the pods/log subresource — that's the
		// permission we should check.
		return actionVerb{"get", "", "pods/log"}, true
	case ActScale:
		// kubectl scale uses the /scale subresource; any role that
		// allows update on it is sufficient.
		return actionVerb{"update", "apps", "deployments/scale"}, true
	case ActRestart:
		// `kubectl rollout restart` patches a template annotation on
		// the deployment object proper.
		return actionVerb{"patch", "apps", "deployments"}, true
	}
	return actionVerb{}, false
}

// actionMenuState is the modal's state.
type actionMenuState struct {
	open    bool
	ref     cluster.DescribeRef
	options []Action
	cursor  int
	notice  string // ephemeral status line (e.g. "Logs: not yet implemented")
}

// renderActionMenu draws the centered actions modal.
func (m Model) renderActionMenu(canvasWidth, canvasHeight int) string {
	if !m.actionMenu.open {
		return ""
	}
	const w = 42

	var b strings.Builder
	title := m.Theme.Title.Render(" actions ") +
		m.Theme.Dim.Render(" "+actionsTitle(m.actionMenu.ref))
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	for i, a := range m.actionMenu.options {
		marker := "  "
		if i == m.actionMenu.cursor {
			marker = m.Theme.Title.Render(" ›")
		}
		label := a.Label()
		style := m.Theme.Base
		if a.destructive() {
			style = m.Theme.StatusBad
		}
		line := fmt.Sprintf("%s %s", marker, style.Render(label))
		if i == m.actionMenu.cursor {
			line = m.Theme.Selected.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	if m.actionMenu.notice != "" {
		b.WriteString(m.Theme.StatusWrn.Render(" "+m.actionMenu.notice) + "\n")
	}
	b.WriteString(m.Theme.Footer.Render(" j/k  enter  esc"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 1).
		Width(w).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

func actionsTitle(r cluster.DescribeRef) string {
	if r.Name == "" {
		return ""
	}
	if r.Namespace != "" {
		return r.Kind + "/" + r.Name + " · " + r.Namespace
	}
	return r.Kind + "/" + r.Name
}
