package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
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

// NsEventMsg wraps a cluster.NamespaceEvent for tea.Program.Send.
type NsEventMsg cluster.NamespaceEvent

// SvcEventMsg wraps a cluster.ServiceEvent for tea.Program.Send.
type SvcEventMsg cluster.ServiceEvent

// IngEventMsg wraps a cluster.IngressEvent for tea.Program.Send.
type IngEventMsg cluster.IngressEvent

// EndpointSliceEventMsg wraps a cluster.EndpointSliceEvent. Feeds the
// Services table's READY column, aggregated per Service at render time
// rather than merged into the service row.
type EndpointSliceEventMsg cluster.EndpointSliceEvent

// ResourceBatchMsg applies a bounded informer burst before the next render.
// The forwarder binds the whole batch to one FocusTarget with FocusedMsg.
type ResourceBatchMsg []tea.Msg

// MetricsSnapshotMsg wraps a focused-cluster metrics snapshot.
type MetricsSnapshotMsg cluster.MetricsSnapshot

// NetworkSnapshotMsg wraps a focused-cluster network snapshot
// (per-pod rx/tx rates plus a cluster-aggregate).
type NetworkSnapshotMsg cluster.NetworkSnapshot

// PermissionResultMsg is fired when a SelfSubjectAccessReview returns.
// Key matches cluster.PermissionKey(). Context is the origin cluster so
// the UI receiver can drop results that arrived after the user Tabbed
// away — same pattern every other async msg uses.
type PermissionResultMsg struct {
	Context string
	Key     string
	Allowed bool
	Reason  string // populated for denials; verbatim from SSAR Status.Reason
	Err     string // populated on transport / RBAC error
}

// permState is the cached SSAR result. Reason/Err are kept so the
// RBAC overlay can explain *why* a permission was denied instead of
// just dropping the bit.
type permState struct {
	Allowed bool
	Reason  string
	Err     string
}

// DeleteResultMsg is fired when a Delete call returns.
type DeleteResultMsg cluster.DeleteResult

// NodeOpResultMsg is fired when a Cordon or Uncordon call returns.
// The Op field on cluster.NodeOpResult disambiguates which one.
type NodeOpResultMsg cluster.NodeOpResult

// toastClearMsg fires after the toast deadline so we re-render
// without the message.
type toastClearMsg time.Time

// View identifies which resource view is currently shown.
type View int

const (
	// Order matches the number keys, which walk the object graph:
	// the pod, the deployment managing it, the service fronting it,
	// the ingress exposing it — then the cluster furniture you browse
	// rather than watch. Events are deliberately absent: they are a
	// lens over a resource, not a resource, and live on `e`.
	ViewPods View = iota
	ViewDeployments
	ViewServices
	ViewIngresses
	ViewNodes
	ViewNamespaces
	ViewFleet   // fleet dashboard, full-bleed triage view
	ViewCluster // per-cluster dashboard: tiles, gauges, top pods, warnings
)

// ProbeTickMsg fires periodically so the header reflects fresh probe
// state from the model.Store.
type ProbeTickMsg time.Time

