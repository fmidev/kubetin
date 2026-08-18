package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// dashboardPane identifies a scrollable region of the dashboard. The
// status banner isn't one: it never scrolls and there's nothing to do
// to it, so Tab skips straight past it.
type dashboardPane int

const (
	dashPaneContainers dashboardPane = iota
	dashPaneEvents
	dashPaneLogs
	dashPaneCount
)

// dashboardTarget is one entry on the drill-down stack.
type dashboardTarget struct {
	Ref cluster.DescribeRef
	UID types.UID
}

type dashboardState struct {
	open  bool
	stack []dashboardTarget
	focus dashboardPane

	// scroll is per-pane. Containers and events count rows from the
	// top; logs count from the *bottom* so 0 means "following the
	// tail", matching logsState.scroll.
	scroll [dashPaneCount]int

	// canvas is the stacked layout's whole-body offset, used instead
	// of the per-pane offsets when the panes are drawn in one column.
	canvas int

	// containers of the pod being streamed, plus which one we're on.
	// Held here rather than read off the row each time so `c` cycles
	// deterministically while the informer churns.
	containers []string
	containerI int
}

func (s dashboardState) target() (dashboardTarget, bool) {
	if len(s.stack) == 0 {
		return dashboardTarget{}, false
	}
	return s.stack[len(s.stack)-1], true
}

// openDashboard pushes a target and starts its log stream. Multi-
// container pods stream container[0] rather than interrupting with the
// picker modal — `c` cycles once the dashboard is up.
func (m Model) openDashboard(ref cluster.DescribeRef, uid types.UID) (tea.Model, tea.Cmd) {
	m.dashboard.open = true
	m.dashboard.stack = append(m.dashboard.stack, dashboardTarget{Ref: ref, UID: uid})
	m.dashboard.focus = dashPaneLogs
	m.dashboard.scroll = [dashPaneCount]int{}
	m.dashboard.canvas = 0

	containers := []string{}
	if r, ok := m.pods[uid]; ok {
		containers = r.Containers
	}
	m.dashboard.containers = containers
	m.dashboard.containerI = 0

	return m, m.startDashboardLogs(ref)
}

// startDashboardLogs begins (or restarts) the stream for the current
// target and container. Returns nil when the user is known to lack
// pods/log — firing a request we know the apiserver will refuse just
// trades a live pane for an error pane.
func (m *Model) startDashboardLogs(ref cluster.DescribeRef) tea.Cmd {
	if st, ok := m.permissions[cluster.PermissionKey(m.WatchedContext, "get", "", "pods/log", ref.Namespace)]; ok && !st.Allowed {
		m.logs.err = "no permission to read pod logs (get pods/log)"
		m.logs.streaming = false
		return nil
	}
	container := ""
	if m.dashboard.containerI < len(m.dashboard.containers) {
		container = m.dashboard.containers[m.dashboard.containerI]
	}
	return m.beginLogStream(ref, container)
}

// closeDashboard tears down the whole stack and stops the stream.
func (m Model) closeDashboard() (tea.Model, tea.Cmd) {
	m.dashboard = dashboardState{}
	return m, m.stopDashboardLogs()
}

// popDashboard returns to the previous target, or closes when this was
// the last one.
func (m Model) popDashboard() (tea.Model, tea.Cmd) {
	if len(m.dashboard.stack) <= 1 {
		return m.closeDashboard()
	}
	m.dashboard.stack = m.dashboard.stack[:len(m.dashboard.stack)-1]
	m.dashboard.scroll = [dashPaneCount]int{}
	m.dashboard.canvas = 0
	t, _ := m.dashboard.target()
	m.dashboard.containers = nil
	m.dashboard.containerI = 0
	if r, ok := m.pods[t.UID]; ok {
		m.dashboard.containers = r.Containers
	}
	return m, m.startDashboardLogs(t.Ref)
}

func (m *Model) stopDashboardLogs() tea.Cmd {
	m.logs.streaming = false
	if m.OnLogsStop != nil {
		m.OnLogsStop()
	}
	return nil
}

