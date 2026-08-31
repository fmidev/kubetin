package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/model"
)

func seedFleetCluster(store *model.Store, ctx string, mut func(*model.ProbeFields)) {
	pf := model.NewProbeFields()
	pf.Reach = model.ReachHealthy
	pf.ServerVersion = "v1.30.0"
	pf.NodeCount, pf.NodeReady = 3, 3
	pf.AllocCPUMilli, pf.AllocMemBytes = 12000, 100<<20
	pf.PodsTotal = 10
	if mut != nil {
		mut(&pf)
	}
	store.ApplyProbe(ctx, pf)
}

// fleetTestModel: "bad" in NEEDS ATTENTION, "fine" and "ok" healthy —
// display order [bad, fine, ok].
func fleetTestModel() Model {
	store := model.NewStore()
	seedFleetCluster(store, "fine", nil)
	seedFleetCluster(store, "ok", nil)
	seedFleetCluster(store, "bad", func(pf *model.ProbeFields) {
		pf.Reach = model.ReachDegraded
		pf.NodeReady = 2
		pf.NodesNotReadyNames = []string{"n3"}
	})
	m := New("fine", store, []string{"bad", "fine", "ok"})
	m.width, m.height = 120, 40
	m.view = ViewFleet
	return m
}

func fleetPress(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	res, cmd := m.handleFleetKey(k)
	return res.(Model), cmd
}

func TestFleetCursorMovesAndClamps(t *testing.T) {
	m := fleetTestModel()
	if got := m.fleetCursor(); got != "bad" {
		t.Fatalf("initial cursor = %q, want the worst cluster first", got)
	}
	m, _ = fleetPress(t, m, key("j"))
	m, _ = fleetPress(t, m, key("j"))
	if got := m.fleetCursor(); got != "ok" {
		t.Fatalf("cursor after jj = %q, want ok", got)
	}
	m, _ = fleetPress(t, m, key("j")) // clamp at bottom
	if got := m.fleetCursor(); got != "ok" {
		t.Fatalf("cursor must clamp at last, got %q", got)
	}
	m, _ = fleetPress(t, m, key("g"))
	if got := m.fleetCursor(); got != "bad" {
		t.Fatalf("g should jump to first, got %q", got)
	}
	m, _ = fleetPress(t, m, key("k")) // clamp at top
	if got := m.fleetCursor(); got != "bad" {
		t.Fatalf("cursor must clamp at first, got %q", got)
	}
	m, _ = fleetPress(t, m, key("G"))
	if got := m.fleetCursor(); got != "ok" {
		t.Fatalf("G should jump to last, got %q", got)
	}
}

func TestFleetFilterNarrowsAndSnapsCursor(t *testing.T) {
	m := fleetTestModel()
	m.fleet.cursorCtx = "bad"
	m.filterText = "fi"
	order := m.fleetOrder()
	if len(order) != 1 || order[0] != "fine" {
		t.Fatalf("filtered order = %v, want [fine]", order)
	}
	if got := m.fleetCursor(); got != "fine" {
		t.Fatalf("cursor should snap to a visible cluster, got %q", got)
	}
}

func TestFleetOpenSwitchesClusterAndView(t *testing.T) {
	m := fleetTestModel()
	m.OnFocusChange = func(string) {}
	m.fleet.cursorCtx = "bad"

	m, cmd := fleetPress(t, m, key("o"))
	if m.WatchedContext != "bad" {
		t.Errorf("WatchedContext = %q, want bad", m.WatchedContext)
	}
	if m.view != ViewPods {
		t.Errorf("view = %v, want ViewPods", m.view)
	}
	if cmd == nil {
		t.Error("o must return the focus-change cmd")
	}
}

func TestFleetOpenOnWatchedClusterJustSwitchesView(t *testing.T) {
	m := fleetTestModel()
	m.OnFocusChange = func(string) {}
	m.fleet.cursorCtx = "fine" // already watched

	m, cmd := fleetPress(t, m, key("o"))
	if m.view != ViewPods {
		t.Errorf("view = %v, want ViewPods", m.view)
	}
	if cmd != nil {
		t.Error("re-focusing the watched cluster would resync every watcher for nothing")
	}
}

func TestFleetToggleRoundTripPreservesResourceCursor(t *testing.T) {
	m := fleetTestModel()
	m.view = ViewDeployments
	m.cursor = "some-uid"

	res, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = res.(Model)
	if m.view != ViewFleet || m.fleet.returnView != ViewDeployments {
		t.Fatalf("f1 should enter fleet remembering the view; got view=%v return=%v",
			m.view, m.fleet.returnView)
	}

	m, _ = fleetPress(t, m, tea.KeyMsg{Type: tea.KeyF1})
	if m.view != ViewDeployments {
		t.Errorf("f1 again should restore the previous view, got %v", m.view)
	}
	if m.cursor != "some-uid" {
		t.Errorf("resource cursor must survive the round trip, got %q", m.cursor)
	}
}

func TestFleetEscClearsFilterBeforeLeaving(t *testing.T) {
	m := fleetTestModel()
	m.fleet.returnView = ViewNodes
	m.filterText = "fi"

	m, _ = fleetPress(t, m, key("esc"))
	if m.filterText != "" || m.view != ViewFleet {
		t.Fatalf("first esc clears the filter only; filter=%q view=%v", m.filterText, m.view)
	}
	m, _ = fleetPress(t, m, key("esc"))
	if m.view != ViewNodes {
		t.Errorf("second esc should leave to the return view, got %v", m.view)
	}
}
