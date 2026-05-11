package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// PodEventMsg wraps a cluster.PodEvent for tea.Program.Send.
type PodEventMsg cluster.PodEvent

// NodeEventMsg wraps a cluster.NodeEvent for tea.Program.Send.
type NodeEventMsg cluster.NodeEvent

// DeployEventMsg wraps a cluster.DeployEvent for tea.Program.Send.
type DeployEventMsg cluster.DeployEvent

// EvtEventMsg wraps a cluster.EventEvent for tea.Program.Send.
type EvtEventMsg cluster.EventEvent

// MetricsSnapshotMsg wraps a focused-cluster metrics snapshot.
type MetricsSnapshotMsg cluster.MetricsSnapshot

// NetworkSnapshotMsg wraps a focused-cluster network snapshot
// (per-pod rx/tx rates plus a cluster-aggregate).
type NetworkSnapshotMsg cluster.NetworkSnapshot

// PermissionResultMsg is fired when a SelfSubjectAccessReview returns.
// Key matches cluster.PermissionKey().
type PermissionResultMsg struct {
	Key     string
	Allowed bool
}

// DeleteResultMsg is fired when a Delete call returns.
type DeleteResultMsg cluster.DeleteResult

// toastClearMsg fires after the toast deadline so we re-render
// without the message.
type toastClearMsg time.Time

// PodsClearedMsg is sent when the focused cluster changes so the
// pods/nodes tables empty before new ADD events arrive.
type PodsClearedMsg struct{}

// View identifies which resource view is currently shown.
type View int

const (
	ViewPods View = iota
	ViewDeployments
	ViewNodes
	ViewEvents
	ViewOverview // fleet overview, full-screen cards
)

// ProbeTickMsg fires periodically so the header reflects fresh probe
// state from the model.Store.
type ProbeTickMsg time.Time

// Model is the top-level bubbletea Model.
type Model struct {
	WatchedContext string
	Store          *model.Store
	Theme          Theme
	Contexts       []string // ordered list of all kubeconfig contexts
	// Build is the version line shown in the help overlay. main wires
	// this; left empty when not provided (the help renders without it).
	Build string

	// OnFocusChange is called from a tea.Cmd when the user switches
	// the focused cluster (Tab/Shift-Tab). Main wires this to the
	// watcher swap coordinator.
	OnFocusChange func(ctx string)

	// OnDescribe runs the describe fetch off-thread and returns the
	// result as a tea.Msg. Main wires this so the UI doesn't have to
	// import the supervisor directly.
	OnDescribe func(req DescribeRequestMsg, focusedCtx string) tea.Msg

	// OnCanI runs a SelfSubjectAccessReview off-thread.
	OnCanI func(ctxName, verb, group, resource, namespace string) tea.Msg

	// OnDelete runs a delete off-thread.
	OnDelete func(focusedCtx string, ref cluster.DescribeRef) tea.Msg

	// OnScale resizes a deployment via the /scale subresource.
	OnScale func(focusedCtx string, ref cluster.DescribeRef, replicas int32) tea.Msg

	// OnRolloutRestart bumps a template annotation to roll a deployment.
	OnRolloutRestart func(focusedCtx string, ref cluster.DescribeRef) tea.Msg

	// OnLogsStart kicks off a follow=true log stream. Reply messages
	// (LogLineMsg / LogErrorMsg / LogEOSMsg) will arrive on the
	// program's message channel.
	OnLogsStart func(focusedCtx string, req LogStartMsg) tea.Msg

	// OnLogsStop cancels the active log stream.
	OnLogsStop func()

	// OnExec opens an interactive shell into a pod container. Unlike
	// the other callbacks which return a tea.Msg (synchronous fetch),
	// exec is a terminal hand-off: the callback returns a tea.Cmd
	// that wraps tea.Exec(cluster.ExecCmd) so bubbletea releases the
	// alt-screen for the lifetime of the session and reclaims it on
	// the user's `exit` / `Ctrl-D`.
	OnExec func(focusedCtx string, ref cluster.DescribeRef, container string, command []string) tea.Cmd

	pods          map[types.UID]podRow
	nodes         map[types.UID]nodeRow
	deployments   map[types.UID]deploymentRow
	events        map[types.UID]eventRow
	view          View
	cursor        types.UID
	width         int
	height        int
	debugMode     bool
	filterText    string // active filter; empty = no filter
	filterFocused bool   // capturing keystrokes into filterText

	// Per-view "first event received" flags. Reset when the focused
	// cluster changes (PodsClearedMsg), set to true on the first event
	// of that kind. Used so the empty-table placeholder can distinguish
	// "still syncing" from "synced with zero rows" — otherwise a Tab to
	// a cluster with no pods looks identical to a stuck informer.
	syncedPods, syncedNodes, syncedDeploys, syncedEvents bool
	syncStartedAt                                        time.Time

	// Cluster-aggregate network rates, last sample. clusterNetOK is
	// false until the first non-error NetworkSnapshotMsg arrives —
	// without that flag a fresh Tab would briefly show "0 B/s" which
	// is indistinguishable from "scraped successfully and traffic is
	// idle". When OK=false the UI hides the network panels entirely.
	clusterNetRX, clusterNetTX int64
	clusterNetOK               bool

	namespace       string // empty = all namespaces
	nsPickerOpen    bool
	nsPickerCursor  int
	nsPickerOptions []string // refreshed when picker opens

	sortKey  SortKey
	sortDesc bool

	helpOpen       bool
	describe       describeState
	actionMenu     actionMenuState
	deleteConfirm  deleteConfirmState
	scaleConfirm   scaleConfirmState
	restartConfirm restartConfirmState
	logs           logsState
	exec           execState
	permissions    map[string]bool // cached SSAR results, keyed via cluster.PermissionKey
	overviewScroll int
	toast          string // ephemeral one-line status (e.g. "Deleted Pod/foo")
	toastUntil     time.Time

	// eventScope, when non-nil, restricts ViewEvents to events whose
	// involvedObject matches the given Kind/Namespace/Name. Set by the
	// action menu's "Events" item so the user can drill from a single
	// pod / deployment / node into its events. Cleared on Esc inside
	// ViewEvents and on every switch *away* from ViewEvents — the
	// scope shouldn't survive into the next visit, otherwise it gets
	// confusing fast.
	eventScope *eventScopeRef

	quitMsg string
}

// New returns a Model. context is the kubeconfig context whose pods
// we render; store provides multi-cluster probe state for the header.
func New(context string, store *model.Store, contexts []string) Model {
	return Model{
		WatchedContext: context,
		Store:          store,
		Theme:          DefaultTheme(),
		Contexts:       contexts,
		pods:           make(map[types.UID]podRow),
		nodes:          make(map[types.UID]nodeRow),
		deployments:    make(map[types.UID]deploymentRow),
		events:         make(map[types.UID]eventRow),
		permissions:    make(map[string]bool),
	}
}