// Model is the top-level bubbletea Model.
type Model struct {
	WatchedContext  string
	focusGeneration uint64
	focusLife       *focusLifetime
	tables          *tableCache
	Store           *model.Store
	Theme           Theme
	Contexts        []string // ordered list of all kubeconfig contexts
	// Build is the version line shown in the help overlay. main wires
	// this; left empty when not provided (the help renders without it).
	Build string

	// LogTail is how many historical lines the log viewer replays
	// before following. Zero means logTailDefault; negative means the
	// whole log. main wires -log-tail here, like Build and
	// HideSidebar.
	LogTail int

	// HideSidebar suppresses the cluster rail. New defaults it to true
	// for a single-cluster kubeconfig, where the rail would list the
	// one cluster you're already looking at; `C` toggles it and
	// -no-sidebar forces it on at startup. Exported so main can apply
	// the flag, like Build.
	HideSidebar bool

	// OnFocusChange is called from a tea.Cmd when the user switches
	// the focused cluster (Tab/Shift-Tab). Main wires this to the
	// watcher swap coordinator.
	OnFocusChange func(FocusTarget)

	// OnDescribe runs the describe fetch off-thread and returns the
	// result as a tea.Msg. Main wires this so the UI doesn't have to
	// import the supervisor directly.
	OnDescribe func(req DescribeRequestMsg, focusedCtx string) tea.Msg

	// OnFleetDetail runs the fleet dashboard's on-demand cluster
	// drill-down off-thread; the UI wraps the result in a
	// generation-tagged FleetDetailMsg.
	OnFleetDetail func(ctxName string) cluster.FleetDetailResult

	// OnCanI runs a SelfSubjectAccessReview off-thread.
	OnCanI func(ctxName, verb, group, resource, namespace string) tea.Msg

	// OnDelete runs a delete off-thread.
	OnDelete func(focusedCtx string, ref cluster.DescribeRef) tea.Msg

	// OnScale resizes a deployment via the /scale subresource.
	OnScale func(focusedCtx string, ref cluster.DescribeRef, replicas int32) tea.Msg

	// OnRolloutRestart bumps a template annotation to roll a deployment.
	OnRolloutRestart func(focusedCtx string, ref cluster.DescribeRef) tea.Msg

	// OnLogsStart kicks off a follow=true log stream using req.Context.
	// Reply messages
	// (LogLineMsg / LogErrorMsg / LogEOSMsg) will arrive on the
	// program's message channel.
	OnLogsStart func(focusedCtx string, req LogStartMsg) tea.Msg

	// OnExec opens an interactive shell into a pod container. Unlike
	// the other callbacks which return a tea.Msg (synchronous fetch),
	// exec is a terminal hand-off: the callback returns a tea.Cmd
	// that wraps tea.Exec(cluster.ExecCmd) so bubbletea releases the
	// alt-screen for the lifetime of the session and reclaims it on
	// the user's `exit` / `Ctrl-D`.
	OnExec func(focus FocusTarget, ref cluster.DescribeRef, container string, command []string) tea.Cmd

	// OnCordon / OnUncordon: synchronous one-shot PATCH on
	// /spec/unschedulable. Returns a NodeOpResultMsg.
	OnCordon   func(focusedCtx string, node string) tea.Msg
	OnUncordon func(focusedCtx string, node string) tea.Msg

	// OnDrainStart begins an async drain. It returns DrainStartMsg
	// synchronously to confirm the supervisor accepted the request
	// (or surface a setup error); subsequent DrainProgressMsg /
	// DrainDoneMsg events flow through DrainMsg envelopes carrying
	// req.Session, inside the existing FocusedMsg envelope.
	OnDrainStart func(req DrainRequest) tea.Msg

	pods        map[types.UID]podRow
	nodes       map[types.UID]nodeRow
	deployments map[types.UID]deploymentRow
	events      map[types.UID]eventRow
	namespaces  map[types.UID]nsRow
	services    map[types.UID]serviceRow
	ingresses   map[types.UID]ingressRow
	// endpointSlices is keyed by slice UID, not by Service: one Service
	// owns several slices and they are summed at render time. See
	// collectEndpointCounts.
	endpointSlices map[types.UID]endpointSliceRow
	view           View
	cursor         types.UID
	width          int
	height         int
	debugMode      bool
	filterText     string // active filter; empty = no filter
	filterFocused  bool   // capturing keystrokes into filterText

	// Per-view "first event received" flags. Reset when the focused
	// cluster changes synchronously, set to true on the first event
	// of that kind. Used so the empty-table placeholder can distinguish
	// "still syncing" from "synced with zero rows" — otherwise a Tab to
	// a cluster with no pods looks identical to a stuck informer.
	syncedPods, syncedNodes, syncedDeploys, syncedEvents, syncedNamespaces bool
	syncedServices, syncedIngresses                                        bool
	syncStartedAt                                                          time.Time

	// Retained network rates, with availability and sample time tracked
	// separately. Partial samples carry a coverage label.
	clusterNetRX, clusterNetTX int64
	clusterNetOK               bool
	clusterNetAt               time.Time
	clusterNetStatus           string
	clusterNetCoverage         string // empty for a complete sample
	networkSnapshotAt          time.Time
	networkExpiresAt           time.Time
	netHistory                 netRing
	focusedMetrics             focusedMetricsState
	watchNamespace             string
	restartBaseline            map[types.UID]int32 // first-seen restart count per pod, see noteRestartBaseline

	namespace       string // empty = all namespaces
	nsPickerOpen    bool
	nsPickerCursor  int
	nsPickerOptions []string // refreshed when picker opens

	sortKey  SortKey
	sortDesc bool

	// Namespace view has its own sort state — its columns differ from
	// the pod table (no Restarts/CPU/Mem, has Pods/Deps/Warn counts),
	// so it gets a parallel enum + flag rather than overloading SortKey.
	nsSortKey  NsSortKey
	nsSortDesc bool

	helpOpen            bool
	helpScroll          int
	describe            describeState
	actionMenu          actionMenuState
	deleteConfirm       deleteConfirmState
	scaleConfirm        scaleConfirmState
	restartConfirm      restartConfirmState
	drainConfirm        drainConfirmState
	drainProgress       drainProgressState
	drainSession        uint64
	logs                logsState
	eventsLens          eventsLensState
	exec                execState
	dashboard           dashboardState
	permissions         map[string]permState // cached SSAR results, keyed via cluster.PermissionKey
	permissionsInFlight map[string]struct{}  // dispatched but not yet returned; lets the RBAC overlay render "?" without re-firing
	rbacOpen            bool
	fleet               fleetState
	fleetTrends         map[string]*trendRing
	clusterDash         clusterDashState
	toast               string // ephemeral one-line status (e.g. "Deleted Pod/foo")
	toastUntil          time.Time

	quitMsg string
}

