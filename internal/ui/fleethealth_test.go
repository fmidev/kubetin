package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/fmidev/kubetin/internal/model"
)

// healthyState returns a cluster with everything measured and nothing
// wrong — the baseline each case distorts along one dimension.
func healthyState() model.ClusterState {
	return model.ClusterState{
		Context: "c1", Reach: model.ReachHealthy,
		ServerVersion: "v1.30.0",
		NodeCount:     3, NodeReady: 3,
		AllocCPUMilli: 12000, AllocMemBytes: 100 << 20,
		UsageCPUMilli: 3000, UsageMemBytes: 25 << 20,
		MetricsAvailable: true,
		MetricsAt:        time.Now(),
		PodsTotal:        42,
	}
}

// unknownState mimics a slot right after Run's seeding: healthy reach
// but every health count at the -1 sentinel.
func unknownState() model.ClusterState {
	st := healthyState()
	st.NodesMemPressure, st.NodesDiskPressure, st.NodesPIDPressure = -1, -1, -1
	st.NodesCordoned = -1
	st.PodsTotal, st.PodsPending, st.PodsFailed, st.PodsUnknownPhase = -1, -1, -1, -1
	st.DeploysTotal, st.DeploysDegraded, st.DeploysZeroReady = -1, -1, -1
	st.WarnEvents15m = -1
	return st
}

func TestClusterAlertsRules(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*model.ClusterState)
		wantSev  alertSeverity // worst severity expected
		wantText string        // substring expected in some alert ("" = expect no alerts)
	}{
		{"healthy-baseline", nil, sevInfo, ""},
		{"unknown-sentinels-never-alert", func(st *model.ClusterState) {
			*st = unknownState()
		}, sevInfo, ""},
		{"unreachable-is-offline-not-alert", func(st *model.ClusterState) {
			st.Reach = model.ReachUnreachable
			st.LastError = "dial tcp: i/o timeout"
		}, sevInfo, ""},
		{"auth-failed-is-offline-not-alert", func(st *model.ClusterState) {
			st.Reach = model.ReachAuthFailed
		}, sevInfo, ""},
		{"connecting-silent", func(st *model.ClusterState) {
			st.Reach = model.ReachConnecting
		}, sevInfo, ""},
		{"notready-nodes", func(st *model.ClusterState) {
			st.Reach = model.ReachDegraded
			st.NodeReady = 1
			st.NodesNotReadyNames = []string{"n2", "n3"}
		}, sevCrit, "2 nodes NotReady (n2, n3)"},
		{"degraded-non-node", func(st *model.ClusterState) {
			st.Reach = model.ReachDegraded
			st.LastError = "rbac: list pods denied"
		}, sevWarn, "degraded: rbac"},
		{"deploys-partial", func(st *model.ClusterState) {
			st.DeploysDegraded = 2
			st.DegradedDeployNames = []string{"a/d1 3/5"}
		}, sevWarn, "2 deployments below desired (a/d1 3/5 +1 more)"},
		{"deploys-zero-ready", func(st *model.ClusterState) {
			st.DeploysDegraded = 1
			st.DeploysZeroReady = 1
			st.DegradedDeployNames = []string{"a/d2 0/2"}
		}, sevCrit, "1 deployment below desired (a/d2 0/2)"},
		{"mem-pressure-named", func(st *model.ClusterState) {
			st.NodesMemPressure = 1
			st.NodesPressureNames = []string{"n2"}
		}, sevWarn, "MemoryPressure on n2"},
		{"multi-pressure-counts-only", func(st *model.ClusterState) {
			st.NodesMemPressure = 1
			st.NodesDiskPressure = 1
			st.NodesPressureNames = []string{"n2", "n3"}
		}, sevWarn, "MemoryPressure on 1 node"},
		{"cordoned", func(st *model.ClusterState) {
			st.NodesCordoned = 2
		}, sevWarn, "2 nodes cordoned"},
		{"pods-failed", func(st *model.ClusterState) {
			st.PodsFailed = 1
		}, sevWarn, "1 pod Failed"},
		{"pods-pending-below-threshold", func(st *model.ClusterState) {
			st.PodsPending = fleetPendingWarn - 1
		}, sevInfo, ""},
		{"pods-pending-at-threshold", func(st *model.ClusterState) {
			st.PodsPending = fleetPendingWarn
		}, sevWarn, "5 pods Pending"},
		{"warn-events-below-threshold", func(st *model.ClusterState) {
			st.WarnEvents15m = fleetWarnEventsWarn - 1
		}, sevInfo, ""},
		{"warn-events-at-threshold", func(st *model.ClusterState) {
			st.WarnEvents15m = fleetWarnEventsWarn
		}, sevWarn, "10 warning events /15m"},
		{"mem-79-silent", func(st *model.ClusterState) {
			st.UsageMemBytes = st.AllocMemBytes * 79 / 100
		}, sevInfo, ""},
		{"mem-80-warn", func(st *model.ClusterState) {
			st.UsageMemBytes = st.AllocMemBytes * 80 / 100
		}, sevWarn, "memory 80% of allocatable"},
		{"mem-89-warn", func(st *model.ClusterState) {
			st.UsageMemBytes = st.AllocMemBytes * 89 / 100
		}, sevWarn, "memory 89%"},
		{"mem-90-crit", func(st *model.ClusterState) {
			st.UsageMemBytes = st.AllocMemBytes * 90 / 100
		}, sevCrit, "memory 90%"},
		{"mem-stale-sample-never-alerts", func(st *model.ClusterState) {
			st.UsageMemBytes = st.AllocMemBytes * 95 / 100
			st.MetricsAt = time.Now().Add(-10 * time.Minute)
		}, sevInfo, ""},
		{"mem-ignored-without-metrics", func(st *model.ClusterState) {
			st.MetricsAvailable = false
			st.UsageMemBytes = st.AllocMemBytes
		}, sevInfo, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := healthyState()
			if tc.mutate != nil {
				tc.mutate(&st)
			}
			alerts := clusterAlerts(st)
			if got := worstSeverity(alerts); got != tc.wantSev {
				t.Errorf("worst severity = %d, want %d (alerts: %+v)", got, tc.wantSev, alerts)
			}
			if tc.wantText == "" {
				if crit, warn := alertCounts(alerts); crit+warn != 0 {
					t.Errorf("expected no alerts, got %+v", alerts)
				}
				return
			}
			found := false
			for _, a := range alerts {
				if strings.Contains(a.Text, tc.wantText) {
					found = true
				}
			}
			if !found {
				t.Errorf("no alert contains %q; alerts: %+v", tc.wantText, alerts)
			}
		})
	}
}