// Init kicks off the periodic probe-tick command.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return ProbeTickMsg(t) })
}

// Update handles all incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.logs.open {
			return m.handleLogsKey(msg)
		}
		if m.deleteConfirm.open {
			return m.handleDeleteConfirmKey(msg)
		}
		if m.scaleConfirm.open {
			return m.handleScaleConfirmKey(msg)
		}
		if m.restartConfirm.open {
			return m.handleRestartConfirmKey(msg)
		}
		if m.describe.open {
			return m.handleDescribeKey(msg)
		}
		if m.actionMenu.open {
			return m.handleActionMenuKey(msg)
		}
		if m.exec.pickerOpen {
			return m.handleExecPickerKey(msg)
		}
		if m.nsPickerOpen {
			return m.handleNsPickerKey(msg)
		}
		if m.helpOpen {
			return m.handleHelpKey(msg)
		}
		if m.filterFocused {
			return m.handleFilterKey(msg)
		}
		return m.handleKey(msg)

	case PodEventMsg:
		// Drop events from a context the user isn't focused on. Old
		// watchers can keep flushing buffered events past their cancel
		// (forwarder select is pseudo-random, 256-cap channels), and
		// without this guard the new cluster's m.pods inherits foreign
		// UIDs that never receive a matching DELETE.
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyPodEvent(m.pods, cluster.PodEvent(msg))
		m.syncedPods = true
		// Guard cursor wipe to ViewPods only — non-pod views park the
		// cursor on a non-pod UID that will never appear in m.pods,
		// and without the view guard every pod event nukes the cursor
		// (and the action menu surface alongside it).
		if _, ok := m.pods[m.cursor]; !ok && m.view == ViewPods {
			m.cursor = ""
		}
		return m, nil

	case NodeEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyNodeEvent(m.nodes, cluster.NodeEvent(msg))
		m.syncedNodes = true
		if _, ok := m.nodes[m.cursor]; !ok && m.view == ViewNodes {
			m.cursor = ""
		}
		return m, nil

	case DeployEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyDeployEvent(m.deployments, cluster.DeployEvent(msg))
		m.syncedDeploys = true
		if _, ok := m.deployments[m.cursor]; !ok && m.view == ViewDeployments {
			m.cursor = ""
		}
		return m, nil

	case EvtEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyEvtEvent(m.events, cluster.EventEvent(msg))
		m.syncedEvents = true
		return m, nil

	case DescribeResultMsg:
		// Drop describes that returned from a context we already
		// Tabbed away from — otherwise stale YAML overwrites the new
		// view's loading state.
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		m.describe.loading = false
		m.describe.result = cluster.DescribeResult(msg)
		m.describe.scroll = 0
		return m, nil

	case PermissionResultMsg:
		m.permissions[msg.Key] = msg.Allowed
		// If the menu is open and references this key, refresh
		// its options so newly-allowed actions become visible. Re-
		// anchor the cursor to the SAME action it was on before — by
		// identity, not by index. A slow SSAR landing while the user
		// has the menu open and inserting a new entry above the
		// cursor would otherwise silently slide the highlight onto a
		// different action.
		if m.actionMenu.open {
			var prev Action = -1
			if m.actionMenu.cursor < len(m.actionMenu.options) {
				prev = m.actionMenu.options[m.actionMenu.cursor]
			}
			m.actionMenu.options = m.permittedActions(m.actionMenu.ref)
			m.actionMenu.cursor = 0
			if prev >= 0 {
				for i, a := range m.actionMenu.options {
					if a == prev {
						m.actionMenu.cursor = i
						break
					}
				}
			}
		}
		return m, nil

	case DeleteResultMsg:
		res := cluster.DeleteResult(msg)
		// Drop results from a context the user already Tabbed away
		// from. Without this, an in-flight Delete that returns after
		// the user opened a fresh confirm on cluster B would force-
		// close B's modal and toast "Deleted A/foo" over the top.
		if res.Context != m.WatchedContext {
			return m, nil
		}
		// Close the confirm modal whether it succeeded or not — the
		// result is communicated via the footer toast.
		m.deleteConfirm.open = false
		m.deleteConfirm.pending = false
		m.deleteConfirm.typed = ""
		if res.OK {
			m.toast = fmt.Sprintf("✓ Deleted %s/%s", res.Ref.Kind, res.Ref.Name)
		} else {
			m.toast = fmt.Sprintf("✕ Delete failed: %s", res.Err)
		}
		m.toastUntil = time.Now().Add(4 * time.Second)
		return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })

	case ExecDoneMsg:
		// Bubbletea has already reclaimed the alt-screen by the time
		// this arrives. We only need to surface non-nil errors so a
		// failed setup ("rbac: create pods/exec denied", "container
		// not running", etc.) doesn't disappear silently into the
		// regained TUI. Clean exits (user typed `exit` / Ctrl-D)
		// land here with msg.Err == nil and produce no toast.
		if msg.Err != nil {
			m.toast = fmt.Sprintf("✕ Exec: %s", msg.Err.Error())
			m.toastUntil = time.Now().Add(6 * time.Second)
			return m, tea.Tick(6*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
		}
		return m, nil

	case ScaleResultMsg:
		res := cluster.ScaleResult(msg)
		if res.Context != m.WatchedContext {
			return m, nil
		}
		m.scaleConfirm.open = false
		m.scaleConfirm.pending = false
		if res.OK {
			m.toast = fmt.Sprintf("✓ Scaled %s/%s → %d", res.Ref.Kind, res.Ref.Name, res.Replicas)
		} else {
			m.toast = fmt.Sprintf("✕ Scale failed: %s", res.Err)
		}
		m.toastUntil = time.Now().Add(4 * time.Second)
		return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })

	case RolloutResultMsg:
		res := cluster.RolloutResult(msg)
		if res.Context != m.WatchedContext {
			return m, nil
		}
		m.restartConfirm.open = false
		m.restartConfirm.pending = false
		if res.OK {
			m.toast = fmt.Sprintf("✓ Rolling restart of %s/%s", res.Ref.Kind, res.Ref.Name)
		} else {
			m.toast = fmt.Sprintf("✕ Restart failed: %s", res.Err)
		}
		m.toastUntil = time.Now().Add(4 * time.Second)
		return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })

	case toastClearMsg:
		if !time.Time(msg).Before(m.toastUntil) {
			m.toast = ""
		}
		return m, nil

	case LogLineMsg:
		// Session check: drop messages from a previously-cancelled
		// stream still draining through its forwarder. Without this,
		// rapid Esc + reopen of logs (or pod-switch) lets old EOS or
		// error markers contaminate the new stream's state.
		if msg.Session != m.logs.session {
			return m, nil
		}
		m.logs.reconnecting = false
		m.applyLogLine(msg.Line)
		return m, nil
	case LogLinesMsg:
		if msg.Session != m.logs.session {
			return m, nil
		}
		m.logs.reconnecting = false
		m.applyLogLines(msg.Lines)
		return m, nil
	case LogReconnectingMsg:
		if msg.Session != m.logs.session {
			return m, nil
		}
		// Surface the in-flight reconnect; the old "✕ <err>" path
		// would have set m.logs.err here, which paints the indicator
		// red even though we're about to recover. Clear any prior
		// fatal err that lingered from a stale session too.
		m.logs.reconnecting = true
		m.logs.err = ""
		return m, nil
	case LogErrorMsg:
		if msg.Session != m.logs.session {
			return m, nil
		}
		m.logs.err = msg.Err
		m.logs.reconnecting = false
		return m, nil
	case LogEOSMsg:
		if msg.Session != m.logs.session {
			return m, nil
		}
		m.logs.finished = true
		m.logs.reconnecting = false
		return m, nil

	case NetworkSnapshotMsg:
		// Drop snapshots from a context that's no longer focused. The
		// match key (namespace+name) collides freely across clusters
		// (every cluster has kube-system/coredns-*), so without this
		// guard A's metrics overwrite B's rows on Tab — and the
		// cluster-aggregate header values are unconditionally wrong
		// regardless of name collisions.
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		if !msg.OK {
			// Don't clobber a previously-good reading on a transient
			// failure — the existing rates remain on screen until the
			// next successful scrape.
			return m, nil
		}
		netByKey := make(map[string]cluster.PodNetwork, len(msg.Pods))
		for _, n := range msg.Pods {
			netByKey[n.Namespace+"/"+n.Name] = n
		}
		for uid, row := range m.pods {
			if n, ok := netByKey[row.Namespace+"/"+row.Name]; ok {
				row.NetRXBps = n.RXBytesPerSec
				row.NetTXBps = n.TXBytesPerSec
				row.HasNetwork = true
				m.pods[uid] = row
			}
		}
		m.clusterNetRX = msg.Cluster.RXBytesPerSec
		m.clusterNetTX = msg.Cluster.TXBytesPerSec
		m.clusterNetOK = true
		return m, nil

	case MetricsSnapshotMsg:
		// Drop snapshots from a context the user is no longer focused
		// on. The match key (namespace+name for pods, name for nodes)
		// collides across clusters in practice (kube-system/coredns
		// exists everywhere), so without this guard A's CPU/Mem
		// figures land on B's identically-named rows.
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		// metrics-server PodMetrics/NodeMetrics resources don't carry
		// the source object's UID — they have their own metadata. So
		// match by namespace/name (pods) and name (nodes).
		pmByKey := make(map[string]cluster.PodMetric, len(msg.Pods))
		for _, pm := range msg.Pods {
			pmByKey[pm.Namespace+"/"+pm.Name] = pm
		}
		// Sweep all pods: present in this snapshot get fresh values;
		// absent pods get HasMetrics=false so the table renders "—"
		// instead of last-known stale CPU/Mem. Without this clear, a
		// pod that drops out of metrics-server (transient scrape
		// failure, RBAC change, eviction-then-recreate with new UID)
		// keeps its old numbers indefinitely.
		for uid, row := range m.pods {
			if pm, ok := pmByKey[row.Namespace+"/"+row.Name]; ok {
				row.CPUMilli = pm.CPUMilli
				row.MemBytes = pm.MemBytes
				row.HasMetrics = true
			} else {
				row.HasMetrics = false
			}
			m.pods[uid] = row
		}
		nmByName := make(map[string]cluster.NodeMetric, len(msg.Nodes))
		for _, nm := range msg.Nodes {
			nmByName[nm.Name] = nm
		}
		for uid, row := range m.nodes {
			if nm, ok := nmByName[row.Name]; ok {
				row.CPUMilli = nm.CPUMilli
				row.MemBytes = nm.MemBytes
				row.HasMetrics = true
			} else {
				row.HasMetrics = false
			}
			m.nodes[uid] = row
		}
		return m, nil

	case PodsClearedMsg:
		m.pods = make(map[types.UID]podRow)
		m.nodes = make(map[types.UID]nodeRow)
		m.deployments = make(map[types.UID]deploymentRow)
		m.events = make(map[types.UID]eventRow)
		m.cursor = ""
		m.syncedPods, m.syncedNodes, m.syncedDeploys, m.syncedEvents = false, false, false, false
		m.syncStartedAt = time.Now()
		m.clusterNetRX, m.clusterNetTX, m.clusterNetOK = 0, 0, false
		// A scoped object on the previous cluster doesn't exist on the
		// new one — clear so the next render isn't filtered down to
		// zero matches with no obvious way to recover.
		m.eventScope = nil
		return m, nil

	case ProbeTickMsg:
		return m, tickCmd()
	}
	return m, nil
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.view == ViewOverview {
			m.overviewScroll++
		} else {
			m.moveCursor(+1)
		}
	case "k", "up":
		if m.view == ViewOverview {
			if m.overviewScroll > 0 {
				m.overviewScroll--
			}
		} else {
			m.moveCursor(-1)
		}
	case "g", "home":
		if m.view == ViewOverview {
			m.overviewScroll = 0
		} else {
			m.cursorToIndex(0)
		}
	case "G", "end":
		if m.view == ViewOverview {
			max := m.overviewLineCount() - 1
			if max < 0 {
				max = 0
			}
			m.overviewScroll = max
		} else {
			uids := m.visibleUIDs()
			m.cursorToIndex(len(uids) - 1)
		}
	case "tab":
		// Assign cmd before returning. `return m, m.cycleFocus(+1)`
		// has unspecified l-to-r evaluation per the Go spec — gc
		// happens to mutate m before reading it for the return value
		// today, but a future compiler change could leave m's mutated
		// WatchedContext invisible to the caller.
		cmd := m.cycleFocus(+1)
		return m, cmd
	case "shift+tab":
		cmd := m.cycleFocus(-1)
		return m, cmd
	case "f1":
		m.view = ViewOverview
		m.cursor = ""
		m.eventScope = nil
	case "f2":
		m.debugMode = !m.debugMode
	case "?":
		m.helpOpen = !m.helpOpen
	case "d":
		return m.openDescribe(false)
	// Note: Shift-Y at the table level used to dispatch describe
	// reveal=true for the cursor row. That's a footgun even though
	// today's tables only host non-Secret kinds — we don't want to
	// teach the muscle memory. Reveal is now only reachable from
	// inside an already-open Secret describe via handleDescribeKey.
	case "enter":
		return m.openActionMenu()
	case "1":
		// Pressing the current view's key while already in that view
		// used to reset the cursor unconditionally. Skip the reset
		// when there's no actual view change so the cursor stays put.
		if m.view != ViewPods {
			m.view = ViewPods
			m.cursor = ""
			m.eventScope = nil
			m.anchorCursorToVisible()
		}
	case "2":
		if m.view != ViewDeployments {
			m.view = ViewDeployments
			m.cursor = ""
			m.eventScope = nil
			m.anchorCursorToVisible()
		}
	case "3":
		if m.view != ViewNodes {
			m.view = ViewNodes
			m.cursor = ""
			m.eventScope = nil
			m.anchorCursorToVisible()
		}
	case "4":
		// Pressing 4 is "show me events" (unscoped). If the user got
		// here via the action menu's Events item the scope is set;
		// pressing 4 again is the way to clear it without leaving
		// the events view.
		if m.view != ViewEvents {
			m.view = ViewEvents
			m.cursor = ""
		}
		m.eventScope = nil
	case "/":
		m.filterFocused = true
	case "n":
		m.openNsPicker()
	case "0":
		// Quick-shortcut: jump back to all-namespaces.
		m.namespace = ""
		m.anchorCursorToVisible()
	case "s":
		m.sortKey = m.sortKey.next()
		// Cursor anchored to UID; new sort order will keep it visible.
	case "S":
		m.sortDesc = !m.sortDesc
	case "esc":
		// ESC priority: close overlays first, then clear narrowing.
		if m.helpOpen {
			m.helpOpen = false
			return m, nil
		}
		m.filterText = ""
		m.namespace = ""
		m.eventScope = nil
		m.anchorCursorToVisible()
	}
	return m, nil
}

