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
	dashPaneMain dashboardPane = iota // containers (Pod) / replicas (Deployment)
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

	// scroll is per-pane, counting rows from the top. The logs pane is
	// deliberately absent: its offset lives in logsState next to
	// follow, so the buffer's pin-on-new-lines logic and the paused
	// indicator behave exactly as they do in the full-screen viewer.
	scroll [dashPaneCount]int

	// logRef is the pod actually being streamed. For a Pod target it
	// equals the target; for a Deployment it's one chosen replica, so
	// it has to be tracked separately.
	logRef cluster.DescribeRef

	// containers of the pod being streamed, plus which one we're on.
	// Held here rather than read off the row each time so `c` cycles
	// deterministically while the informer churns.
	containers []string
	containerI int

	// podCursor indexes the Deployment view's owned-pod list. It's a
	// cursor rather than a scroll offset because the row is a drill-in
	// target, not just something to read.
	podCursor int
}

// dashSubject is what the dashboard is currently showing. Panes,
// status height and scroll bounds all switch on Kind rather than each
// re-resolving the target off the stack.
type dashSubject struct {
	Kind   string
	Pod    podRow
	Deploy deploymentRow
	Pods   []podRow // owned pods; Deployment only
}

func (s dashSubject) isDeploy() bool { return s.Kind == "Deployment" }

// statusHeight is how many rows the banner needs for this subject.
func (s dashSubject) statusHeight() int {
	if s.isDeploy() {
		return dashDeployStatusHeight(s.Deploy)
	}
	return dashPodStatusHeight(s.Pod)
}

// mainLabel names the left pane for this subject.
func (s dashSubject) mainLabel() string {
	if s.isDeploy() {
		return "PODS"
	}
	return "CONTAINERS"
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
	t := dashboardTarget{Ref: ref, UID: uid}
	m.dashboard.open = true
	m.dashboard.stack = append(m.dashboard.stack, t)
	m.dashboard.focus = dashPaneLogs
	m.dashboard.scroll = [dashPaneCount]int{}
	m.dashboard.podCursor = 0
	m.prepareLogTarget(t)
	// Assigned before the return: startDashboardLogs mutates m and the
	// Go spec doesn't order the non-call m operand against the call —
	// the cycleFocus gotcha.
	cmd := m.startDashboardLogs()
	return m, cmd
}

// prepareLogTarget picks which pod the log pane streams. A Deployment
// streams its newest Running replica — the same choice
// openLogsForDeployment makes, so a recent rollout surfaces over older
// replicas. Drilling into a specific pod is how you read a different
// one; selection in the PODS pane deliberately does not restart the
// stream, because scrolling a list shouldn't tear down a connection.
func (m *Model) prepareLogTarget(t dashboardTarget) {
	m.dashboard.containers = nil
	m.dashboard.containerI = 0
	m.dashboard.logRef = cluster.DescribeRef{}

	if t.Ref.Kind == "Deployment" {
		d, ok := m.deployments[t.UID]
		if !ok {
			return
		}
		p, ok := newestRunningPod(m.deployOwnedPods(d))
		if !ok {
			return
		}
		m.dashboard.logRef = podRefFor(p)
		m.dashboard.containers = p.Containers
		return
	}
	if r, ok := m.pods[t.UID]; ok {
		m.dashboard.containers = r.Containers
	}
	m.dashboard.logRef = t.Ref
}

// newestRunningPod prefers the most recently created Running replica,
// falling back to the newest pod of any phase so a fully-broken
// deployment still shows something rather than an empty pane.
func newestRunningPod(pods []podRow) (podRow, bool) {
	var running, any podRow
	var haveRunning, haveAny bool
	for _, p := range pods {
		if !haveAny || p.CreatedAt.After(any.CreatedAt) {
			any, haveAny = p, true
		}
		if string(p.Phase) != "Running" {
			continue
		}
		if !haveRunning || p.CreatedAt.After(running.CreatedAt) {
			running, haveRunning = p, true
		}
	}
	if haveRunning {
		return running, true
	}
	return any, haveAny
}

