package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/cluster"
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

func TestFleetEnterExpandsAndFetches(t *testing.T) {
	m := fleetTestModel()
	var asked string
	m.OnFleetDetail = func(ctx string) tea.Msg {
		asked = ctx
		return FleetDetailMsg{Context: ctx, At: time.Now()}
	}
	m.fleet.cursorCtx = "bad"

	m, cmd := fleetPress(t, m, key("enter"))
	if m.fleet.expanded != "bad" || !m.fleet.detail.loading {
		t.Fatalf("enter should expand and start loading; expanded=%q loading=%v",
			m.fleet.expanded, m.fleet.detail.loading)
	}
	if cmd == nil {
		t.Fatal("enter must dispatch the fetch cmd")
	}
	msg := cmd()
	if asked != "bad" {
		t.Fatalf("fetch asked for %q, want bad", asked)
	}
	res, _ := m.Update(msg)
	m = res.(Model)
	if m.fleet.detail.loading || m.fleet.detail.result.Context != "bad" {
		t.Errorf("result should land; loading=%v ctx=%q",
			m.fleet.detail.loading, m.fleet.detail.result.Context)
	}

	m, _ = fleetPress(t, m, key("enter"))
	if m.fleet.expanded != "" {
		t.Errorf("enter again should collapse, expanded=%q", m.fleet.expanded)
	}
}

func TestFleetDetailGuardDropsStaleResults(t *testing.T) {
	m := fleetTestModel()
	m.fleet.expanded = "bad"

	// A result for a cluster the user is no longer expanding.
	res, _ := m.Update(FleetDetailMsg{Context: "fine", At: time.Now()})
	m = res.(Model)
	if m.fleet.detail.result.Context != "" {
		t.Errorf("mismatched result must be dropped, got %q", m.fleet.detail.result.Context)
	}

	// Same cluster, but the user already left the dashboard.
	m.view = ViewPods
	res, _ = m.Update(FleetDetailMsg{Context: "bad", At: time.Now()})
	m = res.(Model)
	if m.fleet.detail.result.Context != "" {
		t.Errorf("result after leaving the view must be dropped, got %q", m.fleet.detail.result.Context)
	}
}

func TestFleetExpandDoesNotShowAnotherClustersRows(t *testing.T) {
	m := fleetTestModel()
	m.OnFleetDetail = func(ctx string) tea.Msg { return FleetDetailMsg{Context: ctx} }
	m.fleet.expanded = "fine"
	m.fleet.detail = fleetDetailState{result: cluster.FleetDetailResult{
		Context: "fine",
		Pods:    []cluster.FleetPodIssue{{Namespace: "x", Name: "y", Phase: "Pending"}},
		At:      time.Now(),
	}}

	// Move to another cluster and expand it: the old cluster's rows
	// must not render under the new card while the fetch is in flight.
	m.fleet.cursorCtx = "bad"
	m.fleet.expanded = ""
	m, _ = fleetPress(t, m, key("enter"))
	if got := m.fleet.detail.result.Context; got != "" {
		t.Errorf("stale detail for %q survived a cross-cluster expand", got)
	}
}

func TestFleetEscCollapsesDetailFirst(t *testing.T) {
	m := fleetTestModel()
	m.fleet.returnView = ViewNodes
	m.fleet.expanded = "bad"

	m, _ = fleetPress(t, m, key("esc"))
	if m.fleet.expanded != "" || m.view != ViewFleet {
		t.Fatalf("first esc collapses the detail; expanded=%q view=%v", m.fleet.expanded, m.view)
	}
	m, _ = fleetPress(t, m, key("esc"))
	if m.view != ViewNodes {
		t.Errorf("second esc leaves the dashboard, got %v", m.view)
	}
}

func TestFleetFilterIsolatedFromResourceFilter(t *testing.T) {
	m := fleetTestModel()
	m.view = ViewPods
	m.filterText = "kube-system"

	res, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = res.(Model)
	if m.filterText != "" {
		t.Fatalf("pod filter leaked into the fleet view: %q", m.filterText)
	}
	if got := len(m.fleetOrder()); got != 3 {
		t.Fatalf("fleet should be unfiltered on entry, %d clusters visible", got)
	}

	m.filterText = "fi" // a fleet-side filter
	m, _ = fleetPress(t, m, tea.KeyMsg{Type: tea.KeyF1})
	if m.view != ViewPods || m.filterText != "kube-system" {
		t.Errorf("leaving must restore the resource filter; view=%v filter=%q", m.view, m.filterText)
	}
}

func TestFleetOpenRestoresResourceFilter(t *testing.T) {
	m := fleetTestModel()
	m.view = ViewPods
	m.filterText = "payments"
	res, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = res.(Model)
	m.OnFocusChange = func(string) {}
	m.fleet.cursorCtx = "bad"
	m.filterText = "ba" // fleet filter

	m, _ = fleetPress(t, m, key("o"))
	if m.view != ViewPods || m.filterText != "payments" {
		t.Errorf("o must restore the resource filter; view=%v filter=%q", m.view, m.filterText)
	}
}

func TestFleetViewKeyRestoresResourceFilter(t *testing.T) {
	m := fleetTestModel()
	m.view = ViewPods
	m.filterText = "payments"
	res, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyF1})
	m = res.(Model)
	m.filterText = "prod" // fleet filter

	m, _ = fleetPress(t, m, key("2"))
	if m.view != ViewDeployments || m.filterText != "payments" {
		t.Errorf("1-6 must restore the resource filter; view=%v filter=%q", m.view, m.filterText)
	}
}