// handleNsPickerKey routes input while the namespace picker is open.
func (m Model) handleNsPickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		m.nsPickerOpen = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.nsPickerCursor < len(m.nsPickerOptions)-1 {
			m.nsPickerCursor++
		}
	case "k", "up":
		if m.nsPickerCursor > 0 {
			m.nsPickerCursor--
		}
	case "g", "home":
		m.nsPickerCursor = 0
	case "G", "end":
		if len(m.nsPickerOptions) > 0 {
			m.nsPickerCursor = len(m.nsPickerOptions) - 1
		}
	case "enter":
		if len(m.nsPickerOptions) > 0 {
			ns := m.nsPickerOptions[m.nsPickerCursor]
			if ns == "(all namespaces)" {
				m.namespace = ""
			} else {
				m.namespace = ns
			}
		}
		m.nsPickerOpen = false
		m.anchorCursorToVisible()
	}
	return m, nil
}

func (m *Model) openNsPicker() {
	// Collect namespaces from every cached resource map, not just
	// pods. A deployment in `cicd` with no pods present should still
	// be reachable through the picker; same for namespaces that only
	// have events.
	seen := map[string]struct{}{}
	for _, p := range m.pods {
		seen[p.Namespace] = struct{}{}
	}
	for _, d := range m.deployments {
		seen[d.Namespace] = struct{}{}
	}
	for _, e := range m.events {
		if e.Namespace != "" {
			seen[e.Namespace] = struct{}{}
		}
	}
	delete(seen, "")
	opts := make([]string, 0, len(seen)+1)
	opts = append(opts, "(all namespaces)")
	for ns := range seen {
		opts = append(opts, ns)
	}
	// Skip element 0 in sort, keep "(all namespaces)" pinned at top.
	if len(opts) > 1 {
		sort.Strings(opts[1:])
	}

	m.nsPickerOptions = opts
	m.nsPickerOpen = true
	m.nsPickerCursor = 0
	if m.namespace != "" {
		for i, o := range opts {
			if o == m.namespace {
				m.nsPickerCursor = i
				break
			}
		}
	}
}