// dashCanvasSize mirrors View()'s body arithmetic so the key handler
// and the renderer agree on which layout is active. Same render-and-
// count approach overviewLineCount uses — the header and footer both
// vary in height, and a formula drifts the moment either changes.
func (m Model) dashCanvasSize() (int, int) {
	h := m.height - lipgloss.Height(m.renderHeader()) - lipgloss.Height(m.renderFooter())
	if h < 1 {
		h = 1
	}
	return m.width, h
}

// dashLayoutNow resolves the current geometry, or false when there's
// no target to render.
func (m Model) dashLayoutNow() (dashLayout, podRow, bool) {
	t, ok := m.dashboard.target()
	if !ok {
		return dashLayout{}, podRow{}, false
	}
	r, ok := m.pods[t.UID]
	if !ok {
		return dashLayout{}, podRow{}, false
	}
	w, h := m.dashCanvasSize()
	return dashLayoutFor(w, h, dashPodStatusHeight(r), m.Theme), r, true
}

func (m Model) handleDashboardKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "esc", "q":
		return m.popDashboard()
	case "tab":
		m.dashboard.focus = (m.dashboard.focus + 1) % dashPaneCount
	case "shift+tab":
		m.dashboard.focus = (m.dashboard.focus + dashPaneCount - 1) % dashPaneCount
	case "j", "down":
		m.scrollDashboard(+1)
	case "k", "up":
		m.scrollDashboard(-1)
	case "g", "home":
		m.jumpDashboard(true)
	case "G", "end":
		m.jumpDashboard(false)
	case "c":
		return m.cycleDashboardContainer()
	case "l":
		// Same buffer, full canvas — no refetch, the stream keeps
		// running underneath.
		if m.logs.streaming {
			m.logs.open = true
		}
	case "d":
		if t, ok := m.dashboard.target(); ok {
			return m.openDescribeFor(t.Ref)
		}
	case "enter":
		return m.openActionMenu()
	}
	return m, nil
}

// scrollDashboard moves the active scroll offset by delta, clamped to
// the content. Which offset is "active" depends on the layout: the
// stacked column scrolls as one canvas, the wide frame scrolls the
// focused pane.
func (m *Model) scrollDashboard(delta int) {
	lay, r, ok := m.dashLayoutNow()
	if !ok {
		return
	}
	if !lay.wide {
		_, h := m.dashCanvasSize()
		m.canvasScrollTo(m.dashboard.canvas+delta, r, h)
		return
	}
	// Logs count from the bottom, so "down" means towards the tail.
	if m.dashboard.focus == dashPaneLogs {
		delta = -delta
	}
	max := m.dashScrollMax(m.dashboard.focus, lay, r)
	v := m.dashboard.scroll[m.dashboard.focus] + delta
	if v < 0 {
		v = 0
	}
	if v > max {
		v = max
	}
	m.dashboard.scroll[m.dashboard.focus] = v
}

func (m *Model) jumpDashboard(top bool) {
	lay, r, ok := m.dashLayoutNow()
	if !ok {
		return
	}
	if !lay.wide {
		_, h := m.dashCanvasSize()
		if top {
			m.dashboard.canvas = 0
		} else {
			m.canvasScrollTo(1<<30, r, h)
		}
		return
	}
	max := m.dashScrollMax(m.dashboard.focus, lay, r)
	// For logs, offset counts from the tail: g is the oldest line we
	// still hold, G is back to following.
	switch {
	case m.dashboard.focus == dashPaneLogs && top:
		m.dashboard.scroll[dashPaneLogs] = max
	case m.dashboard.focus == dashPaneLogs:
		m.dashboard.scroll[dashPaneLogs] = 0
	case top:
		m.dashboard.scroll[m.dashboard.focus] = 0
	default:
		m.dashboard.scroll[m.dashboard.focus] = max
	}
}

func (m *Model) canvasScrollTo(want int, r podRow, h int) {
	_, clamped := scrollWindow(m.stackedBody(r, m.width), want, h)
	m.dashboard.canvas = clamped
}