func TestClusterAlertsWorstFirst(t *testing.T) {
	st := healthyState()
	st.NodesCordoned = 1                           // warn, derived early
	st.UsageMemBytes = st.AllocMemBytes * 95 / 100 // crit, derived last
	alerts := clusterAlerts(st)
	if len(alerts) < 2 || alerts[0].Sev != sevCrit {
		t.Fatalf("crit alert should sort first: %+v", alerts)
	}
}

func TestGroupFleetOrdering(t *testing.T) {
	mild := healthyState()
	mild.Context = "mild"
	mild.NodesCordoned = 1 // one warn

	bad := healthyState()
	bad.Context = "bad"
	bad.Reach = model.ReachDegraded
	bad.NodeReady = 1
	bad.NodesNotReadyNames = []string{"n2", "n3"} // crit

	fine := healthyState()
	fine.Context = "fine"

	starting := model.ClusterState{Context: "starting", Reach: model.ReachConnecting}
	vpnOff := model.ClusterState{Context: "vpn-off", Reach: model.ReachUnreachable, LastError: "dial tcp: i/o timeout"}
	expired := model.ClusterState{Context: "expired", Reach: model.ReachAuthFailed}

	g := groupFleet([]model.ClusterState{mild, vpnOff, fine, bad, expired, starting})
	if len(g.Attention) != 2 || g.Attention[0].St.Context != "bad" || g.Attention[1].St.Context != "mild" {
		t.Errorf("attention order wrong: %+v", g.Attention)
	}
	if len(g.Healthy) != 1 || g.Healthy[0].St.Context != "fine" {
		t.Errorf("healthy wrong: %+v", g.Healthy)
	}
	if len(g.Starting) != 1 || g.Starting[0].St.Context != "starting" {
		t.Errorf("starting wrong: %+v", g.Starting)
	}
	if len(g.Offline) != 2 || g.Offline[0].St.Context != "expired" || g.Offline[1].St.Context != "vpn-off" {
		t.Errorf("offline wrong (auth failures first): %+v", g.Offline)
	}
	order := fleetOrderOf(g)
	if order[len(order)-1] != "vpn-off" || order[len(order)-2] != "expired" {
		t.Errorf("offline clusters must order last: %v", order)
	}
}