// openActionMenu pops the action overlay for the currently-selected
// resource. Gated actions (Delete, Logs) are filtered by SSAR
// permission cache. Uncached gates kick off a check in the background;
// the menu re-renders when results arrive.
func (m Model) openActionMenu() (tea.Model, tea.Cmd) {
	ref, ok := m.refForCursor()
	if !ok {
		return m, nil
	}
	m.actionMenu.open = true
	m.actionMenu.ref = ref
	m.actionMenu.options = m.permittedActions(ref)
	m.actionMenu.cursor = 0
	m.actionMenu.notice = ""

	// Dispatch any missing SSAR checks so we can re-filter when results land.
	cmds := m.dispatchPermissionChecks(ref)
	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

// permittedActions filters actionsFor by what we know is allowed.
// Unknown (uncached) gated actions are HIDDEN until the SSAR returns
// — better to err on the side of "don't surface a button I might not
// be allowed to push" than to dangle a forbidden one.
func (m Model) permittedActions(ref cluster.DescribeRef) []Action {
	all := actionsFor(ref.Kind)
	out := make([]Action, 0, len(all))
	for _, a := range all {
		av, gated := verbsForAction(a, ref)
		if !gated {
			out = append(out, a)
			continue
		}
		key := cluster.PermissionKey(m.WatchedContext, av.Verb, av.Group, av.Resource, ref.Namespace)
		if allowed, ok := m.permissions[key]; ok && allowed {
			out = append(out, a)
		}
	}
	return out
}

// dispatchPermissionChecks emits SSAR commands for gated actions
// whose permission status we haven't cached yet.
func (m Model) dispatchPermissionChecks(ref cluster.DescribeRef) []tea.Cmd {
	if m.OnCanI == nil {
		return nil
	}
	all := actionsFor(ref.Kind)
	cmds := []tea.Cmd{}
	for _, a := range all {
		av, gated := verbsForAction(a, ref)
		if !gated {
			continue
		}
		key := cluster.PermissionKey(m.WatchedContext, av.Verb, av.Group, av.Resource, ref.Namespace)
		if _, ok := m.permissions[key]; ok {
			continue
		}
		cb := m.OnCanI
		ctxName := m.WatchedContext
		ns := ref.Namespace
		v := av
		cmds = append(cmds, func() tea.Msg {
			return cb(ctxName, v.Verb, v.Group, v.Resource, ns)
		})
	}
	return cmds
}

// handleActionMenuKey routes input while the action menu is open.
func (m Model) handleActionMenuKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		m.actionMenu.open = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.actionMenu.cursor < len(m.actionMenu.options)-1 {
			m.actionMenu.cursor++
		}
	case "k", "up":
		if m.actionMenu.cursor > 0 {
			m.actionMenu.cursor--
		}
	case "enter":
		return m.executeAction(m.actionMenu.options[m.actionMenu.cursor])
	}
	return m, nil
}