func podRefFor(p podRow) cluster.DescribeRef {
	return cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: p.Namespace, Name: p.Name,
	}
}

// startDashboardLogs begins (or restarts) the stream for the current
// target and container. Returns nil when the user is known to lack
// pods/log — firing a request we know the apiserver will refuse just
// trades a live pane for an error pane.
func (m *Model) startDashboardLogs() tea.Cmd {
	ref := m.dashboard.logRef
	if ref.Name == "" {
		m.logs.lines = nil
		m.logs.err = "no pod available to stream logs from"
		m.logs.streaming = false
		return nil
	}
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
	cmd := m.stopDashboardLogs()
	return m, cmd
}

// popDashboard returns to the previous target, or closes when this was
// the last one.
func (m Model) popDashboard() (tea.Model, tea.Cmd) {
	if len(m.dashboard.stack) <= 1 {
		return m.closeDashboard()
	}
	m.dashboard.stack = m.dashboard.stack[:len(m.dashboard.stack)-1]
	m.dashboard.scroll = [dashPaneCount]int{}
	t, _ := m.dashboard.target()
	m.prepareLogTarget(t)
	cmd := m.startDashboardLogs()
	return m, cmd
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
// count approach — the header and footer both
// vary in height, and a formula drifts the moment either changes.
func (m Model) dashCanvasSize() (int, int) {
	h := m.height - lipgloss.Height(m.renderHeader()) - lipgloss.Height(m.renderFooter())
	if h < 1 {
		h = 1
	}
	return m.width, h
}

// dashSubjectNow resolves the target into renderable state, or false
// when the object has gone from the informer cache.
func (m Model) dashSubjectNow() (dashSubject, bool) {
	t, ok := m.dashboard.target()
	if !ok {
		return dashSubject{}, false
	}
	if t.Ref.Kind == "Deployment" {
		d, ok := m.deployments[t.UID]
		if !ok {
			return dashSubject{}, false
		}
		return dashSubject{Kind: "Deployment", Deploy: d, Pods: m.deployOwnedPods(d)}, true
	}
	r, ok := m.pods[t.UID]
	if !ok {
		return dashSubject{}, false
	}
	return dashSubject{Kind: "Pod", Pod: r}, true
}

// dashLayoutNow resolves the current geometry, or false when there's
// no target to render.
func (m Model) dashLayoutNow() (dashLayout, dashSubject, bool) {
	s, ok := m.dashSubjectNow()
	if !ok {
		return dashLayout{}, dashSubject{}, false
	}
	w, h := m.dashCanvasSize()
	return dashLayoutFor(w, h, s.statusHeight(), m.Theme), s, true
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
	case "f":
		m.logs.follow = !m.logs.follow
		if m.logs.follow {
			m.logs.scroll = 0
		}
	case "i":
		return m.drillIntoSelectedPod()
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
		ref, uid, ok := m.dashActionTarget()
		if !ok {
			return m, nil
		}
		return m.openActionMenuFor(ref, uid)
	}
	return m, nil
}

// dashActionTarget is what Enter acts on: the replica under the PODS-
// pane cursor when that pane is focused, otherwise the dashboard's own
// target. The table cursor — which is what the action menu resolves
// from everywhere else — is the wrong source here. It still points at
// the row the dashboard was opened from, so selecting a replica and
// pressing Enter offered the parent Deployment's Scale and Restart
// instead of the pod's own actions, and drilling into a pod with `i`
// had the same problem one level down.
func (m Model) dashActionTarget() (cluster.DescribeRef, types.UID, bool) {
	t, ok := m.dashboard.target()
	if !ok {
		return cluster.DescribeRef{}, "", false
	}
	// No subject means the object has gone from the informer cache and
	// the pane already says so. Offering Delete on a name that may
	// since belong to a replacement object is worse than doing
	// nothing, so Enter is inert there.
	sub, ok := m.dashSubjectNow()
	if !ok {
		return cluster.DescribeRef{}, "", false
	}
	if sub.isDeploy() && m.dashboard.focus == dashPaneMain {
		if i := m.dashboard.podCursor; i >= 0 && i < len(sub.Pods) {
			p := sub.Pods[i]
			return podRefFor(p), p.UID, true
		}
	}
	return t.Ref, t.UID, true
}