// dashScrollMax is the largest useful offset for a pane: render its
// content and count, so the bound can't drift from what's drawn.
func (m Model) dashScrollMax(p dashboardPane, lay dashLayout, r podRow) int {
	var content string
	var h int
	switch p {
	case dashPaneContainers:
		// Natural height (h=0): asking for a huge height would make
		// clampCanvas pad the content out to that many rows.
		content = m.renderDashContainers(r, lay.left.w, 0, 0)
		h = lay.left.h
	case dashPaneEvents:
		t, _ := m.dashboard.target()
		content = m.renderDashEvents(m.dashEventsFor(t.Ref), lay.right.w, 0, 0)
		h = lay.right.h
	case dashPaneLogs:
		max := len(m.logs.lines) - lay.logs.h
		if max < 0 {
			return 0
		}
		return max
	}
	if max := lineCount(content) - h; max > 0 {
		return max
	}
	return 0
}

// cycleDashboardContainer moves to the next container and restarts the
// stream against it.
func (m Model) cycleDashboardContainer() (tea.Model, tea.Cmd) {
	if len(m.dashboard.containers) < 2 {
		return m, nil
	}
	t, ok := m.dashboard.target()
	if !ok {
		return m, nil
	}
	m.dashboard.containerI = (m.dashboard.containerI + 1) % len(m.dashboard.containers)
	m.dashboard.scroll[dashPaneLogs] = 0
	cmd := m.startDashboardLogs(t.Ref)
	return m, cmd
}

// renderDashboard is the body renderer. It owns the full canvas — the
// sidebar is hidden, the same way the fleet overview takes the row.
func (m Model) renderDashboard(height, width int) string {
	t, ok := m.dashboard.target()
	if !ok {
		return clampCanvas("", width, height)
	}
	r, ok := m.pods[t.UID]
	if !ok {
		msg := m.Theme.StatusWrn.Render("  "+t.Ref.Kind+"/"+t.Ref.Name+
			" is no longer present in this cluster") + "\n\n" +
			m.Theme.Dim.Render("  Esc to go back")
		return clampCanvas(msg, width, height)
	}

	lay := dashLayoutFor(width, height, dashPodStatusHeight(r), m.Theme)
	if lay.wide {
		return m.renderDashboardWide(r, t, lay, width, height)
	}
	body, _ := scrollWindow(m.stackedBody(r, width), m.dashboard.canvas, height)
	return clampCanvas(body, width, height)
}

func (m Model) renderDashboardWide(r podRow, t dashboardTarget, lay dashLayout, w, h int) string {
	canvas := lay.frame

	canvas = dashPaneTitle(canvas, m.dashTitle(t), 2, 0)
	canvas = dashPaneTitle(canvas, m.paneLabel("CONTAINERS", dashPaneContainers), 2, lay.left.y-1)
	canvas = dashPaneTitle(canvas, m.paneLabel("EVENTS", dashPaneEvents), lay.right.x+1, lay.right.y-1)
	canvas = dashPaneTitle(canvas, m.logsPaneLabel(), 2, lay.logs.y-1)

	canvas = splicePane(canvas, m.renderDashPodStatus(r, lay.status.w, lay.status.h), lay.status)
	canvas = splicePane(canvas, m.renderDashContainers(r, lay.left.w, lay.left.h,
		m.dashboard.scroll[dashPaneContainers]), lay.left)
	canvas = splicePane(canvas, m.renderDashEvents(m.dashEventsFor(t.Ref), lay.right.w, lay.right.h,
		m.dashboard.scroll[dashPaneEvents]), lay.right)
	canvas = splicePane(canvas, m.renderDashLogs(lay.logs.w, lay.logs.h,
		m.dashboard.scroll[dashPaneLogs]), lay.logs)

	if bottom := m.dashBottomLabel(); bottom != "" {
		canvas = dashPaneTitle(canvas, bottom, 2, h-1)
	}
	return clampCanvas(canvas, w, h)
}