// executeAction dispatches the chosen action. Describe runs the live
// fetch flow; Logs/Delete are placeholders for now (next iterations).
func (m Model) executeAction(a Action) (tea.Model, tea.Cmd) {
	switch a {
	case ActDescribe:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		if m.OnDescribe == nil {
			return m, nil
		}
		m.describe.open = true
		m.describe.loading = true
		m.describe.scroll = 0
		m.describe.result = cluster.DescribeResult{Ref: ref}
		req := DescribeRequestMsg{Ref: ref, Reveal: false}
		cb := m.OnDescribe
		focused := m.WatchedContext
		return m, func() tea.Msg { return cb(req, focused) }
	case ActLogs:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openLogsForCursor(ref)
	case ActExec:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openExec(ref)
	case ActEvents:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		m.eventScope = &eventScopeRef{
			Kind:      ref.Kind,
			Namespace: ref.Namespace,
			Name:      ref.Name,
		}
		m.view = ViewEvents
		m.cursor = ""
		return m, nil
	case ActScale:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openScaleConfirm(ref)
	case ActRestart:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openRestartConfirm(ref)
	case ActDelete:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openDeleteConfirm(ref)
	}
	return m, nil
}

// openDescribe builds a DescribeRef for the cursor and dispatches the
// fetch via the OnDescribe callback. When reveal is true and the ref
// is a Secret, secret data is returned in the clear (and the callback
// is expected to audit-log that).
func (m Model) openDescribe(reveal bool) (tea.Model, tea.Cmd) {
	ref, ok := m.refForCursor()
	if !ok {
		return m, nil
	}
	if m.OnDescribe == nil {
		return m, nil
	}
	m.describe.open = true
	m.describe.loading = true
	m.describe.scroll = 0
	m.describe.result = cluster.DescribeResult{Ref: ref}
	req := DescribeRequestMsg{Ref: ref, Reveal: reveal}
	cb := m.OnDescribe
	focused := m.WatchedContext
	return m, func() tea.Msg { return cb(req, focused) }
}

// handleDescribeKey routes input while the describe overlay is open.
func (m Model) handleDescribeKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	maxScroll := 0
	if m.describe.result.YAML != "" {
		maxScroll = strings.Count(m.describe.result.YAML, "\n")
	}
	switch k.String() {
	case "esc", "q":
		m.describe.open = false
		// Drop the YAML buffer once the modal closes. When reveal=true
		// was used the buffer holds plaintext Secret values; we don't
		// want them to linger in the model for the rest of the session
		// (panic stack dumps, scroll-up artefacts, etc.).
		m.describe.result.YAML = ""
		m.describe.revealed = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.describe.scroll < maxScroll {
			m.describe.scroll++
		}
	case "k", "up":
		if m.describe.scroll > 0 {
			m.describe.scroll--
		}
	case "ctrl+d":
		m.describe.scroll += 10
		if m.describe.scroll > maxScroll {
			m.describe.scroll = maxScroll
		}
	case "ctrl+u":
		m.describe.scroll -= 10
		if m.describe.scroll < 0 {
			m.describe.scroll = 0
		}
	case "g", "home":
		m.describe.scroll = 0
	case "G", "end":
		m.describe.scroll = maxScroll
	case "Y":
		// Re-issue with reveal=true to show secret data. Mark the
		// describe state revealed so the bright banner persists for
		// the entire session of this modal.
		ref := m.describe.result.Ref
		if ref.Kind == "Secret" && m.OnDescribe != nil {
			m.describe.loading = true
			m.describe.scroll = 0
			m.describe.revealed = true
			req := DescribeRequestMsg{Ref: ref, Reveal: true}
			cb := m.OnDescribe
			focused := m.WatchedContext
			return m, func() tea.Msg { return cb(req, focused) }
		}
	}
	return m, nil
}

func (m *Model) anchorCursorToVisible() {
	uids := m.visibleUIDs()
	if uidIndex(uids, m.cursor) < 0 {
		if len(uids) > 0 {
			m.cursor = uids[0]
		} else {
			m.cursor = ""
		}
	}
}

// handleFilterKey routes input while the filter is being edited.
// Most keys go straight into filterText; ESC and ENTER unfocus. Nav
// keys (F1, F2, Tab, Shift-Tab) unfocus and then run, so the user
// doesn't have to press Esc + nav-key to switch view/cluster while a
// filter is active.
func (m Model) handleFilterKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Pre-empt navigation keys: unfocus the filter (preserve text) and
	// fall through to the normal handler.
	switch k.String() {
	case "f1", "f2", "tab", "shift+tab":
		m.filterFocused = false
		return m.handleKey(k)
	}
	switch k.Type {
	case tea.KeyEsc:
		m.filterText = ""
		m.filterFocused = false
	case tea.KeyEnter:
		m.filterFocused = false
	case tea.KeyBackspace:
		r := []rune(m.filterText)
		if len(r) > 0 {
			m.filterText = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.filterText += " "
	case tea.KeyRunes:
		m.filterText += string(k.Runes)
	case tea.KeyCtrlC:
		m.quitMsg = "bye"
		return m, tea.Quit
	}
	m.anchorCursorToVisible()
	return m, nil
}

// cycleFocus moves focus to the next/previous context with a healthy
// or degraded probe state. Falls back to any context if none qualify.
func (m *Model) cycleFocus(delta int) tea.Cmd {
	if len(m.Contexts) == 0 || m.OnFocusChange == nil {
		return nil
	}
	idx := -1
	for i, c := range m.Contexts {
		if c == m.WatchedContext {
			idx = i
			break
		}
	}
	n := len(m.Contexts)
	for step := 1; step <= n; step++ {
		j := ((idx+delta*step)%n + n) % n
		c := m.Contexts[j]
		st, ok := m.Store.Get(c)
		if !ok {
			continue
		}
		if st.Reach == model.ReachHealthy || st.Reach == model.ReachDegraded {
			m.WatchedContext = c
			cb := m.OnFocusChange
			return func() tea.Msg {
				cb(c)
				return PodsClearedMsg{}
			}
		}
	}
	return nil
}

func (m *Model) moveCursor(delta int) {
	uids := m.visibleUIDs()
	if len(uids) == 0 {
		m.cursor = ""
		return
	}
	idx := uidIndex(uids, m.cursor)
	if idx < 0 {
		idx = 0
	} else {
		idx += delta
		if idx < 0 {
			idx = 0
		}
		if idx >= len(uids) {
			idx = len(uids) - 1
		}
	}
	m.cursor = uids[idx]
}