// New returns a Model. context is the kubeconfig context whose pods
// we render; store provides multi-cluster probe state for the header.
func New(context string, store *model.Store, contexts []string) Model {
	return Model{
		WatchedContext: context,
		focusLife:      newFocusLifetime(),
		tables:         &tableCache{},
		Store:          store,
		Theme:          DefaultTheme(),
		Contexts:       contexts,
		// Seeded rather than enforced, so `C` toggles from whatever the
		// user actually sees on their first launch: a lone cluster
		// starts hidden and C reveals it, a fleet starts shown and C
		// hides it. Either way the key does something visible.
		HideSidebar:         len(contexts) <= 1,
		pods:                make(map[types.UID]podRow),
		nodes:               make(map[types.UID]nodeRow),
		deployments:         make(map[types.UID]deploymentRow),
		events:              make(map[types.UID]eventRow),
		namespaces:          make(map[types.UID]nsRow),
		services:            make(map[types.UID]serviceRow),
		ingresses:           make(map[types.UID]ingressRow),
		endpointSlices:      make(map[types.UID]endpointSliceRow),
		permissions:         make(map[string]permState),
		permissionsInFlight: make(map[string]struct{}),
		fleetTrends:         make(map[string]*trendRing),
		restartBaseline:     make(map[types.UID]int32),
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
	if focused, ok := msg.(FocusedMsg); ok {
		if focused.Focus != m.Focus() {
			discardFocusedMessage(focused.Msg)
			return m, nil
		}
		msg = focused.Msg
	} else if m.focusGeneration != 0 {
		// Once focus has changed, untagged cluster work cannot identify
		// which visit produced it, even when its context name matches.
		switch msg.(type) {
		case ResourceBatchMsg, PodEventMsg, NodeEventMsg, DeployEventMsg, EvtEventMsg, NsEventMsg,
			SvcEventMsg, IngEventMsg, EndpointSliceEventMsg, MetricsSnapshotMsg, NetworkSnapshotMsg,
			modalResultMsg, DescribeResultMsg, PermissionResultMsg, DeleteResultMsg, ScaleResultMsg, RolloutResultMsg,
			NodeOpResultMsg, DrainMsg, DrainStartMsg, DrainProgressMsg, DrainDoneMsg:
			discardFocusedMessage(msg)
			return m, nil
		}
	}
	if drain, ok := msg.(DrainMsg); ok {
		if drain.Session == 0 || drain.Session != m.drainProgress.session || m.drainProgress.finished {
			discardFocusedMessage(drain.Msg)
			return m, nil
		}
		msg = drain.Msg
	} else {
		switch msg.(type) {
		case DrainStartMsg, DrainProgressMsg, DrainDoneMsg:
			discardFocusedMessage(msg)
			return m, nil
		}
	}
	var request *modalRequest
	if result, ok := msg.(modalResultMsg); ok {
		request, msg = result.request, result.msg
	}
	switch msg := msg.(type) {
	case ResourceBatchMsg:
		var cmds []tea.Cmd
		for _, event := range msg {
			updated, cmd := m.Update(FocusedMsg{Focus: m.Focus(), Msg: event})
			m = updated.(Model)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

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
		if m.drainConfirm.open {
			return m.handleDrainConfirmKey(msg)
		}
		if m.drainProgress.open {
			return m.handleDrainProgressKey(msg)
		}
		if m.describe.open {
			return m.handleDescribeKey(msg)
		}
		if m.eventsLens.open {
			return m.handleEventsKey(msg)
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
		if m.rbacOpen {
			return m.handleRBACKey(msg)
		}
		if m.dashboard.open {
			return m.handleDashboardKey(msg)
		}
		if m.filterFocused {
			return m.handleFilterKey(msg)
		}
		if m.view == ViewFleet {
			return m.handleFleetKey(msg)
		}
		if m.view == ViewCluster {
			return m.handleClusterDashKey(msg)
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
		if msg.Kind == cluster.PodSynced {
			m.syncedPods = true
			m.watchNamespace = msg.WatchNamespace
			return m, nil
		}
		noteRestartBaseline(m.restartBaseline, msg.UID, msg.Restarts, msg.Kind == cluster.PodDeleted)
		before, existed := m.pods[msg.UID]
		applyPodEvent(m.pods, cluster.PodEvent(msg))
		after, exists := m.pods[msg.UID]
		m.invalidatePodOrder(before, after)
		if existed != exists || before.Phase != after.Phase {
			m.tables.phasesValid = false
		}
		if existed != exists || before.Namespace != after.Namespace {
			m.invalidateTables(ViewNamespaces)
			m.tables.nsCounts = nil
		}
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
		m.invalidateTables(ViewNodes)
		m.syncedNodes = true
		if _, ok := m.nodes[m.cursor]; !ok && m.view == ViewNodes {
			m.cursor = ""
		}
		return m, nil

	case DeployEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		if msg.Kind == cluster.DeploySynced {
			m.syncedDeploys = true
			return m, nil
		}
		applyDeployEvent(m.deployments, cluster.DeployEvent(msg))
		m.invalidateTables(ViewDeployments, ViewNamespaces)
		m.tables.nsCounts = nil
		if _, ok := m.deployments[m.cursor]; !ok && m.view == ViewDeployments {
			m.cursor = ""
		}
		return m, nil

	case EvtEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		if msg.Kind == cluster.EvtSynced {
			m.syncedEvents = true
			return m, nil
		}
		applyEvtEvent(m.events, cluster.EventEvent(msg))
		m.invalidateTables(ViewNamespaces)
		m.tables.nsCounts = nil
		return m, nil

	case NsEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyNsEvent(m.namespaces, cluster.NamespaceEvent(msg))
		m.invalidateTables(ViewNamespaces)
		m.syncedNamespaces = true
		return m, nil

	case SvcEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyServiceEvent(m.services, cluster.ServiceEvent(msg))
		m.invalidateTables(ViewServices)
		m.syncedServices = true
		if _, ok := m.services[m.cursor]; !ok && m.view == ViewServices {
			m.cursor = ""
		}
		return m, nil

	case IngEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		applyIngressEvent(m.ingresses, cluster.IngressEvent(msg))
		m.invalidateTables(ViewIngresses)
		m.syncedIngresses = true
		if _, ok := m.ingresses[m.cursor]; !ok && m.view == ViewIngresses {
			m.cursor = ""
		}
		return m, nil

	case EndpointSliceEventMsg:
		if msg.Context != m.WatchedContext {
			return m, nil
		}
		// No synced flag and no cursor guard: slices are never rows,
		// only a column on the Services table.
		applyEndpointSliceEvent(m.endpointSlices, cluster.EndpointSliceEvent(msg))
		return m, nil

	case DescribeResultMsg:
		if msg.Context != m.WatchedContext || !m.describe.open || !m.describe.loading || !m.describe.request.accepts(request) {
			return m, nil
		}
		m.describe.request.stop()
		m.describe.loading = false
		m.describe.result = cluster.DescribeResult(msg)
		m.describe.scroll = 0
		return m, nil

	case PermissionResultMsg:
		if msg.Context != "" && msg.Context != m.WatchedContext {
			return m, nil
		}
		delete(m.permissionsInFlight, msg.Key)
		m.permissions[msg.Key] = permState{Allowed: msg.Allowed, Reason: msg.Reason, Err: msg.Err}
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
				prev = m.actionMenu.options[m.actionMenu.cursor].Action
			}
			m.actionMenu.options = m.classifiedActions(m.actionMenu.ref)
			m.actionMenu.cursor = firstSelectable(m.actionMenu.options)
			if prev >= 0 {
				for i, it := range m.actionMenu.options {
					if it.Action == prev && it.Status == actionAllowed {
						m.actionMenu.cursor = i
						break
					}
				}
			}
		}
		return m, nil

	case DeleteResultMsg:
		res := cluster.DeleteResult(msg)
		if res.Context != m.WatchedContext || !m.deleteConfirm.open || !m.deleteConfirm.pending || !m.deleteConfirm.request.accepts(request) {
			return m, nil
		}
		m.deleteConfirm.request.stop()
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

	case NodeOpResultMsg:
		res := cluster.NodeOpResult(msg)
		if res.Context != m.WatchedContext {
			return m, nil
		}
		// One toast for both Cordon and Uncordon — same shape, just
		// different verb in the message. The watcher will update the
		// node row's Schedulable column on its own; we don't need to
		// mutate m.nodes here.
		verb := "Cordoned"
		if res.Op == "uncordon" {
			verb = "Uncordoned"
		}
		if res.OK {
			m.toast = fmt.Sprintf("✓ %s %s", verb, res.Node)
		} else {
			m.toast = fmt.Sprintf("✕ %s %s failed: %s", verb, res.Node, res.Err)
		}
		m.toastUntil = time.Now().Add(4 * time.Second)
		return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })

	case DrainStartMsg:
		return m.applyDrainStart(msg)
	case DrainProgressMsg:
		return m.applyDrainProgress(msg)
	case DrainDoneMsg:
		return m.applyDrainDone(msg)

	case ExecDoneMsg:
		if msg.Focus != m.Focus() {
			return m, nil
		}
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
		if res.Context != m.WatchedContext || !m.scaleConfirm.open || !m.scaleConfirm.pending || !m.scaleConfirm.request.accepts(request) {
			return m, nil
		}
		m.scaleConfirm.request.stop()
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
		if res.Context != m.WatchedContext || !m.restartConfirm.open || !m.restartConfirm.pending || !m.restartConfirm.request.accepts(request) {
			return m, nil
		}
		m.restartConfirm.request.stop()
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
		m.applyNetworkSnapshot(msg, time.Now())
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
		m.focusedMetrics = focusedMetricsState{seen: true, ok: msg.OK, at: msg.At}
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
			before := row
			if pm, ok := pmByKey[row.Namespace+"/"+row.Name]; ok {
				row.CPUMilli = pm.CPUMilli
				row.MemBytes = pm.MemBytes
				row.ContainerMemBytes = containerMemByName(pm.Containers)
				row.HasMetrics = true
			} else {
				row.HasMetrics = false
				row.ContainerMemBytes = nil
			}
			m.pods[uid] = row
			m.invalidatePodOrder(before, row)
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

	case FleetDetailMsg:
		// Guard on the cluster the dashboard requested, not on
		// WatchedContext — the dashboard shows all clusters. A result
		// for a card the user already collapsed or moved past is
		// stale and must not attach to another cluster's card.
		if m.view != ViewFleet || msg.Result.Context != m.fleet.expanded ||
			msg.Seq != m.fleet.detailSeq {
			return m, nil
		}
		m.fleet.detail = fleetDetailState{result: msg.Result}
		return m, nil

	case ProbeTickMsg:
		m.expireNetwork(time.Time(msg))
		m.sampleFleetTrends()
		// A zero timestamp is immediately due: an offline expansion
		// has no result, and when the probe later promotes that
		// cluster the panel would otherwise say "fetching" forever.
		if m.view == ViewFleet && m.fleet.expanded != "" && !m.fleet.detail.loading &&
			!m.fleetOffline(m.fleet.expanded) &&
			(m.fleet.detail.result.At.IsZero() ||
				time.Since(m.fleet.detail.result.At) > 30*time.Second) {
			cmd := m.fetchFleetDetail(m.fleet.expanded)
			return m, tea.Batch(tickCmd(), cmd)
		}
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
		m.moveCursor(+1)
	case "k", "up":
		m.moveCursor(-1)
	case "g", "home":
		m.cursorToIndex(0)
	case "G", "end":
		uids := m.visibleUIDs()
		m.cursorToIndex(len(uids) - 1)
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
		// The resource cursor deliberately survives the round trip;
		// Esc/F1 in the dashboard restores returnView with the row
		// still selected.
		m.enterFleet()
	case "f3":
		m.enterClusterDash()
	case "f2":
		m.debugMode = !m.debugMode
	case "?":
		m.helpOpen = !m.helpOpen
	case "C":
		// Tab still switches clusters with the rail hidden; this only
		// controls whether the list is drawn.
		m.HideSidebar = !m.HideSidebar
	case "R":
		m.rbacOpen = !m.rbacOpen
		if m.rbacOpen {
			cmds := m.dispatchRBACOverview()
			if len(cmds) > 0 {
				return m, tea.Batch(cmds...)
			}
		}
	case "i":
		return m.openDashboardForCursor()
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
		return m.switchView(ViewPods)
	case "2":
		return m.switchView(ViewDeployments)
	case "3":
		return m.switchView(ViewServices)
	case "4":
		return m.switchView(ViewIngresses)
	case "5":
		return m.switchView(ViewNodes)
	case "6":
		return m.switchView(ViewNamespaces)
	case "e":
		// Events for the highlighted row. Scope is snapshotted on
		// press rather than tracking the cursor, so the list doesn't
		// churn underneath you while you read it.
		return m.openEventsForCursor()
	case "l":
		// Logs for the highlighted row — the same one-key shape `e`
		// gives events and `i` gives the dashboard.
		return m.openLogsForCursorKey()
	case "E":
		return m.openEventsAll()
	case "/":
		m.filterFocused = true
	case "n":
		m.openNsPicker()
	case "0":
		// Quick-shortcut: jump back to all-namespaces.
		m.namespace = ""
		m.anchorCursorToVisible()
	case "s":
		// Each table view owns its own sort state; dispatch by view so
		// `s` cycles the visible table's columns rather than silently
		// rotating the pod table's sort key from inside another view.
		switch m.view {
		case ViewNamespaces:
			m.nsSortKey = m.nsSortKey.next()
		case ViewServices, ViewIngresses:
			// Fixed namespace/name order, no sort state to cycle.
		default:
			m.sortKey = m.sortKey.next()
		}
		// Cursor anchored to UID; new sort order will keep it visible.
	case "S":
		switch m.view {
		case ViewNamespaces:
			m.nsSortDesc = !m.nsSortDesc
		case ViewServices, ViewIngresses:
			// See `s` above.
		default:
			m.sortDesc = !m.sortDesc
		}
	case "esc":
		// ESC priority: close overlays first, then clear narrowing.
		if m.helpOpen {
			m.helpOpen = false
			return m, nil
		}
		m.filterText = ""
		m.namespace = ""
		m.anchorCursorToVisible()
	}
	return m, nil
}

