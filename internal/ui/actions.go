package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// Action identifies a single menu entry.
type Action int

const (
	ActDescribe Action = iota
	ActLogs
	ActExec
	ActEvents
	ActScale
	ActRestart
	ActCordon
	ActUncordon
	ActDrain
	ActSetNamespace
	ActDelete
)

func (a Action) Label() string {
	switch a {
	case ActDescribe:
		return "Describe"
	case ActLogs:
		return "Logs"
	case ActExec:
		return "Shell"
	case ActEvents:
		return "Events"
	case ActScale:
		return "Scale"
	case ActRestart:
		return "Restart (Rollout)"
	case ActCordon:
		return "Cordon"
	case ActUncordon:
		return "Uncordon"
	case ActDrain:
		return "Drain"
	case ActSetNamespace:
		return "Set as active namespace"
	case ActDelete:
		return "Delete"
	}
	return "?"
}

// destructive returns true for actions that should require an extra
// confirmation step. Drain joins Delete here — evicting every pod on
// a node is the same blast-radius shape.
func (a Action) destructive() bool { return a == ActDelete || a == ActDrain }

// actionsFor returns the menu items applicable to a given Kind.
// Kept tiny and explicit so adding a new resource is one obvious spot.
//
// Node lists *both* Cordon and Uncordon — the caller prunes one based
// on the node's current Schedulable state before showing the menu.
// Keeping it state-blind here avoids threading runtime state through
// the actionsFor signature.
func actionsFor(kind string) []Action {
	switch kind {
	case "Pod":
		return []Action{ActDescribe, ActLogs, ActExec, ActEvents, ActDelete}
	case "Deployment":
		return []Action{ActDescribe, ActScale, ActRestart, ActLogs, ActEvents, ActDelete}
	case "Node":
		return []Action{ActDescribe, ActEvents, ActCordon, ActUncordon, ActDrain}
	case "Namespace", "Project":
		// Set-as-active first because that's the by-far most-frequent
		// action on a namespace / project row — the user already has
		// the cursor on it, one Enter saves three keystrokes from the
		// picker. Project (OpenShift) and Namespace get the same set;
		// Project routes through the project.openshift.io API but the
		// user-facing operations are identical.
		return []Action{ActSetNamespace, ActDescribe, ActEvents, ActDelete}
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
	case ActEvents:
		// Scoping the events view to a resource just walks the
		// already-watched event cache — no extra API call is made.
		// But the event watcher itself needs `list/watch events` to
		// have populated that cache, so gate on the same verb so we
		// don't offer a menu item the user's RBAC can't actually feed.
		return actionVerb{"list", "", "events"}, true
	case ActExec:
		// `kubectl exec` is `create pods/exec` under the covers — a
		// subresource POST that opens a streaming session. Many
		// developer-tier roles get pods/get + pods/log but not
		// pods/exec; hiding the menu item when denied avoids users
		// hitting Enter on something they'll never be allowed to run.
		return actionVerb{"create", "", "pods/exec"}, true
	case ActCordon, ActUncordon:
		// Both go through a strategic-merge PATCH on
		// /spec/unschedulable. `patch nodes` is the single verb.
		return actionVerb{"patch", "", "nodes"}, true
	case ActDrain:
		// Drain cordons first (patch nodes), then evicts every pod
		// (create pods/eviction). We gate on the eviction verb
		// because that's the rarer permission — cluster admins
		// have both; most developer roles have neither; the few
		// roles that have one usually have the other.
		return actionVerb{"create", "", "pods/eviction"}, true
	}
	return actionVerb{}, false
}

// rbacProbe is one row in the RBAC overlay: a labelled tuple of
// verb/group/resource that we want the apiserver to rule on, grouped
// by the Kind it relates to so the overlay can section the output.
type rbacProbe struct {
	Group    string // for display grouping (Pods, Deployments, …) — not the API group
	Action   string // human label ("Logs", "Exec", "Delete pods")
	Verb     string
	APIGroup string // RBAC group ("", "apps")
	Resource string // may include subresource ("pods/log")
}