func (m *Model) cursorToIndex(i int) {
	uids := m.visibleUIDs()
	if len(uids) == 0 {
		m.cursor = ""
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= len(uids) {
		i = len(uids) - 1
	}
	m.cursor = uids[i]
}

func uidIndex(uids []types.UID, uid types.UID) int {
	for i, u := range uids {
		if u == uid {
			return i
		}
	}
	return -1
}

// spinnerFrames cycles a braille spinner used while informers warm up.
// Driven by the 1Hz ProbeTick — one frame per second is enough feedback
// without burning CPU on per-frame redraws of an otherwise static view.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// emptyPlaceholder produces the right-shaped "no rows" text for a view.
// When the informer for that view hasn't fired its first event yet we
// show a syncing line with the elapsed seconds so the user can tell
// the difference between "still loading" and "loaded, zero rows".
func (m Model) emptyPlaceholder(synced bool, kindPlural string) string {
	if synced {
		return m.Theme.Dim.Render(fmt.Sprintf(" (no %s in this cluster)", kindPlural))
	}
	elapsed := 0
	if !m.syncStartedAt.IsZero() {
		elapsed = int(time.Since(m.syncStartedAt).Seconds())
	}
	glyph := spinnerFrames[elapsed%len(spinnerFrames)]
	return m.Theme.Dim.Render(
		fmt.Sprintf(" %s syncing %s informer (%ds)…", glyph, kindPlural, elapsed),
	)
}

// View renders the full screen.
func (m Model) View() string {
	if m.width == 0 {
		return "kubetin loading…"
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	mainWidth := m.width - SidebarWidth
	var body string
	switch {
	case m.view == ViewOverview:
		// Overview takes the whole row — it IS the cluster overview,
		// the sidebar would be redundant.
		body = m.renderOverview(bodyHeight, m.width)
	case mainWidth < 20:
		body = m.mainPane(bodyHeight, m.width)
	default:
		sidebar := m.renderSidebar(bodyHeight)
		main := m.mainPane(bodyHeight, mainWidth)
		body = lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main)
	}

	// Overlay precedence (highest first):
	// logs → confirms (delete/scale/restart) → describe → action menu
	// → exec picker → help → ns picker.
	switch {
	case m.logs.open:
		body = m.renderLogs(m.width, bodyHeight)
	case m.deleteConfirm.open:
		body = m.renderDeleteConfirm(m.width, bodyHeight)
	case m.scaleConfirm.open:
		body = m.renderScaleConfirm(m.width, bodyHeight)
	case m.restartConfirm.open:
		body = m.renderRestartConfirm(m.width, bodyHeight)
	case m.describe.open:
		body = m.renderDescribe(m.width, bodyHeight)
	case m.actionMenu.open:
		body = m.renderActionMenu(m.width, bodyHeight)
	case m.exec.pickerOpen:
		body = m.renderExecPicker(m.width, bodyHeight)
	case m.helpOpen:
		body = m.renderHelp(m.width, bodyHeight)
	case m.nsPickerOpen:
		body = m.renderNsPicker(m.width, bodyHeight)
	}
	// Body and footer are run through clampCanvas so their dimensions
	// match what bodyHeight + footerHeight told JoinVertical to expect.
	// Without this, trailing newlines in body strings or wider-than-
	// inner separators in overlay boxes silently add visual rows that
	// scroll the top header line off the alt-screen.
	footerH := lipgloss.Height(footer)
	body = clampCanvas(body, m.width, bodyHeight)
	footer = clampCanvas(footer, m.width, footerH)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// mainPane chooses among the active resource view + debug overlay.
func (m Model) mainPane(height, width int) string {
	if m.debugMode {
		return m.renderDebug(height, width)
	}
	switch m.view {
	case ViewNodes:
		return m.renderNodeTable(height, width)
	case ViewDeployments:
		return m.renderDeployTable(height, width)
	case ViewEvents:
		return m.renderEventsView(height, width)
	}
	return m.renderTable(height, width)
}

// visibleUIDs returns the set of UIDs currently visible (filter +
// namespace + view) so cursor logic can be written generically.
func (m Model) visibleUIDs() []types.UID {
	needle := strings.ToLower(m.filterText)
	switch m.view {
	case ViewNodes:
		rows := sortedNodeRows(m.nodes)
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			if needle != "" && !strings.Contains(strings.ToLower(r.Name), needle) {
				continue
			}
			out = append(out, r.UID)
		}
		return out
	case ViewDeployments:
		rows := sortedDeployRows(m.deployments)
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			if m.namespace != "" && r.Namespace != m.namespace {
				continue
			}
			if needle != "" &&
				!strings.Contains(strings.ToLower(r.Name), needle) &&
				!strings.Contains(strings.ToLower(r.Namespace), needle) {
				continue
			}
			out = append(out, r.UID)
		}
		return out
	case ViewEvents:
		out := make([]types.UID, 0, len(m.events))
		for uid, r := range m.events {
			if needle != "" &&
				!strings.Contains(strings.ToLower(r.Reason), needle) &&
				!strings.Contains(strings.ToLower(r.Message), needle) &&
				!strings.Contains(strings.ToLower(r.InvolvedName), needle) {
				continue
			}
			out = append(out, uid)
		}
		return out
	case ViewOverview:
		// Overview is full-screen cards, not row-driven; cursor count
		// here represents clusters with state. Keeps the header from
		// reading "podCount/podCount" while the overview is shown.
		out := make([]types.UID, 0, len(m.Contexts))
		for range m.Contexts {
			out = append(out, "")
		}
		return out
	}
	rows := m.visibleRows()
	out := make([]types.UID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UID)
	}
	return out
}