// handleNsPickerKey routes input while the namespace picker is open.
// switchView moves to v, leaving the cursor alone when it's already
// the active view — pressing a view's own key used to reset the cursor
// for nothing.
func (m Model) switchView(v View) (tea.Model, tea.Cmd) {
	if m.view == v {
		return m, nil
	}
	m.view = v
	m.cursor = ""
	m.anchorCursorToVisible()
	return m, nil
}

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
	return m.openActionMenuFor(ref, m.cursor)
}

// openActionMenuFor is openActionMenu against an explicit resource,
// for callers whose subject isn't the table cursor — the dashboard
// acts on the target it drilled into, and on the replica selected in
// its PODS pane.
func (m Model) openActionMenuFor(ref cluster.DescribeRef, uid types.UID) (tea.Model, tea.Cmd) {
	ref.UID = uid
	m.actionMenu.open = true
	m.actionMenu.ref = ref
	m.actionMenu.uid = uid
	m.actionMenu.options = m.classifiedActions(ref)
	m.actionMenu.cursor = firstSelectable(m.actionMenu.options)
	m.actionMenu.notice = ""

	// Dispatch any missing SSAR checks so we can re-classify when results land.
	cmds := m.dispatchPermissionChecks(ref)
	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

// classifiedActions returns every applicable action for the row's Kind,
// each tagged with its RBAC status. Replaces the old permittedActions
// (which silently dropped denied + pending rows): showing them dimmed
// is more informative — users now see "Exec (no permission)" instead
// of an Exec entry that just isn't there.
//
// Node has one extra dimension: Cordon and Uncordon are mutually
// exclusive based on the node's current Schedulable state. We surface
// only the one that would actually change something — showing both
// against a cordoned node would let the user "cordon" something
// already cordoned, which reads as a no-op bug.
func (m Model) classifiedActions(ref cluster.DescribeRef) []actionItem {
	all := actionsFor(ref.Kind)
	out := make([]actionItem, 0, len(all))
	var schedulable, hasNode bool
	if ref.Kind == "Node" {
		schedulable, hasNode = m.nodeSchedulable(ref.Name)
	}
	for _, a := range all {
		if ref.Kind == "Node" && hasNode {
			if a == ActCordon && !schedulable {
				continue
			}
			if a == ActUncordon && schedulable {
				continue
			}
		}
		av, gated := verbsForAction(a, ref)
		if !gated {
			out = append(out, actionItem{Action: a, Status: actionAllowed})
			continue
		}
		key := cluster.PermissionKey(m.WatchedContext, av.Verb, av.Group, av.Resource, ref.Namespace)
		if state, ok := m.permissions[key]; ok {
			if state.Allowed {
				out = append(out, actionItem{Action: a, Status: actionAllowed})
			} else {
				out = append(out, actionItem{Action: a, Status: actionDenied, Reason: state.Reason})
			}
			continue
		}
		out = append(out, actionItem{Action: a, Status: actionPending})
	}
	return out
}

// firstSelectable returns the index of the first allowed row in opts,
// or 0 if nothing is selectable yet. Used when (re)opening the menu so
// the cursor lands on something the user can press Enter on.
func firstSelectable(opts []actionItem) int {
	for i, it := range opts {
		if it.Status == actionAllowed {
			return i
		}
	}
	return 0
}

// nodeSchedulable looks up the cached Schedulable state for a node
// by name. Second return is false when the node isn't in our cache
// — in which case the caller falls back to showing both Cordon and
// Uncordon, since we don't know which would be the no-op.
func (m Model) nodeSchedulable(name string) (bool, bool) {
	for _, n := range m.nodes {
		if n.Name == name {
			return n.Schedulable, true
		}
	}
	return false, false
}

// dispatchPermissionChecks emits SSAR commands for gated actions
// whose permission status we haven't cached yet. Marks each dispatched
// key in-flight so the RBAC overlay (and a second concurrent menu open)
// can render "?" without re-firing the same SSAR.
func (m *Model) dispatchPermissionChecks(ref cluster.DescribeRef) []tea.Cmd {
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
		if _, busy := m.permissionsInFlight[key]; busy {
			continue
		}
		m.permissionsInFlight[key] = struct{}{}
		cb := m.OnCanI
		ctxName := m.WatchedContext
		ns := ref.Namespace
		v := av
		cmds = append(cmds, m.focusedCmd(func() tea.Msg {
			return cb(ctxName, v.Verb, v.Group, v.Resource, ns)
		}))
	}
	return cmds
}