func TestDerivePulse(t *testing.T) {
	a := healthyState()
	a.Context = "a"
	a.PodsPending = 2

	b := healthyState()
	b.Context = "b"
	b.NodeCount, b.NodeReady = 5, 4
	b.NodesNotReadyNames = []string{"n5"}
	b.PodsTotal = -1 // unknown total
	b.PodsFailed = 1

	// Offline with stale carried-forward counts: visible as a count,
	// excluded from every fleet total.
	off := healthyState()
	off.Context = "off"
	off.Reach = model.ReachUnreachable
	off.PodsTotal = 500
	off.NodeCount, off.NodeReady = 9, 9

	g := groupFleet([]model.ClusterState{a, b, off})
	p := derivePulse(g)
	if p.Clusters != 3 {
		t.Errorf("Clusters = %d, want 3", p.Clusters)
	}
	if p.Offline != 1 {
		t.Errorf("Offline = %d, want 1", p.Offline)
	}
	if p.Nodes != 8 || p.NodesBad != 1 {
		t.Errorf("nodes = %d (%d bad), want 8 (1 bad)", p.Nodes, p.NodesBad)
	}
	if p.Pods != 42 || p.AllPodsKnown {
		t.Errorf("pods = %d known=%v, want 42 with AllPodsKnown=false", p.Pods, p.AllPodsKnown)
	}
	if p.PodsBad != 3 {
		t.Errorf("PodsBad = %d, want 3 (2 pending + 1 failed)", p.PodsBad)
	}
	if p.NeedAction != 1 {
		t.Errorf("NeedAction = %d, want 1 (only b alerts)", p.NeedAction)
	}
	if !p.HasMetrics || p.MemPct != 25 {
		t.Errorf("metrics = %v mem %d%%, want 25%%", p.HasMetrics, p.MemPct)
	}
}

func TestTrendRingDedupesAndCaps(t *testing.T) {
	r := &trendRing{}
	t0 := time.Now()
	r.push(10, t0)
	r.push(99, t0) // same timestamp: dropped
	if len(r.vals) != 1 || r.vals[0] != 10 {
		t.Fatalf("dedupe failed: %v", r.vals)
	}
	for i := 0; i < fleetTrendCap*2; i++ {
		r.push(i, t0.Add(time.Duration(i+1)*time.Second))
	}
	if len(r.vals) != fleetTrendCap {
		t.Errorf("len = %d, want capped at %d", len(r.vals), fleetTrendCap)
	}
	if r.vals[len(r.vals)-1] != fleetTrendCap*2-1 {
		t.Errorf("ring should keep the newest samples, got tail %d", r.vals[len(r.vals)-1])
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline(nil, 5); got != "     " {
		t.Errorf("no samples should render blanks, got %q", got)
	}
	if got := sparkline([]int{50}, 5); got != "     " {
		t.Errorf("one sample is no trend, got %q", got)
	}
	got := sparkline([]int{0, 50, 100, 120, -5}, 5)
	if w := len([]rune(got)); w != 5 {
		t.Errorf("width = %d runes, want 5 (%q)", w, got)
	}
	if !strings.HasPrefix(got, "▁") || []rune(got)[2] != '█' {
		t.Errorf("scale wrong: %q", got)
	}
	long := sparkline([]int{1, 2, 3, 4, 5, 6, 7, 8}, 4)
	if w := len([]rune(long)); w != 4 {
		t.Errorf("oversized history must clip to width, got %d runes", w)
	}
}

func TestNodeDotsPalette(t *testing.T) {
	th := DefaultTheme()
	st := healthyState() // 3 nodes
	st.NodeReady = 2
	st.NodesCordonedReady = 1
	want := th.StatusOK.Render("●") + th.StatusWrn.Render("●") + th.StatusBad.Render("●")
	if got := nodeDots(st, th); got != want {
		t.Errorf("dots = %q, want green+amber+red for ready/cordoned/notready", got)
	}

	// The cordoned node IS the NotReady one: red only, no phantom amber.
	st.NodesCordonedReady = 0
	st.NodesCordoned = 1
	want = th.StatusOK.Render(strings.Repeat("●", 2)) + th.StatusWrn.Render("") + th.StatusBad.Render("●")
	if got := nodeDots(st, th); got != want {
		t.Errorf("dots = %q, want green×2+red — cordoned+NotReady paints red once", got)
	}

	// Unknown sentinel never paints amber.
	st = healthyState()
	st.NodesCordonedReady = -1
	want = th.StatusOK.Render(strings.Repeat("●", 3)) + th.StatusWrn.Render("") + th.StatusBad.Render("")
	if got := nodeDots(st, th); got != want {
		t.Errorf("dots = %q, want all green for unknown cordon count", got)
	}
}