// visibleRows applies the active namespace + text filter to the
// sorted pod rows. Text filter matches namespace and name (case-insensitive).
func (m Model) visibleRows() []podRow {
	rows := sortedRows(m.pods, m.sortKey, m.sortDesc)
	needle := ""
	if m.filterText != "" {
		needle = strings.ToLower(m.filterText)
	}
	out := make([]podRow, 0, len(rows))
	for _, r := range rows {
		if m.namespace != "" && r.Namespace != m.namespace {
			continue
		}
		if needle != "" {
			if !strings.Contains(strings.ToLower(r.Namespace), needle) &&
				!strings.Contains(strings.ToLower(r.Name), needle) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func (m Model) renderHeader() string {
	st, _ := m.Store.Get(m.WatchedContext)
	// Each line clamped to exactly (m.width × 1) so two-line header is
	// guaranteed two visible rows regardless of how wide the metrics
	// line wants to be on a narrow terminal.
	//
	// hr is a single-row horizontal separator under the header,
	// matching the sidebar's right-edge `│` in colour (236) so the
	// top-bar divider and the sidebar divider read as the same
	// visual element wrapped around the body. View()'s bodyHeight
	// computation uses lipgloss.Height(header), so adding this row
	// automatically shrinks the body by one and the layout invariant
	// (total render == m.width × m.height) holds without further work.
	hr := lipgloss.NewStyle().
		Foreground(lipgloss.Color("236")).
		Render(strings.Repeat("─", m.width))
	return clampCanvas(m.renderHeaderIdentity(st), m.width, 1) +
		"\n" +
		clampCanvas(m.renderHeaderMetrics(st), m.width, 1) +
		"\n" +
		hr
}

// renderHeaderIdentity is the original single-line header — cluster
// name, namespace, view, reach, clock. Kept as line 1 of the new
// two-line header so we don't lose anything users were already
// looking at.
func (m Model) renderHeaderIdentity(st model.ClusterState) string {
	dot := m.Theme.styleForReach(st.Reach).Render(st.Reach.Glyph())
	display := st.RawName
	if display == "" {
		display = m.WatchedContext
	}
	ns := m.namespace
	if ns == "" {
		ns = "all"
	}
	viewLabel := "pods"
	total := len(m.pods)
	switch m.view {
	case ViewNodes:
		viewLabel = "nodes"
		total = len(m.nodes)
	case ViewDeployments:
		viewLabel = "deployments"
		total = len(m.deployments)
	case ViewEvents:
		viewLabel = "events"
		total = len(m.events)
	}
	title := m.Theme.Title.Render(
		fmt.Sprintf(" kubetin %s · ns:%s · %s ", strings.TrimSpace(display), ns, viewLabel),
	)

	visible := len(m.visibleUIDs())
	right := fmt.Sprintf(" %d/%d %s · %s · %s ",
		visible, total, viewLabel, st.Reach, time.Now().Format("15:04:05"))
	right = m.Theme.Dim.Render(right)

	left := dot + " " + title
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// renderHeaderMetrics is line 2 of the header — htop-style metrics for
// the focused cluster: CPU and MEM bars with absolute usage, pod
// counts (broken down by phase when there's anything not Running),
// node ready ratio, and the server version. We chose to keep this
// always visible because the F1 fleet overview hides it behind a
// modal swap, and the sidebar's compact bars don't carry the
// absolute numbers a top-like tool needs.
func (m Model) renderHeaderMetrics(st model.ClusterState) string {
	// Pod phase tally — only walk if we have pods cached. This is the
	// focused cluster's pod set, not a fleet aggregate.
	var running, pending, failed int
	for _, p := range m.pods {
		switch p.Phase {
		case corev1.PodRunning:
			running++
		case corev1.PodPending:
			pending++
		case corev1.PodFailed:
			failed++
		}
	}

	podStr := fmt.Sprintf("pods %d", len(m.pods))
	if pending > 0 || failed > 0 {
		extras := []string{fmt.Sprintf("%d run", running)}
		if pending > 0 {
			extras = append(extras, fmt.Sprintf("%d pend", pending))
		}
		if failed > 0 {
			extras = append(extras, fmt.Sprintf("%d fail", failed))
		}
		podStr = fmt.Sprintf("pods %d (%s)", len(m.pods), strings.Join(extras, ", "))
	}

	nodeStr := "nodes —"
	if st.NodeCount > 0 {
		if st.NodeReady != st.NodeCount {
			nodeStr = fmt.Sprintf("nodes %d/%d", st.NodeReady, st.NodeCount)
		} else {
			nodeStr = fmt.Sprintf("nodes %d", st.NodeCount)
		}
	}
	verStr := shortVersion(st.ServerVersion)
	if verStr == "" {
		verStr = "—"
	}

	// Network panel: only show when we have a real reading. Hidden
	// otherwise so users without nodes/proxy RBAC don't see "0 B/s"
	// that they can't act on.
	netStr := ""
	if m.clusterNetOK {
		netStr = fmt.Sprintf("  ·  ↓ %s  ↑ %s", formatRate(m.clusterNetRX), formatRate(m.clusterNetTX))
	}

	// Right-side context strip (net · pods · nodes · version) is
	// constant width. Compute it first so we can hand the remaining
	// columns to the bars.
	right := fmt.Sprintf("%s  ·  %s  ·  %s  ·  %s ", netStr, podStr, nodeStr, verStr)
	right = m.Theme.Dim.Render(right)
	rightW := lipgloss.Width(right)

	// Bars share what's left. overviewBar's printed width =
	// label(3) + " " + cells + " " + 4 pct + 2 spaces + suffix
	// (suffix is "<5>/<5> cores" → 13 chars OR "<7>/<7>" → 15 chars).
	// Reserve a fixed 36 chars per bar block for the non-cell parts
	// and split the rest across two bars.
	const reservedPerBar = 36
	const sepBetweenBars = 4
	avail := m.width - rightW - 2 /*leading space + buffer*/ - sepBetweenBars - 2*reservedPerBar
	cellsEach := avail / 2
	if cellsEach < 6 {
		cellsEach = 6
	}
	if cellsEach > 30 {
		cellsEach = 30
	}

	if !st.MetricsAvailable || st.AllocCPUMilli <= 0 {
		left := " " + m.Theme.Dim.Render("metrics unavailable for this cluster")
		pad := m.width - lipgloss.Width(left) - rightW
		if pad < 1 {
			pad = 1
		}
		return left + strings.Repeat(" ", pad) + right
	}

	cpu := overviewBar("CPU", st.UsageCPUMilli, st.AllocCPUMilli, cellsEach, m.Theme,
		fmt.Sprintf("%s/%s", coresStr(st.UsageCPUMilli), coresStr(st.AllocCPUMilli)))
	mem := overviewBar("MEM", st.UsageMemBytes, st.AllocMemBytes, cellsEach, m.Theme,
		fmt.Sprintf("%s/%s", memStrFixed(st.UsageMemBytes), memStrFixed(st.AllocMemBytes)))

	left := " " + cpu + strings.Repeat(" ", sepBetweenBars) + mem
	pad := m.width - lipgloss.Width(left) - rightW
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func (m Model) renderFooter() string {
	hint := " ?:help  F1:overview  1:pods  2:deploy  3:nodes  4:events  Tab:cluster  n:ns  /:filter  s:sort  Enter:actions  F2:debug  q:quit "
	hint = m.Theme.Footer.Render(hint)

	// Toast precedence over the hint when present.
	if m.toast != "" && time.Now().Before(m.toastUntil) {
		toastStyle := m.Theme.StatusOK
		if strings.HasPrefix(m.toast, "✕") {
			toastStyle = m.Theme.StatusBad
		}
		hint = toastStyle.Render(" " + m.toast)
	}

	// Filter line — shown above the hint when filter is focused or
	// has content. Caret rendered as block █ when focused. Suppress
	// when an overlay owns the body region: the filter doesn't apply
	// to log streams or describe output, and a stale "/ <text>" line
	// at the bottom of the screen while the user is reading logs is
	// just noise.
	overlayOpen := m.logs.open || m.describe.open || m.deleteConfirm.open ||
		m.scaleConfirm.open || m.restartConfirm.open || m.actionMenu.open ||
		m.helpOpen || m.nsPickerOpen
	if !overlayOpen && (m.filterFocused || m.filterText != "") {
		caret := ""
		if m.filterFocused {
			caret = m.Theme.Title.Render("█")
		}
		matched, total := m.filterCounts()
		summary := m.Theme.Dim.Render(fmt.Sprintf("  (%d/%d)", matched, total))
		prompt := m.Theme.Title.Render(" / ") + m.filterText + caret + summary
		return prompt + "\n" + hint
	}
	return hint
}

// filterCounts returns (matched, total) for the active view so the
// filter footer doesn't lie ("(M/N) deployments" was previously
// reading M from visible pods and N from m.pods regardless of view).
func (m Model) filterCounts() (matched, total int) {
	switch m.view {
	case ViewNodes:
		total = len(m.nodes)
	case ViewDeployments:
		total = len(m.deployments)
	case ViewEvents:
		total = len(m.events)
	default:
		total = len(m.pods)
	}
	matched = len(m.visibleUIDs())
	return
}

func (m Model) renderTable(maxRows int, maxWidth int) string {
	_ = maxWidth // future: column elasticity at narrow widths
	rows := m.visibleRows()

	const (
		colNs    = 18
		colName  = 36
		colPhase = 12
		// colCont fits "CONTAINERS" (10) so the header isn't truncated;
		// 10 cells is also enough room for one dot per container in
		// virtually all pods, with overflow "+N" if a sidecar-heavy
		// pod has more.
		colCont = 10
		// colRst must fit "RESTARTS" (8) plus the optional sort arrow,
		// otherwise the arrow gets ANSI-truncated off the right edge.
		colRst   = 9
		colCPU   = 7
		colMem   = 9
		colNetRX = 9
		colNetTX = 9
		colAge   = 5
		colNode  = 20
	)

	// Build a mixed-style header cell: bold-grey label + cyan arrow
	// when this column is the active sort. We have to render label
	// and arrow separately and concat — passing the resulting ANSI-
	// laced string through the byte-level truncate inside padCol
	// would slice an escape sequence in half and bleed broken codes
	// across the rest of the screen.
	hdr := m.Theme.Header
	arrowStyle := m.Theme.Title
	mark := func(col SortKey, label string) string {
		base := hdr.Render(label)
		if m.sortKey != col {
			return base
		}
		arrow := "▲"
		if m.sortDesc {
			arrow = "▼"
		}
		return base + arrowStyle.Render(arrow)
	}
	header := " " +
		padCellANSI(mark(SortNamespace, "NAMESPACE"), colNs) + "  " +
		padCellANSI(mark(SortName, "POD"), colName) + "  " +
		padCellANSI(mark(SortStatus, "STATUS"), colPhase) + "  " +
		padCellANSI(hdr.Render("CONTAINERS"), colCont) + "  " +
		padCellANSIRight(mark(SortRestarts, "RESTARTS"), colRst) + "  " +
		padCellANSIRight(mark(SortAge, "AGE"), colAge) + "  " +
		padCellANSIRight(mark(SortCPU, "CPU"), colCPU) + "  " +
		padCellANSIRight(mark(SortMem, "MEM"), colMem) + "  " +
		padCellANSIRight(mark(SortNetRX, "↓ NET"), colNetRX) + "  " +
		padCellANSIRight(mark(SortNetTX, "↑ NET"), colNetTX) + "  " +
		padCellANSI(mark(SortNode, "NODE"), colNode)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedPods, "pods"))
		return b.String()
	}

	visible := rows
	if maxRows > 0 && len(visible) > maxRows-1 {
		// Keep cursor in view: simple windowing centered on cursor.
		idx := rowIndex(rows, m.cursor)
		if idx < 0 {
			idx = 0
		}
		half := (maxRows - 1) / 2
		start := idx - half
		if start < 0 {
			start = 0
		}
		end := start + (maxRows - 1)
		if end > len(rows) {
			end = len(rows)
			start = end - (maxRows - 1)
			if start < 0 {
				start = 0
			}
		}
		visible = rows[start:end]
	}

	warnIdx := recentWarningIndex(m.events)
	for _, r := range visible {
		cpuStr, memStr := "—", "—"
		if r.HasMetrics {
			cpuStr = formatCPU(r.CPUMilli)
			memStr = formatMem(r.MemBytes)
		}
		rxStr, txStr := "—", "—"
		if r.HasNetwork {
			rxStr = formatRate(r.NetRXBps)
			txStr = formatRate(r.NetTXBps)
		}
		line := warnGlyph(warnIdx, "Pod", r.Namespace, r.Name, m.Theme) +
			padCol(r.Namespace, colNs, m.Theme.Base) + "  " +
			padCol(r.Name, colName, m.Theme.Base) + "  " +
			padCol(string(r.Phase), colPhase, m.Theme.styleForPhase(r.Phase)) + "  " +
			padCellANSI(podContainerDots(r, colCont, m.Theme), colCont) + "  " +
			padColRight(fmt.Sprintf("%d", r.Restarts), colRst, m.Theme.Base) + "  " +
			padColRight(formatAge(r.CreatedAt), colAge, m.Theme.Base) + "  " +
			padColRight(cpuStr, colCPU, m.Theme.Base) + "  " +
			padColRight(memStr, colMem, m.Theme.Base) + "  " +
			padColRight(rxStr, colNetRX, m.Theme.Base) + "  " +
			padColRight(txStr, colNetTX, m.Theme.Base) + "  " +
			padCol(shortHost(r.Node), colNode, m.Theme.Base)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func padPhase(p corev1.PodPhase, width int, th Theme) string {
	s := string(p)
	if s == "" {
		s = "—"
	}
	if len(s) > width {
		s = s[:width]
	}
	pad := strings.Repeat(" ", width-len(s))
	return th.styleForPhase(p).Render(s) + pad
}

// truncate returns s shortened to at most n visible cells, with "…"
// in the trailing cell when truncation occurred. Operates on runes
// (not bytes) so multi-byte characters in event messages, YAML
// bodies, and OS-image fields don't get sliced mid-rune. Plain text
// only — for ANSI-styled content use lipgloss.MaxWidth via
// padCellANSI / padCellANSIRight.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return string(r[0])
	}
	return string(r[:n-1]) + "…"
}

// styleForReach delegates the cluster reach colour to the theme.
func (t Theme) styleForReach(r model.Reach) lipgloss.Style {
	switch r {
	case model.ReachHealthy:
		return t.StatusOK
	case model.ReachConnecting:
		return t.StatusWrn
	case model.ReachDegraded:
		return t.StatusWrn
	case model.ReachUnreachable, model.ReachAuthFailed:
		return t.StatusBad
	}
	return t.StatusDim
}