// dispatchRBACOverview fans out every rbacProbeSet entry against both
// cluster scope (namespace="") and the active namespace filter (when
// set). Bounded at ~22 SSARs per cluster; results land via the same
// PermissionResultMsg path so the cache shape stays single-purpose.
func (m *Model) dispatchRBACOverview() []tea.Cmd {
	if m.OnCanI == nil {
		return nil
	}
	scopes := []string{""}
	if m.namespace != "" {
		scopes = append(scopes, m.namespace)
	}
	cmds := []tea.Cmd{}
	for _, p := range rbacProbeSet() {
		for _, ns := range scopes {
			key := cluster.PermissionKey(m.WatchedContext, p.Verb, p.APIGroup, p.Resource, ns)
			if _, ok := m.permissions[key]; ok {
				continue
			}
			if _, busy := m.permissionsInFlight[key]; busy {
				continue
			}
			m.permissionsInFlight[key] = struct{}{}
			cb := m.OnCanI
			ctxName := m.WatchedContext
			verb, group, res, scope := p.Verb, p.APIGroup, p.Resource, ns
			cmds = append(cmds, m.focusedCmd(func() tea.Msg {
				return cb(ctxName, verb, group, res, scope)
			}))
		}
	}
	return cmds
}

// handleActionMenuKey routes input while the action menu is open.
// j/k skip past denied/pending rows so the cursor only ever sits on
// something Enter can act on. Enter on anything else is a no-op
// (defensive — the cursor logic should already prevent this).
func (m Model) handleActionMenuKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		m.actionMenu.open = false
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		for i := m.actionMenu.cursor + 1; i < len(m.actionMenu.options); i++ {
			if m.actionMenu.options[i].Status == actionAllowed {
				m.actionMenu.cursor = i
				break
			}
		}
	case "k", "up":
		for i := m.actionMenu.cursor - 1; i >= 0; i-- {
			if m.actionMenu.options[i].Status == actionAllowed {
				m.actionMenu.cursor = i
				break
			}
		}
	case "enter":
		if m.actionMenu.cursor >= len(m.actionMenu.options) {
			return m, nil
		}
		it := m.actionMenu.options[m.actionMenu.cursor]
		if it.Status != actionAllowed {
			return m, nil
		}
		return m.executeAction(it.Action)
	}
	return m, nil
}