// rbacProbeSet enumerates every verb the action menu ever gates on,
// expanded to a per-Kind list so the RBAC overlay can show ✓/✗ per
// (verb, resource) combination. Kept here next to verbsForAction so
// adding a new gated action surfaces obviously in one place.
func rbacProbeSet() []rbacProbe {
	return []rbacProbe{
		{"Pods", "Logs", "get", "", "pods/log"},
		{"Pods", "Exec", "create", "", "pods/exec"},
		{"Pods", "Delete", "delete", "", "pods"},
		{"Pods", "Events (list)", "list", "", "events"},
		{"Deployments", "Scale", "update", "apps", "deployments/scale"},
		{"Deployments", "Restart (patch)", "patch", "apps", "deployments"},
		{"Deployments", "Delete", "delete", "apps", "deployments"},
		{"Nodes", "Cordon / Uncordon", "patch", "", "nodes"},
		{"Nodes", "Drain (evict pods)", "create", "", "pods/eviction"},
		{"Nodes", "Delete", "delete", "", "nodes"},
		{"Namespaces", "Delete", "delete", "", "namespaces"},
	}
}

// actionStatus describes the RBAC state of one menu row at render time.
// Until this PR the menu hid denied rows entirely; we now classify
// them so the renderer can dim a "no permission" line in-place. That
// gives the user a concrete reason for a missing capability instead of
// a button they can never see.
type actionStatus int

const (
	actionAllowed actionStatus = iota
	actionPending              // SSAR dispatched, awaiting reply
	actionDenied
)

// actionItem pairs an Action with its render-time RBAC status. The
// action menu stores these directly so render + Enter handler agree on
// what's actionable without re-querying the cache.
type actionItem struct {
	Action Action
	Status actionStatus
	Reason string // verbatim SSAR denial reason; empty when allowed/pending
}

// actionMenuState is the modal's state.
type actionMenuState struct {
	open    bool
	ref     cluster.DescribeRef
	options []actionItem
	cursor  int
	notice  string // ephemeral status line (e.g. "Logs: not yet implemented")
}