// syncDashboardLogTarget re-points the dashboard at a stream started
// from outside it — the Logs action on a replica picked in the PODS
// pane, or the container picker. Closing the full-screen viewer over
// an open dashboard deliberately keeps the stream alive and drops back
// to the log pane, so without this the pane would render one pod while
// logRef named another, and `c` would silently cycle the containers of
// the pod the dashboard originally chose and switch back to it.
func (m *Model) syncDashboardLogTarget(ref cluster.DescribeRef, container string) {
	if !m.dashboard.open {
		return
	}
	m.dashboard.logRef = ref
	m.dashboard.containers = m.containersFor(ref)
	m.dashboard.containerI = 0
	for i, c := range m.dashboard.containers {
		if c == container {
			m.dashboard.containerI = i
			break
		}
	}
}

// focusedPaneSize is the content width and height of the pane j/k is
// driving, in whichever layout is active. Both layouts resolve their
// geometry through here, so the scroll bounds and what is actually
// drawn cannot disagree about how big a pane is.
func (m Model) focusedPaneSize(lay dashLayout, sub dashSubject) (int, int) {
	if lay.wide {
		switch m.dashboard.focus {
		case dashPaneMain:
			return lay.left.w, lay.left.h
		case dashPaneEvents:
			return lay.right.w, lay.right.h
		}
		return lay.logs.w, lay.logs.h
	}
	cw, ch := m.dashCanvasSize()
	inner := cw - 2
	if inner < 1 {
		inner = 1
	}
	_, mainH, eventsH, logsH := m.stackedPaneHeights(sub, cw, ch)
	switch m.dashboard.focus {
	case dashPaneMain:
		return inner, mainH
	case dashPaneEvents:
		return inner, eventsH
	}
	return inner, logsH
}

// stackedCanvasOffset is the row the stacked column must start at for
// the focused pane to be on screen.
//
// Derived per render rather than stored. The pane heights come from
// live cluster data — a burst of events grows the events pane from one
// row to eight — so any remembered offset goes stale the moment the
// cluster changes, not just when the user presses something. It used
// to be a field refreshed on open, Tab, pop and resize, and an event
// arriving between those pushed the focused pane below the fold with
// no way back except cycling focus.
func (m Model) stackedCanvasOffset(g stackedGeom, h int) int {
	body := g.body
	if lipgloss.Height(body) <= h {
		return 0
	}
	top := m.stackedPaneTop(m.dashboard.focus, g.status, g.main, g.events)

	// Show the pane from its top edge. Aligning its bottom instead
	// would jump the column further than the eye can follow when the
	// pane is tall.
	_, clamped := scrollWindow(body, top, h)
	return clamped
}

// scrollDashboard moves the focused pane's scroll offset by delta,
// clamped to that pane's content. Focus means the same thing in both
// layouts: stacked mode used to route j/k at the whole canvas instead,
// which left Tab moving a highlight that changed nothing.
func (m *Model) scrollDashboard(delta int) {
	lay, sub, ok := m.dashLayoutNow()
	if !ok {
		return
	}
	// The owned-pod list is a selection, so j/k moves the cursor there
	// in both layouts — scrolling past a row you meant to drill into
	// would make the pane useless.
	if sub.isDeploy() && m.dashboard.focus == dashPaneMain {
		m.movePodCursor(delta, len(sub.Pods))
		return
	}
	pw, ph := m.focusedPaneSize(lay, sub)
	// Logs share the viewer's offset: j/k move it and pause/resume
	// follow, so a scrolled-back pane holds its position as new lines
	// arrive instead of sliding with the tail.
	if m.dashboard.focus == dashPaneLogs {
		m.scrollLogsBy(-delta, ph)
		return
	}
	max := m.dashScrollMax(m.dashboard.focus, sub, pw, ph)
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
	lay, sub, ok := m.dashLayoutNow()
	if !ok {
		return
	}
	if sub.isDeploy() && m.dashboard.focus == dashPaneMain {
		if top {
			m.dashboard.podCursor = 0
		} else if n := len(sub.Pods); n > 0 {
			m.dashboard.podCursor = n - 1
		}
		return
	}
	pw, ph := m.focusedPaneSize(lay, sub)
	// For logs, offset counts from the tail: g is the oldest line we
	// still hold (paused), G is back to following.
	if m.dashboard.focus == dashPaneLogs {
		if top {
			m.logs.scroll = clampLogsScrollTo(len(m.logs.lines), len(m.logs.lines), ph)
			m.logs.follow = false
		} else {
			m.logs.scroll = 0
			m.logs.follow = true
		}
		return
	}
	max := m.dashScrollMax(m.dashboard.focus, sub, pw, ph)
	switch {
	case top:
		m.dashboard.scroll[m.dashboard.focus] = 0
	default:
		m.dashboard.scroll[m.dashboard.focus] = max
	}
}