// executeAction dispatches the chosen action. Describe runs the live
// fetch flow; Logs/Delete are placeholders for now (next iterations).
func (m Model) executeAction(a Action) (tea.Model, tea.Cmd) {
	switch a {
	case ActDashboard:
		ref, uid := m.actionMenu.ref, m.actionMenu.uid
		m.actionMenu.open = false
		return m.openDashboard(ref, uid)
	case ActDescribe:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.startDescribe(ref, false)
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
		return m.openEventsFor(ref)
	case ActScale:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openScaleConfirm(ref)
	case ActRestart:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openRestartConfirm(ref)
	case ActCordon:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		if m.OnCordon == nil {
			return m, nil
		}
		cb := m.OnCordon
		focused := m.WatchedContext
		name := ref.Name
		return m, m.focusedCmd(func() tea.Msg { return cb(focused, name) })
	case ActUncordon:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		if m.OnUncordon == nil {
			return m, nil
		}
		cb := m.OnUncordon
		focused := m.WatchedContext
		name := ref.Name
		return m, m.focusedCmd(func() tea.Msg { return cb(focused, name) })
	case ActDrain:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		return m.openDrainConfirm(ref)
	case ActSetNamespace:
		ref := m.actionMenu.ref
		m.actionMenu.open = false
		// Client-side only — no apiserver call. Switch the namespace
		// filter, jump to the Pods view (the most common reason you
		// scoped to a namespace is to see what's running in it), and
		// surface a toast so the action feels acknowledged. Cursor
		// resets because the previous-view cursor wouldn't map.
		m.namespace = ref.Name
		m.view = ViewPods
		m.cursor = ""
		m.anchorCursorToVisible()
		m.toast = "ns:" + ref.Name
		m.toastUntil = time.Now().Add(2 * time.Second)
		return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
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
	return m.startDescribe(ref, reveal)
}

// handleDescribeKey routes input while the describe overlay is open.
func (m Model) handleDescribeKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	maxScroll := 0
	if m.describe.result.YAML != "" {
		maxScroll = strings.Count(m.describe.result.YAML, "\n")
	}
	switch k.String() {
	case "esc", "q":
		m.closeDescribe()
	case "ctrl+c":
		m.closeDescribe()
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
			return m.startDescribe(ref, true)
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
	case "f1", "f2", "f3", "tab", "shift+tab":
		m.filterFocused = false
		if m.view == ViewFleet {
			return m.handleFleetKey(k)
		}
		if m.view == ViewCluster {
			return m.handleClusterDashKey(k)
		}
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

// cycleFocus moves focus to the next/previous context in the order the
// cluster rail draws them, preferring one with a healthy or degraded
// probe state. Falls back to the next context in the direction of
// travel if none qualify, so Tab can never dead-end on a fleet where
// only the focused cluster is up.
func (m *Model) cycleFocus(delta int) tea.Cmd {
	// <=1 rather than ==0: with a single context the loop below lands
	// back on the context already focused and "switches" to it, which
	// cancels every watcher, blanks the tables and
	// pays for a full re-list — a visible resync in exchange for no
	// navigation at all.
	if len(m.Contexts) <= 1 || m.OnFocusChange == nil {
		return nil
	}
	// The states come from focusOrder's snapshot, not a fresh Store.Get
	// per candidate: reachability has to be judged from the same read
	// that produced the order, or a probe landing mid-scan could make us
	// pick a context that is next in neither the old rail nor the new.
	order := m.focusOrder()
	idx := -1
	for i, st := range order {
		if st.Context == m.WatchedContext {
			idx = i
			break
		}
	}
	n := len(order)
	fallback := ""
	for step := 1; step <= n; step++ {
		j := ((idx+delta*step)%n + n) % n
		st := order[j]
		// The last step lands back on the focused context; re-selecting
		// it is the same no-navigation resync the guard above avoids.
		// (With idx == -1 — focus not in the list at all — nothing
		// matches and every context stays a candidate.)
		if st.Context == m.WatchedContext {
			continue
		}
		if st.Reach == model.ReachHealthy || st.Reach == model.ReachDegraded {
			return m.focusContext(st.Context)
		}
		if fallback == "" {
			fallback = st.Context
		}
	}
	// Nothing reachable to move to, but the user still asked to move:
	// take the immediate neighbour. Its rail row already says why it is
	// red, which beats a key that silently does nothing.
	if fallback != "" {
		return m.focusContext(fallback)
	}
	return nil
}

// focusContext points the watchers and the tables at ctx.
func (m *Model) focusContext(c string) tea.Cmd {
	if c == m.WatchedContext {
		return nil
	}
	if m.focusLife != nil {
		m.focusLife.cancel()
	}
	m.focusLife = newFocusLifetime()
	m.focusGeneration++
	m.WatchedContext = c
	// Clear before returning a command: its execution may be delayed
	// or reordered relative to the next focus change.
	m.clearFocusedState()
	cb := m.OnFocusChange
	focus := m.Focus()
	return func() tea.Msg {
		if cb != nil {
			cb(focus)
		}
		return nil
	}
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
// Until the informer has delivered its initial objects we
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

	// The dashboards drop the selected-cluster header: it would repeat
	// what they show themselves, so the body takes its rows. The fleet
	// one is additionally full-bleed (no rail); the cluster one keeps
	// the rail so Tab visibly walks the fleet underneath it.
	fleetFull := m.view == ViewFleet && !m.dashboard.open
	noHeader := fleetFull || (m.view == ViewCluster && !m.dashboard.open)
	header := ""
	if !noHeader {
		header = m.renderHeader()
	}
	footer := m.renderFooter()
	bodyHeight := m.height - lipgloss.Height(footer)
	if !noHeader {
		bodyHeight -= lipgloss.Height(header)
	}
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var body string
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
	case m.drainProgress.open:
		body = m.renderDrainProgress(m.width, bodyHeight)
	case m.drainConfirm.open:
		body = m.renderDrainConfirm(m.width, bodyHeight)
	case m.describe.open:
		body = m.renderDescribe(m.width, bodyHeight)
	case m.eventsLens.open:
		body = m.renderEventsLens(m.width, bodyHeight)
	case m.actionMenu.open:
		// Floating overlay: keep the underlying table visible around
		// the menu so the user can still see the row they came from.
		// clampCanvas below guarantees the composited result matches
		// (m.width, bodyHeight) — overlayAt only splices, it doesn't
		// alter visible dimensions.
		body = clampCanvas(m.renderBody(bodyHeight, fleetFull), m.width, bodyHeight)
		panel := m.renderActionMenuPanel()
		panelW, panelH := lipgloss.Width(panel), lipgloss.Height(panel)
		col := (m.width - panelW) / 2
		row := (bodyHeight - panelH) / 2
		if row < 0 {
			row = 0
		}
		body = overlayAt(body, panel, col, row)
	case m.exec.pickerOpen:
		body = m.renderExecPicker(m.width, bodyHeight)
	case m.helpOpen:
		body = m.renderHelp(m.width, bodyHeight)
	case m.rbacOpen:
		body = m.renderRBAC(m.width, bodyHeight)
	case m.nsPickerOpen:
		body = m.renderNsPicker(m.width, bodyHeight)
	default:
		body = m.renderBody(bodyHeight, fleetFull)
	}
	// Body and footer are run through clampCanvas so their dimensions
	// match what bodyHeight + footerHeight told JoinVertical to expect.
	// Without this, trailing newlines in body strings or wider-than-
	// inner separators in overlay boxes silently add visual rows that
	// scroll the top header line off the alt-screen.
	footerH := lipgloss.Height(footer)
	body = clampCanvas(body, m.width, bodyHeight)
	footer = clampCanvas(footer, m.width, footerH)
	if noHeader {
		// Joining the empty header string would add a phantom blank
		// row; the full-bleed layout has no header at all.
		return lipgloss.JoinVertical(lipgloss.Left, body, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) renderBody(bodyHeight int, fleetFull bool) string {
	mainWidth := m.width - SidebarWidth
	var body string
	switch {
	case m.dashboard.open:
		// Full width, no sidebar: the dashboard is about one workload,
		// and the cluster rail would just crowd the panes.
		body = m.renderDashboard(bodyHeight, m.width)
	case fleetFull:
		body = m.renderFleet(bodyHeight, m.width)
	case m.HideSidebar:
		// The rail costs 30 columns. Dropping it loses nothing the
		// header doesn't already carry — cluster name, reach, version,
		// node counts and resource bars — and the table gains the
		// width. Defaults on for a single-cluster kubeconfig; `C`
		// toggles it for a fleet.
		body = m.mainPane(bodyHeight, m.width)
	case mainWidth < 70:
		// Below ~100 total cells the sidebar starves the table of the
		// columns that make a monitor useful (CPU/MEM). Give the table
		// the whole row; the cluster list is still reachable via Tab.
		body = m.mainPane(bodyHeight, m.width)
	default:
		sidebar := m.renderSidebar(bodyHeight)
		main := m.mainPane(bodyHeight, mainWidth)
		body = lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main)
	}

	return body
}

// chromeHeights returns the header and footer heights View() will use,
// so key handlers that mirror the body arithmetic (help scrolling)
// agree with the renderer about how much fits. The fleet dashboard is
// full-bleed: zero header rows while it owns the view.
func (m Model) chromeHeights() (headerH, footerH int) {
	footerH = lipgloss.Height(m.renderFooter())
	if (m.view == ViewFleet || m.view == ViewCluster) && !m.dashboard.open {
		return 0, footerH
	}
	return lipgloss.Height(m.renderHeader()), footerH
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
	case ViewNamespaces:
		return m.renderNamespacesView(height, width)
	case ViewServices:
		return m.renderServiceTable(height, width)
	case ViewIngresses:
		return m.renderIngressTable(height, width)
	case ViewCluster:
		return m.renderClusterDash(height, width)
	}
	return m.renderTable(height, width)
}

// visibleUIDs returns the set of UIDs currently visible (filter +
// namespace + view) so cursor logic can be written generically.
func (m Model) visibleUIDs() []types.UID {
	if m.view == ViewCluster {
		return nil
	}
	if m.view == ViewFleet {
		return make([]types.UID, len(m.fleetOrder()))
	}
	return m.tableUIDs(m.view)
}

func (m Model) buildTableUIDs(view View) []types.UID {
	needle := strings.ToLower(m.filterText)
	switch view {
	case ViewNodes:
		rows := sortedNodeRows(m.nodes)
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			if !matchesNames("", r.Name, "", needle) {
				continue
			}
			out = append(out, r.UID)
		}
		return out
	case ViewDeployments:
		rows := sortedDeployRows(m.deployments)
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			if !matchesNames(r.Namespace, r.Name, m.namespace, needle) {
				continue
			}
			out = append(out, r.UID)
		}
		return out
	case ViewNamespaces:
		// Namespaces are cluster-scoped so the m.namespace filter
		// doesn't apply — selecting "ns: foo" then opening the ns
		// view should still show every namespace. Walk in the same
		// order the renderer paints (active sort key + direction) so
		// j/k step through the visible table.
		counts := m.namespaceCounts()
		rows := sortedNsRows(m.namespaces, m.nsSortKey, m.nsSortDesc, counts)
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			if !matchesNames("", r.Name, "", needle) {
				continue
			}
			out = append(out, r.UID)
		}
		return out
	case ViewServices:
		// Reuse the renderer's own filter so the cursor and the table
		// can never disagree about which rows exist.
		rows := m.visibleServiceRows()
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.UID)
		}
		return out
	case ViewIngresses:
		rows := m.visibleIngressRows()
		out := make([]types.UID, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.UID)
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
	needle := strings.ToLower(m.filterText)
	out := make([]podRow, 0, len(rows))
	for _, r := range rows {
		if matchesNames(r.Namespace, r.Name, m.namespace, needle) {
			out = append(out, r)
		}
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
	display := m.WatchedContext
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
	case ViewNamespaces:
		viewLabel = m.namespacesNoun()
		total = len(m.namespaces)
	case ViewServices:
		viewLabel = "services"
		total = len(m.services)
	case ViewIngresses:
		viewLabel = "ingresses"
		total = len(m.ingresses)
	}
	title := m.Theme.Title.Render(
		fmt.Sprintf(" kubetin %s · ns:%s · %s ", strings.TrimSpace(display), ns, viewLabel),
	)

	visible, _ := m.filterCounts()
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
	// The focused cluster's phase tally changes only with pod membership
	// or phase updates, independently of the table's sort and filters.
	running, pending, failed := m.podPhaseCounts()

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

	// Show rates only while fresh; failed or expired samples show status.
	netStr := ""
	if m.clusterNetOK {
		netStr = fmt.Sprintf("  ·  %s↓ %s  ↑ %s", m.clusterNetCoverage, formatRate(m.clusterNetRX), formatRate(m.clusterNetTX))
	} else if m.clusterNetStatus != "" {
		netStr = "  ·  net " + m.clusterNetStatus
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
	hint := " ?:help  F1:fleet  1:pods  2:deploy  3:svc  4:ing  5:nodes  6:ns  e:events  Tab:cluster  n:ns  /:filter  s:sort  Enter:actions  i:dashboard  q:quit "
	if m.view == ViewFleet && !m.dashboard.open {
		hint = " j/k:cluster  Enter:details  o:open  r:refresh  Tab:cluster  /:filter  Esc/F1:back  ?:help  q:quit "
	}
	if m.view == ViewCluster && !m.dashboard.open {
		hint = " Tab:cluster  n:ns  0:all-ns  1-6:tables  F1:fleet  Esc/F3:back  ?:help  q:quit "
	}
	if m.dashboard.open {
		hint = " Tab:pane  j/k:move  g/G:top/bottom  f:follow  i:open pod  c:container  l:logs  d:describe  Enter:actions  Esc:back "
	}
	if len(m.Contexts) <= 1 {
		// Nothing to Tab to, and the rail that would have hinted at
		// other clusters isn't drawn either. After the per-mode
		// overrides so the fleet hint sheds it too ("Tab:pane" in the
		// dashboard hint is a different key and survives).
		hint = strings.Replace(hint, "Tab:cluster  ", "", 1)
	}
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
	overlayOpen := m.logs.open || m.describe.open || m.eventsLens.open || m.deleteConfirm.open ||
		m.scaleConfirm.open || m.restartConfirm.open || m.actionMenu.open ||
		m.helpOpen || m.nsPickerOpen || m.rbacOpen || m.dashboard.open
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
	case ViewNamespaces:
		total = len(m.namespaces)
	case ViewServices:
		total = len(m.services)
	case ViewIngresses:
		total = len(m.ingresses)
	case ViewFleet:
		total = len(m.Contexts)
	case ViewCluster:
		return 0, 0
	default:
		total = len(m.pods)
	}
	if m.view == ViewFleet {
		matched = len(m.fleetOrder())
	} else {
		matched = m.tableCount(m.view)
	}
	return
}

