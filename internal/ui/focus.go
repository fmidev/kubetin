package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"
)

// FocusTarget identifies one UI focus intent, including repeat visits to a cluster.
type FocusTarget struct {
	Context    string
	Generation uint64
}

// FocusedMsg binds asynchronous work to the focus that requested it.
type FocusedMsg struct {
	Focus FocusTarget
	Msg   tea.Msg
}

type focusLifetime struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newFocusLifetime() *focusLifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &focusLifetime{ctx: ctx, cancel: cancel}
}

func (m Model) Focus() FocusTarget {
	return FocusTarget{Context: m.WatchedContext, Generation: m.focusGeneration}
}

func discardFocusedMessage(msg tea.Msg) {
	// A drain can start before its acknowledgement reaches Update. If
	// focus changed in between, release that operation's cancel handle.
	if start, ok := msg.(DrainStartMsg); ok && start.Cancel != nil {
		start.Cancel()
	}
}

func (m Model) focusedCmd(cmd tea.Cmd) tea.Cmd {
	focus, life := m.Focus(), m.focusLife
	return func() tea.Msg {
		if life != nil && life.ctx.Err() != nil {
			return nil
		}
		return FocusedMsg{Focus: focus, Msg: cmd()}
	}
}

func (m *Model) clearFocusedState() {
	m.tables = &tableCache{}
	m.pods = make(map[types.UID]podRow)
	m.nodes = make(map[types.UID]nodeRow)
	m.deployments = make(map[types.UID]deploymentRow)
	m.events = make(map[types.UID]eventRow)
	m.namespaces = make(map[types.UID]nsRow)
	m.services = make(map[types.UID]serviceRow)
	m.ingresses = make(map[types.UID]ingressRow)
	m.endpointSlices = make(map[types.UID]endpointSliceRow)
	m.cursor = ""
	m.syncedPods, m.syncedNodes, m.syncedDeploys, m.syncedEvents, m.syncedNamespaces = false, false, false, false, false
	m.syncedServices, m.syncedIngresses = false, false
	m.syncStartedAt = time.Now()
	m.clusterNetRX, m.clusterNetTX, m.clusterNetOK = 0, 0, false
	m.clusterNetCoverage = ""
	m.clusterNetAt, m.networkSnapshotAt, m.networkExpiresAt = time.Time{}, time.Time{}, time.Time{}
	m.clusterNetStatus = ""
	m.netHistory = netRing{}
	m.restartBaseline = make(map[types.UID]int32)
	m.permissions = make(map[string]permState)
	m.permissionsInFlight = make(map[string]struct{})
	m.eventsLens = eventsLensState{}
	m.dashboard = dashboardState{}
	m.stopDashboardLogs()
	m.logs = logsState{session: m.logs.session + 1}
	if m.drainProgress.cancel != nil {
		m.drainProgress.cancel()
	}
	m.drainProgress = drainProgressState{}
	m.drainConfirm = drainConfirmState{}
	m.actionMenu = actionMenuState{}
	m.deleteConfirm.request.stop()
	m.scaleConfirm.request.stop()
	m.restartConfirm.request.stop()
	m.closeDescribe()
	m.deleteConfirm = deleteConfirmState{}
	m.scaleConfirm = scaleConfirmState{}
	m.restartConfirm = restartConfirmState{}
	m.exec = execState{}
	m.nsPickerOpen, m.rbacOpen = false, false
	m.nsPickerOptions = nil
	m.nsPickerCursor = 0
	m.namespace = ""
	m.toast = ""
	m.toastUntil = time.Time{}
}