// stackedBody builds the single-column form at full natural height.
// The caller windows it — scrolling one tall string keeps every pane
// reachable on a terminal that can't show them all at once.
func (m Model) stackedBody(r podRow, w int) string {
	th := m.Theme
	t, _ := m.dashboard.target()
	// lipgloss Width(w-2) plus the two border columns renders at w, so
	// the usable content width is w-2.
	inner := w - 2
	if inner < 1 {
		inner = 1
	}

	// Each pane is rendered at natural height first, then clamped to
	// what it actually needs (bounded), so a container's continuation
	// row or a short event list sizes its own box instead of being
	// clipped by a guessed row count.
	sized := func(content string, cap int) string {
		h := dashStackPaneHeight(lineCount(content), cap)
		return clampCanvas(content, inner, h)
	}

	events := m.dashEventsFor(t.Ref)
	parts := []string{
		dashStackedBox(m.dashTitle(t),
			m.renderDashPodStatus(r, inner, dashPodStatusHeight(r)), w, false, th),
		dashStackedBox(m.paneLabel("CONTAINERS", dashPaneContainers),
			sized(m.renderDashContainers(r, inner, 0, 0), dashStackContainersMax),
			w, m.dashboard.focus == dashPaneContainers, th),
		dashStackedBox(m.paneLabel("EVENTS", dashPaneEvents),
			sized(m.renderDashEvents(events, inner, 0, 0), dashStackEventsMax),
			w, m.dashboard.focus == dashPaneEvents, th),
		dashStackedBox(m.logsPaneLabel(),
			m.renderDashLogs(inner, dashStackLogHeight, m.dashboard.scroll[dashPaneLogs]),
			w, m.dashboard.focus == dashPaneLogs, th),
	}
	return strings.Join(parts, "\n")
}

// dashStackLogHeight is how many log rows the stacked layout shows.
// Fixed rather than proportional: the column is scrolled anyway, and a
// log pane that grows with the buffer would push everything else out
// of reach.
const (
	dashStackLogHeight     = 10
	dashStackContainersMax = 8
	dashStackEventsMax     = 8
)

func dashStackPaneHeight(n, cap int) int {
	if n < 1 {
		return 1
	}
	if n > cap {
		return cap
	}
	return n
}

func (m Model) dashTitle(t dashboardTarget) string {
	name := t.Ref.Name
	if t.Ref.Namespace != "" {
		name = t.Ref.Namespace + "/" + name
	}
	return m.Theme.Title.Render(name) + m.Theme.Dim.Render("  "+m.WatchedContext)
}

// paneLabel styles a pane title, accenting the focused one so the eye
// finds the pane j/k is driving.
func (m Model) paneLabel(label string, p dashboardPane) string {
	if m.dashboard.focus == p {
		return m.Theme.Title.Render(label)
	}
	return m.Theme.Header.Render(label)
}

// logsPaneLabel appends the stream indicator and container name, which
// is where the user looks to tell "quiet pod" from "stream is dead".
func (m Model) logsPaneLabel() string {
	label := m.paneLabel("LOGS", dashPaneLogs)
	label += m.Theme.Dim.Render(" ─ ") + m.logStatusIndicator()
	if c := m.logs.container; c != "" {
		label += m.Theme.Dim.Render(" ─ " + c)
		if len(m.dashboard.containers) > 1 {
			label += m.Theme.Dim.Render(" (c)")
		}
	}
	return label
}

func (m Model) dashBottomLabel() string {
	if len(m.dashboard.stack) > 1 {
		return m.Theme.Dim.Render("esc: back")
	}
	return ""
}

// openDescribeFor opens the describe overlay for an explicit ref,
// bypassing refForCursor — inside the dashboard the subject is the
// target on the stack, which may be a pod the table cursor isn't on.
func (m Model) openDescribeFor(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	if m.OnDescribe == nil {
		return m, nil
	}
	m.describe.open = true
	m.describe.loading = true
	m.describe.revealed = false
	m.describe.result = cluster.DescribeResult{}
	m.describe.scroll = 0
	cb := m.OnDescribe
	focused := m.WatchedContext
	req := DescribeRequestMsg{Ref: ref}
	return m, func() tea.Msg { return cb(req, focused) }
}

// openDashboardForCursor opens the dashboard for the selected row.
// Pods only: the Deployment form needs its own replica/owned-pod panes
// and lands separately.
func (m Model) openDashboardForCursor() (tea.Model, tea.Cmd) {
	if m.view != ViewPods {
		return m, nil
	}
	ref, ok := m.refForCursor()
	if !ok {
		return m, nil
	}
	return m.openDashboard(ref, m.cursor)
}