// movePodCursor steps the owned-pod selection, clamped to the list.
func (m *Model) movePodCursor(delta, n int) {
	if n == 0 {
		m.dashboard.podCursor = 0
		return
	}
	v := m.dashboard.podCursor + delta
	if v < 0 {
		v = 0
	}
	if v > n-1 {
		v = n - 1
	}
	m.dashboard.podCursor = v
}

// dashScrollMax is the largest useful offset for a pane: render its
// content and count, so the bound can't drift from what's drawn.
func (m Model) dashScrollMax(p dashboardPane, sub dashSubject, w, h int) int {
	var content string
	switch p {
	case dashPaneMain:
		// Natural height (h=0): asking for a huge height would make
		// clampCanvas pad the content out to that many rows.
		content = m.renderDashMain(sub, w, 0)
	case dashPaneEvents:
		// Counted, not rendered: formatting every row to measure it
		// is what this pane's windowing exists to avoid.
		if max := dashEventLineCount(m.dashEventRows(sub)) - h; max > 0 {
			return max
		}
		return 0
	case dashPaneLogs:
		// Logs are bounded by clampLogsScrollTo against logsState, not
		// by this pane's offset array.
		return 0
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
	if _, ok := m.dashboard.target(); !ok {
		return m, nil
	}
	m.dashboard.containerI = (m.dashboard.containerI + 1) % len(m.dashboard.containers)
	cmd := m.startDashboardLogs()
	return m, cmd
}

// renderDashboard is the body renderer. It owns the full canvas — the
// sidebar is hidden, the same way the fleet overview takes the row.
func (m Model) renderDashboard(height, width int) string {
	t, ok := m.dashboard.target()
	if !ok {
		return clampCanvas("", width, height)
	}
	sub, ok := m.dashSubjectNow()
	if !ok {
		msg := m.Theme.StatusWrn.Render("  "+t.Ref.Kind+"/"+t.Ref.Name+
			" is no longer present in this cluster") + "\n\n" +
			m.Theme.Dim.Render("  Esc to go back")
		return clampCanvas(msg, width, height)
	}

	lay := dashLayoutFor(width, height, sub.statusHeight(), m.Theme)
	if lay.wide {
		return m.renderDashboardWide(sub, t, lay, width, height)
	}
	g := m.stackedLayout(sub, width, height)
	body, _ := scrollWindow(g.body, m.stackedCanvasOffset(g, height), height)
	return clampCanvas(body, width, height)
}

// renderDashStatus / renderDashMain / renderDashEventsFor dispatch the
// three subject-dependent panes. The logs pane is subject-independent:
// it renders one stream either way.
func (m Model) renderDashStatus(sub dashSubject, w, h int) string {
	if sub.isDeploy() {
		return m.renderDashDeployStatus(sub.Deploy, w, h)
	}
	return m.renderDashPodStatus(sub.Pod, w, h)
}

func (m Model) renderDashMain(sub dashSubject, w, h int) string {
	if sub.isDeploy() {
		return m.renderDashPods(sub.Pods, w, h, m.dashboard.podCursor)
	}
	return m.renderDashContainers(sub.Pod, w, h, m.dashboard.scroll[dashPaneMain])
}

// dashEventRows is the scoped, sorted event set for the current
// subject. Split out so the layout can count rows without formatting
// them, and so a render that follows reuses the same scope pass.
func (m Model) dashEventRows(sub dashSubject) []eventRow {
	if sub.isDeploy() {
		return m.dashDeployEvents(sub.Deploy, sub.Pods)
	}
	t, _ := m.dashboard.target()
	return m.dashEventsFor(t.Ref)
}

func (m Model) renderDashEventsFor(sub dashSubject, w, h, scroll int) string {
	return m.renderDashEvents(m.dashEventRows(sub), w, h, scroll, sub.isDeploy())
}

func (m Model) renderDashboardWide(sub dashSubject, t dashboardTarget, lay dashLayout, w, h int) string {
	canvas := lay.frame

	canvas = dashPaneTitle(canvas, m.dashTitle(t), 2, 0)
	canvas = dashPaneTitle(canvas, m.paneLabel(sub.mainLabel(), dashPaneMain), 2, lay.left.y-1)
	canvas = dashPaneTitle(canvas, m.paneLabel("EVENTS", dashPaneEvents), lay.right.x+1, lay.right.y-1)
	canvas = dashPaneTitle(canvas, m.logsPaneLabel(), 2, lay.logs.y-1)

	canvas = splicePane(canvas, m.renderDashStatus(sub, lay.status.w, lay.status.h), lay.status)
	canvas = splicePane(canvas, m.renderDashMain(sub, lay.left.w, lay.left.h), lay.left)
	canvas = splicePane(canvas, m.renderDashEventsFor(sub, lay.right.w, lay.right.h,
		m.dashboard.scroll[dashPaneEvents]), lay.right)
	canvas = splicePane(canvas, m.renderDashLogs(lay.logs.w, lay.logs.h), lay.logs)

	if bottom := m.dashBottomLabel(sub); bottom != "" {
		canvas = dashPaneTitle(canvas, bottom, 2, h-1)
	}
	return clampCanvas(canvas, w, h)
}

// stackedPaneHeights resolves each stacked pane's interior height.
// Status and the two middle panes size to their content (bounded);
// logs take whatever is left. Shared by the renderer and the scroll
// logic so they can't disagree about how tall a pane is.
func (m Model) stackedPaneHeights(sub dashSubject, w, h int) (status, main, events, logs int) {
	inner := stackedInnerWidth(w)
	return resolveStackedHeights(sub.statusHeight(),
		lineCount(m.renderDashMain(sub, inner, 0)),
		dashEventLineCount(m.dashEventRows(sub)), h)
}

// resolveStackedHeights is the one place pane heights are decided,
// taking line counts rather than rendering anything. Keeping it pure
// lets the render path measure content it has already produced instead
// of producing it a second time.
func resolveStackedHeights(statusH, mainLines, eventLines, h int) (status, main, events, logs int) {
	status = statusH
	main = dashStackPaneHeight(mainLines, dashStackContainersMax)
	events = dashStackPaneHeight(eventLines, dashStackEventsMax)
	// Each box spends two rows on its borders.
	logs = h - (status + 2) - (main + 2) - (events + 2) - 2
	if logs < dashStackLogMin {
		logs = dashStackLogMin
	}
	return status, main, events, logs
}

func stackedInnerWidth(w int) int {
	inner := w - 2
	if inner < 1 {
		inner = 1
	}
	return inner
}

// stackedPaneTop is the body row each stacked pane's box starts on,
// used to scroll the canvas far enough to reveal it.
func (m Model) stackedPaneTop(p dashboardPane, statusH, mainH, eventsH int) int {
	switch p {
	case dashPaneMain:
		return statusH + 2
	case dashPaneEvents:
		return statusH + 2 + mainH + 2
	case dashPaneLogs:
		return statusH + 2 + mainH + 2 + eventsH + 2
	}
	return 0
}

// stackedGeom is one render's resolved stacked layout: the assembled
// column plus each pane's interior height.
type stackedGeom struct {
	body                       string
	status, main, events, logs int
}

// stackedLayout builds the single-column form and reports the geometry
// it used.
//
// One pass on purpose. The variable panes are rendered once at natural
// height, measured, then windowed down — previously they were rendered
// to measure and rendered again to display, and the canvas offset
// derived the whole thing a third time. Each of those passes filters
// and sorts the cluster-wide event map, and informer updates trigger a
// redraw, so a busy cluster paid it on every event.
func (m Model) stackedLayout(sub dashSubject, w, h int) stackedGeom {
	th := m.Theme
	t, _ := m.dashboard.target()
	inner := stackedInnerWidth(w)

	naturalMain := m.renderDashMain(sub, inner, 0)
	// Scoped once, then counted and formatted from the same slice: the
	// events pane formats only its visible rows, so measuring it by
	// rendering it at natural height would cost more than the render.
	eventRows := m.dashEventRows(sub)

	g := stackedGeom{}
	g.status, g.main, g.events, g.logs = resolveStackedHeights(
		sub.statusHeight(), lineCount(naturalMain), dashEventLineCount(eventRows), h)

	// Rendered again at the resolved height rather than trimmed from
	// naturalMain: the containers pane pins its header above the
	// scroll window and the owned-pod list windows around its cursor,
	// neither of which trimming can reproduce. Both renders are cheap
	// — the pane the one-pass discipline exists for is events.
	sizedMain := m.renderDashMain(sub, inner, g.main)

	g.body = strings.Join([]string{
		dashStackedBox(m.dashTitle(t),
			m.renderDashStatus(sub, inner, g.status), w, false, th),
		dashStackedBox(m.paneLabel(sub.mainLabel(), dashPaneMain),
			sizedMain, w, m.dashboard.focus == dashPaneMain, th),
		dashStackedBox(m.paneLabel("EVENTS", dashPaneEvents),
			m.renderDashEvents(eventRows, inner, g.events,
				m.dashboard.scroll[dashPaneEvents], sub.isDeploy()),
			w, m.dashboard.focus == dashPaneEvents, th),
		dashStackedBox(m.logsPaneLabel(),
			m.renderDashLogs(inner, g.logs),
			w, m.dashboard.focus == dashPaneLogs, th),
	}, "\n")
	return g
}

// stackedBody is stackedLayout for callers that only want the column.
func (m Model) stackedBody(sub dashSubject, w, h int) string {
	return m.stackedLayout(sub, w, h).body
}

// dashStackLogMin is the floor for the stacked log pane. Above it the
// pane grows to fill the window exactly; below it the body overflows
// and the canvas scrolls.
//
// Deliberately small. A larger floor overshoots whenever the leftover
// space is between the floor and zero — the pane then pushes its own
// bottom border off the canvas and forces a scroll on a window that
// was nearly tall enough. Five rows is thin but readable, and it means
// the column fits exactly far more often than it scrolls.
const (
	dashStackLogMin        = 5
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

func (m Model) dashBottomLabel(sub dashSubject) string {
	hints := []string{}
	if sub.isDeploy() && len(sub.Pods) > 0 {
		hints = append(hints, "i: open pod")
	}
	if len(m.dashboard.stack) > 1 {
		hints = append(hints, "esc: back")
	}
	if len(hints) == 0 {
		return ""
	}
	return m.Theme.Dim.Render(strings.Join(hints, "  ·  "))
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

// drillIntoSelectedPod pushes the pod under the PODS-pane cursor onto
// the stack, so Esc returns to the deployment.
func (m Model) drillIntoSelectedPod() (tea.Model, tea.Cmd) {
	sub, ok := m.dashSubjectNow()
	if !ok || !sub.isDeploy() || len(sub.Pods) == 0 {
		return m, nil
	}
	i := m.dashboard.podCursor
	if i < 0 || i >= len(sub.Pods) {
		return m, nil
	}
	p := sub.Pods[i]
	return m.openDashboard(podRefFor(p), p.UID)
}

// openDashboardForCursor opens the dashboard for the selected row.
func (m Model) openDashboardForCursor() (tea.Model, tea.Cmd) {
	if m.view != ViewPods && m.view != ViewDeployments {
		return m, nil
	}
	ref, ok := m.refForCursor()
	if !ok {
		return m, nil
	}
	return m.openDashboard(ref, m.cursor)
}