// podColumns is the pod table in display order. min widths keep each
// header label (plus a 1-cell sort arrow) intact; prio drives the
// narrow-screen degradation order: network and node columns go first,
// the monitor core (pod / status / namespace / cpu) goes last. POD
// (prio 0) is never dropped and soaks up spare width on wide panes.
var podColumns = []column{
	{min: 12, max: 18, prio: 2}, // NAMESPACE
	{min: 20, max: 48, prio: 0}, // POD
	{min: 10, max: 12, prio: 1}, // STATUS
	{min: 10, max: 10, prio: 8}, // CONTAINERS
	{min: 9, max: 9, prio: 7},   // RESTARTS
	{min: 5, max: 5, prio: 5},   // AGE
	{min: 7, max: 7, prio: 3},   // CPU
	{min: 9, max: 9, prio: 4},   // MEM
	{min: 5, max: 5, prio: 6},   // MEM%
	{min: 9, max: 9, prio: 10},  // ↓ NET
	{min: 9, max: 9, prio: 11},  // ↑ NET
	{min: 12, max: 24, prio: 9}, // NODE
}

func (m Model) renderTable(maxRows int, maxWidth int) string {
	rows := rowsForUIDs(m.pods, m.windowUIDs(ViewPods, maxRows))

	// maxWidth-1: the warn-glyph column prefixes every line.
	w := fitColumns(podColumns, maxWidth-1)

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
	header := " " + joinCells(
		padCellANSI(mark(SortNamespace, "NAMESPACE"), w[0]),
		padCellANSI(mark(SortName, "POD"), w[1]),
		padCellANSI(mark(SortStatus, "STATUS"), w[2]),
		padCellANSI(hdr.Render("CONTAINERS"), w[3]),
		padCellANSIRight(mark(SortRestarts, "RESTARTS"), w[4]),
		padCellANSIRight(mark(SortAge, "AGE"), w[5]),
		padCellANSIRight(mark(SortCPU, "CPU"), w[6]),
		padCellANSIRight(mark(SortMem, "MEM"), w[7]),
		padCellANSIRight(mark(SortMemPct, "MEM%"), w[8]),
		padCellANSIRight(mark(SortNetRX, "↓ NET"), w[9]),
		padCellANSIRight(mark(SortNetTX, "↑ NET"), w[10]),
		padCellANSI(mark(SortNode, "NODE"), w[11]),
	)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if m.tableCount(ViewPods) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedPods, "pods"))
		return b.String()
	}

	warnIdx := recentWarningIndex(m.events)
	for _, r := range rows {
		cpuStr, memStr := "—", "—"
		if r.HasMetrics {
			cpuStr = formatCPU(r.CPUMilli)
			memStr = formatMem(r.MemBytes)
		}
		memPctCell := m.Theme.Base.Render("—")
		if p, ok := podMemPct(r); ok {
			memPctCell = m.Theme.loadStyle(p).Render(fmt.Sprintf("%d%%", p))
		}
		rxStr, txStr := r.networkDisplay()
		line := warnGlyph(warnIdx, "Pod", r.Namespace, r.Name, m.Theme) + joinCells(
			padCol(r.Namespace, w[0], m.Theme.Base),
			padCol(r.Name, w[1], m.Theme.Base),
			padCol(string(r.Phase), w[2], m.Theme.styleForPhase(r.Phase)),
			padCellANSI(podContainerDots(r, w[3], m.Theme), w[3]),
			padColRight(fmt.Sprintf("%d", r.Restarts), w[4], m.Theme.Base),
			padColRight(formatAge(r.CreatedAt), w[5], m.Theme.Base),
			padColRight(cpuStr, w[6], m.Theme.Base),
			padColRight(memStr, w[7], m.Theme.Base),
			padCellANSIRight(memPctCell, w[8]),
			padColRight(rxStr, w[9], m.Theme.Base),
			padColRight(txStr, w[10], m.Theme.Base),
			padCol(shortHost(r.Node), w[11], m.Theme.Base),
		)
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
// truncate fits s into n terminal cells, appending "…" when it had to
// cut something.
//
// Cells, not runes. A CJK ideograph or an emoji occupies two columns,
// so counting runes let a 44-rune string claim 88 cells and overflow
// the column it was measured for — padCol handed back 39 cells when
// asked for 20, and inside a lipgloss box that wraps rather than
// clips, which breaks every fixed-height layout downstream.
//
// go-runewidth is what lipgloss itself measures with, so this agrees
// with lipgloss.Width by construction. Grapheme clusters can still be
// split mid-sequence; the result is never wider than n, which is the
// property the layout depends on.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= n {
		return s
	}
	return runewidth.Truncate(s, n, "…")
}

// truncateHead fits s into n cells by cutting from the front, with
// "…" in the leading cell when truncation occurred. For values whose
// tail is the informative part — image refs, where the pinned tag
// sits at the end.
func truncateHead(s string, n int) string {
	if n <= 0 {
		return ""
	}
	w := runewidth.StringWidth(s)
	if w <= n {
		return s
	}
	// "…" is East-Asian-ambiguous — two cells under a CJK locale — so
	// measure it rather than assuming one.
	const ell = "…"
	ew := runewidth.StringWidth(ell)
	if ew > n {
		return runewidth.TruncateLeft(s, w-n, "")
	}
	return runewidth.TruncateLeft(s, w-n+ew, ell)
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