// renderActionMenuPanel returns just the bordered modal box (no
// canvas placement). The caller composites it over the underlying
// table via overlayAt so the row the user came from stays visible
// alongside the menu.
//
// Layout: title "ACTIONS" → kind / name / namespace stacked on
// separate lines so realistic-length pod names don't cram the title,
// → action rows with trailing status glyphs for denied (✗) and
// pending (…). Destructive rows render in red and sit below a dim
// divider so the visual separation reads at a glance.
func (m Model) renderActionMenuPanel() string {
	if !m.actionMenu.open {
		return ""
	}
	const (
		w        = 52
		innerW   = w - 6 // box width minus border (2) and padding (2*2)
		statusAt = innerW - 2
	)

	var b strings.Builder

	// Resource ident is split across the border + first row: the top
	// border carries "<Kind> Actions", the first inner row carries
	// the centered <ns>/<name>, the bottom border carries the cluster
	// context. Keeps the dialog body focused on the action list with
	// minimal chrome.
	resource := centerLine(actionMenuResource(m.actionMenu.ref), innerW, m.Theme.Header)
	b.WriteString(resource + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", innerW)) + "\n")

	// Destructive rows render in red; that's enough to set them apart
	// from safe actions without an extra divider eating a row.
	for i, it := range m.actionMenu.options {
		b.WriteString(m.actionRow(i, it, innerW, statusAt) + "\n")
	}

	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", innerW)) + "\n")
	if m.actionMenu.notice != "" {
		b.WriteString(m.Theme.StatusWrn.Render(m.actionMenu.notice) + "\n")
	}
	b.WriteString(m.Theme.Footer.Render("↑↓ · enter · esc"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(1, 2).
		Width(w).
		Render(b.String())

	// Top border: "<Kind> Actions" in Title style. Bottom border:
	// cluster context in Dim. Reserve 6 cells of border + spacing on
	// each side when truncating so the labels never collide with the
	// corners.
	const titleBudget = w - 6
	top := m.Theme.Title.Render(truncate(actionMenuTitle(m.actionMenu.ref), titleBudget))
	bottom := ""
	if m.WatchedContext != "" {
		bottom = m.Theme.Header.Render(truncate(m.WatchedContext, titleBudget))
	}
	return setBorderTitles(box, top, bottom)
}

// actionMenuTitle returns the "<Kind> Actions" label for the top
// border. Falls back to "Actions" if the kind is unknown.
func actionMenuTitle(ref cluster.DescribeRef) string {
	if ref.Kind == "" {
		return "Actions"
	}
	return ref.Kind + " Actions"
}

// actionMenuResource returns the ns/name pair (or just name for
// cluster-scoped kinds) that's rendered centered as the first content
// row of the dialog. Empty if the menu opened without a target.
func actionMenuResource(ref cluster.DescribeRef) string {
	if ref.Name == "" {
		return ""
	}
	if ref.Namespace != "" {
		return ref.Namespace + "/" + ref.Name
	}
	return ref.Name
}

// centerLine returns s centered inside a width-wide string, styled.
// Long content is truncated to width; short content is padded with
// spaces on both sides.
func centerLine(s string, width int, style lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	t := truncate(s, width)
	w := lipgloss.Width(t)
	if w >= width {
		return style.Render(t)
	}
	left := (width - w) / 2
	right := width - w - left
	return strings.Repeat(" ", left) + style.Render(t) + strings.Repeat(" ", right)
}

// setBorderTitles splices top/bottom labels into the box's first and
// last lines respectively. Either label may be empty (the splice for
// that line is skipped). Reuses the ANSI-aware spliceLine helper so
// border colour state outside the label is preserved.
func setBorderTitles(box, top, bottom string) string {
	if box == "" {
		return box
	}
	lines := strings.Split(box, "\n")
	if len(lines) == 0 {
		return box
	}
	if top != "" {
		lines[0] = spliceCentered(lines[0], top)
	}
	if bottom != "" && len(lines) > 1 {
		last := len(lines) - 1
		lines[last] = spliceCentered(lines[last], bottom)
	}
	return strings.Join(lines, "\n")
}

func spliceCentered(line, label string) string {
	decorated := " " + label + " "
	col := (lipgloss.Width(line) - lipgloss.Width(decorated)) / 2
	if col < 1 {
		col = 1
	}
	return spliceLine(line, decorated, col)
}

// actionRow builds one menu row at width innerW. The trailing status
// glyph (✗ / …) sits at column statusAt so the column reads cleanly
// across rows. Cursor row gets both the › marker and the bg
// highlight (the user asked for both signals).
func (m Model) actionRow(i int, it actionItem, innerW, statusAt int) string {
	marker := "  "
	if i == m.actionMenu.cursor {
		marker = m.Theme.Title.Render("›") + " "
	}

	label := it.Action.Label()
	style := m.Theme.Base
	switch it.Status {
	case actionAllowed:
		if it.Action.destructive() {
			style = m.Theme.StatusBad
		}
	case actionPending, actionDenied:
		style = m.Theme.Dim
	}

	body := marker + style.Render(label)

	// Right-pad the body to statusAt then append the glyph.
	bodyW := lipgloss.Width(body)
	if bodyW < statusAt {
		body += strings.Repeat(" ", statusAt-bodyW)
	}
	switch it.Status {
	case actionPending:
		body += m.Theme.Dim.Render("…")
	case actionDenied:
		body += m.Theme.StatusBad.Render("✗")
	}

	// Final pad-to-innerW so renderSelected paints the highlight all
	// the way across the row.
	bodyW = lipgloss.Width(body)
	if bodyW < innerW {
		body += strings.Repeat(" ", innerW-bodyW)
	}

	if i == m.actionMenu.cursor && it.Status == actionAllowed {
		body = renderSelected(body)
	}
	return body
}