func TestFleetOversizedCardAnchorsAtItsTitle(t *testing.T) {
	m := fleetTestModel()
	store := model.NewStore()
	seedFleetCluster(store, "bad", func(pf *model.ProbeFields) {
		pf.Reach = model.ReachDegraded
		pf.NodeCount, pf.NodeReady = 5, 3
		pf.NodesNotReadyNames = []string{"n4", "n5"}
		pf.NodesMemPressure, pf.NodesDiskPressure, pf.NodesPIDPressure = 1, 1, 1
		pf.NodesPressureNames = []string{"n4"}
		pf.NodesCordoned = 2
		pf.PodsFailed, pf.PodsUnknownPhase, pf.PodsPending = 2, 1, 9
		pf.DeploysTotal, pf.DeploysDegraded = 5, 3
		pf.WarnEvents15m = 99
	})
	m.Store = store
	m.fleet.cursorCtx = "bad"

	// Region of 5 lines, card of ~12: the card cannot fit. Its first
	// line — the title naming the cluster — must still be the first
	// thing shown, not a slice from the middle.
	out := m.renderFleet(6, 80)
	visible := strings.Split(out, "\n")
	if len(visible) < 2 || !strings.Contains(visible[1], "bad") {
		t.Errorf("oversized card should anchor at its title line; got:\n%s", out)
	}
}

func TestFleetPulseHonestWhenAllPodTotalsUnknown(t *testing.T) {
	m := fleetTestModel()
	store := model.NewStore()
	seedFleetCluster(store, "a", func(pf *model.ProbeFields) {
		pf.PodsTotal = -1
		pf.PodsFailed = 2
	})
	m.Store = store

	out := m.renderFleetPulse(m.fleetGroupsFiltered(), 120)
	if !strings.Contains(out, "0+ pods") {
		t.Errorf("unknown totals must render as 0+ pods, got %q", out)
	}
	if !strings.Contains(out, "2✗") {
		t.Errorf("known bad-pod count must not be suppressed, got %q", out)
	}
}

func TestFleetOfflineSectionRendersLastAsCompactRows(t *testing.T) {
	m := fleetTestModel()
	store := model.NewStore()
	seedFleetCluster(store, "prod", nil)
	seedFleetCluster(store, "degraded", func(pf *model.ProbeFields) {
		pf.Reach = model.ReachDegraded
		pf.NodeReady = 2
		pf.NodesNotReadyNames = []string{"n3"}
	})
	seedFleetCluster(store, "vpn-off", func(pf *model.ProbeFields) {
		pf.Reach = model.ReachUnreachable
		pf.ServerVersion = ""
		pf.LastError = "dial tcp 10.8.0.1:6443: i/o timeout"
	})
	m.Store = store

	out := m.renderFleet(20, 100)
	iAttention := strings.Index(out, "NEEDS ATTENTION")
	iHealthy := strings.Index(out, "HEALTHY")
	iOffline := strings.Index(out, "OFFLINE")
	if iAttention < 0 || iHealthy < 0 || iOffline < 0 {
		t.Fatalf("missing sections: attention=%d healthy=%d offline=%d", iAttention, iHealthy, iOffline)
	}
	if !(iAttention < iHealthy && iHealthy < iOffline) {
		t.Errorf("OFFLINE must render last: attention=%d healthy=%d offline=%d",
			iAttention, iHealthy, iOffline)
	}
	if !strings.Contains(out, "unreachable: dial tcp") {
		t.Errorf("offline row should carry the reason, got:\n%s", out)
	}
	if p := derivePulse(m.fleetGroupsFiltered()); p.NeedAction != 1 {
		t.Errorf("NeedAction = %d, want 1 — offline is not attention", p.NeedAction)
	}
}

func TestFleetEnterOnOfflineExplainsInsteadOfFetching(t *testing.T) {
	m := fleetTestModel()
	store := model.NewStore()
	seedFleetCluster(store, "vpn-off", func(pf *model.ProbeFields) {
		pf.Reach = model.ReachUnreachable
		pf.ServerVersion = ""
		pf.LastError = "dial tcp 10.8.0.1:6443: i/o timeout"
	})
	m.Store = store
	fetched := false
	m.OnFleetDetail = func(string) tea.Msg { fetched = true; return nil }
	m.fleet.cursorCtx = "vpn-off"

	m, cmd := fleetPress(t, m, key("enter"))
	if cmd != nil || fetched {
		t.Fatal("enter on an offline cluster must not dispatch a fetch")
	}
	if m.fleet.expanded != "vpn-off" {
		t.Fatalf("panel should still expand, expanded=%q", m.fleet.expanded)
	}

	out := m.renderFleet(20, 100)
	if !strings.Contains(out, "details can't be fetched") ||
		!strings.Contains(out, "dial tcp 10.8.0.1:6443") {
		t.Errorf("panel must explain the offline state, got:\n%s", out)
	}
	for _, lie := range []string{"look clean", "fetching"} {
		if strings.Contains(out, lie) {
			t.Errorf("panel must not claim %q for a cluster it can't see", lie)
		}
	}

	if _, cmd := fleetPress(t, m, key("r")); cmd != nil {
		t.Error("r must not refetch an offline cluster")
	}
}
