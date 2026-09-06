package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/model"
)

func clusterDashModel(extra func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta", "gamma"})
	m.width, m.height, m.view = 160, 40, ViewCluster
	clusterDashFixture(extra)(&m)
	return m
}

func tileByLabel(t *testing.T, tiles []dashTile, label string) dashTile {
	t.Helper()
	for _, tl := range tiles {
		if tl.label == label {
			return tl
		}
	}
	t.Fatalf("no %s tile in %+v", label, tiles)
	return dashTile{}
}

func TestClusterStatsDerivations(t *testing.T) {
	m := clusterDashModel(nil)
	s := m.clusterStats()

	if s.podsTotal != 6 || s.running != 4 || s.pending != 1 || s.failed != 1 {
		t.Errorf("pods = %d run %d pend %d fail %d", s.podsTotal, s.running, s.pending, s.failed)
	}
	if s.crashloop != 1 || s.contReady != 3 || s.contTotal != 6 {
		t.Errorf("containers = %d/%d crashloop %d", s.contReady, s.contTotal, s.crashloop)
	}
	if s.restarts != 60 || s.restartsDelta != 3 {
		t.Errorf("restarts = %d delta %d, want 60 / 3", s.restarts, s.restartsDelta)
	}
	if s.deploysTotal != 3 || s.deploysReady != 1 || s.degraded != 2 {
		t.Errorf("deploys = %d/%d degraded %d", s.deploysReady, s.deploysTotal, s.degraded)
	}
	// Normal events and warnings older than the window are excluded.
	if s.warnEvents != 2 || s.warnObjects != 2 || len(s.warnGroups) != 2 {
		t.Errorf("warnings = %d events %d objects %d groups", s.warnEvents, s.warnObjects, len(s.warnGroups))
	}
	if s.warnGroups[0].Reason != "FailedScheduling" {
		t.Errorf("warnings should be newest first, got %s", s.warnGroups[0].Reason)
	}
	if len(s.topCPU) != 4 || s.topCPU[0].Name != "worker-5d4b-9qz7p" || s.topMem[0].Name != "worker-5d4b-9qz7p" {
		t.Errorf("top pods wrong: cpu %v", s.topCPU)
	}
	want := []string{
		"prod/ingest 0/2 ready",
		"default/cache 4/5 ready",
		"prod/ingest-6c8d-h7v2n CrashLoopBackOff ↺17",
		"prod/migrate-once-zz9 Failed",
		"default/batch-28d4f-abc12 Pending 12m ImagePullBackOff",
	}
	if len(s.unhealthy) != len(want) {
		t.Fatalf("unhealthy = %+v", s.unhealthy)
	}
	for i, w := range want {
		if s.unhealthy[i].text != w {
			t.Errorf("unhealthy[%d] = %q, want %q", i, s.unhealthy[i].text, w)
		}
	}
	if !s.unhealthy[0].bad || s.unhealthy[1].bad || !s.unhealthy[3].bad || s.unhealthy[4].bad {
		t.Errorf("severity glyphs wrong: %+v", s.unhealthy)
	}
	if s.podsByNode["worker-01"] != 2 {
		t.Errorf("podsByNode = %v", s.podsByNode)
	}
}

func TestClusterStatsRespectNamespaceScope(t *testing.T) {
	m := clusterDashModel(func(m *Model) { m.namespace = "prod" })
	s := m.clusterStats()
	if s.podsTotal != 4 || s.pending != 0 || s.deploysTotal != 2 || s.warnEvents != 1 {
		t.Errorf("scoped: pods %d pend %d deploys %d warn %d", s.podsTotal, s.pending, s.deploysTotal, s.warnEvents)
	}
	m.namespace = "default"
	if d := m.clusterStats().restartsDelta; d != 0 {
		t.Errorf("restart delta in default should ignore prod's +3, got %d", d)
	}
	for _, u := range s.unhealthy {
		if strings.HasPrefix(u.text, "default/") {
			t.Errorf("out-of-scope workload listed: %s", u.text)
		}
	}
	// Nodes are cluster-wide; pods-per-node still counts everything.
	if s.podsByNode["worker-01"] != 2 {
		t.Errorf("podsByNode should ignore scope, got %v", s.podsByNode)
	}
}

func TestClusterTilesHonesty(t *testing.T) {
	m := clusterDashModel(nil)
	st, _ := m.Store.Get("alpha")
	tiles := m.clusterTiles(st, m.clusterStats())
	if got := tileByLabel(t, tiles, "NODES"); got.value != "3" || got.sub != "1 cordoned" {
		t.Errorf("NODES = %+v", got)
	}
	if got := tileByLabel(t, tiles, "RESTARTS"); got.value != "60" || got.sub != "+3 in 18m" {
		t.Errorf("RESTARTS = %+v", got)
	}
	if got := tileByLabel(t, tiles, "WARNINGS"); got.value != "2" || got.sub != "15m, 2 objects" {
		t.Errorf("WARNINGS = %+v", got)
	}

	// Nothing synced: every watcher-fed tile is a dash, never a zero.
	m.pods = map[types.UID]podRow{}
	m.deployments = map[types.UID]deploymentRow{}
	m.events = map[types.UID]eventRow{}
	m.syncedPods, m.syncedDeploys, m.syncedEvents = false, false, false
	tiles = m.clusterTiles(st, m.clusterStats())
	for _, label := range []string{"PODS", "DEPLOYS", "CONTAINERS", "RESTARTS", "WARNINGS"} {
		if got := tileByLabel(t, tiles, label); got.value != "—" || got.sub != "syncing" {
			t.Errorf("%s before sync = %+v, want —/syncing", label, got)
		}
	}

	// Unreachable cluster: probe-fed tile explains instead of counting.
	pf := model.NewProbeFields()
	pf.Reach = model.ReachUnreachable
	m.Store.ApplyProbe("alpha", pf)
	st, _ = m.Store.Get("alpha")
	if got := tileByLabel(t, m.clusterTiles(st, m.clusterStats()), "NODES"); got.value != "—" || got.sub != "unreachable" {
		t.Errorf("NODES offline = %+v", got)
	}
}

func TestClusterGaugesGateOnFreshness(t *testing.T) {
	m := clusterDashModel(nil)
	st, _ := m.Store.Get("alpha")
	lines := m.gaugeLines("CPU", st, st.UsageCPUMilli, st.AllocCPUMilli, []int{1, 2, 3}, 90*time.Second, 0, cpuOrZero, 60)
	if len(lines) != 2 || !strings.Contains(lines[0], "61%") || !strings.Contains(lines[1], "1m") {
		t.Errorf("fresh gauge = %q", lines)
	}

	m.Store.ApplyMetrics("alpha", model.MetricsFields{
		UsageCPUMilli: 7400, UsageMemBytes: 37 << 30,
		MetricsAvailable: true, MetricsAt: time.Now().Add(-5 * time.Minute),
	})
	st, _ = m.Store.Get("alpha")
	lines = m.gaugeLines("CPU", st, st.UsageCPUMilli, st.AllocCPUMilli, nil, 0, 0, cpuOrZero, 60)
	if !strings.Contains(lines[0], "stale") || strings.Contains(lines[0], "61%") {
		t.Errorf("stale sample must be tagged, not drawn as current: %q", lines[0])
	}

	m.Store.ApplyMetrics("alpha", model.MetricsFields{MetricsAvailable: false})
	st, _ = m.Store.Get("alpha")
	lines = m.gaugeLines("CPU", st, 0, st.AllocCPUMilli, nil, 0, 0, cpuOrZero, 60)
	if !strings.Contains(lines[0], "metrics unavailable") {
		t.Errorf("unavailable = %q", lines)
	}

	m.namespace = "prod"
	st, _ = m.Store.Get("alpha")
	cpu, _ := m.scopedUsage()
	lines = m.gaugeLines("CPU", st, 0, 0, nil, 0, cpu, cpuOrZero, 60)
	if !strings.Contains(lines[0], "Σ pods 4.5") || strings.Contains(strings.Join(lines, ""), "▬") {
		t.Errorf("scoped gauge should sum pods without a bar: %q", lines)
	}

	// A failed focused snapshot clears every pod's HasMetrics; the sum
	// would read 0 — must say unavailable instead.
	m.focusedMetrics = focusedMetricsState{seen: true, ok: false, at: time.Now()}
	lines = m.gaugeLines("CPU", st, 0, 0, nil, 0, 0, cpuOrZero, 60)
	if !strings.Contains(lines[0], "metrics unavailable") {
		t.Errorf("scoped gauge after failed snapshot = %q", lines)
	}
	m.focusedMetrics = focusedMetricsState{seen: true, ok: true, at: time.Now().Add(-5 * time.Minute)}
	lines = m.gaugeLines("CPU", st, 0, 0, nil, 0, cpu, cpuOrZero, 60)
	if !strings.Contains(lines[0], "stale") {
		t.Errorf("scoped gauge with old snapshot = %q", lines)
	}
	m.focusedMetrics = focusedMetricsState{}
	lines = m.gaugeLines("CPU", st, 0, 0, nil, 0, 0, cpuOrZero, 60)
	if !strings.Contains(lines[0], "waiting for first metrics sample") {
		t.Errorf("scoped gauge before any snapshot = %q", lines)
	}
}

func TestClusterIdentityStripUsesFocusedMetricsUnderScope(t *testing.T) {
	m := clusterDashModel(func(m *Model) {
		m.namespace = "prod"
		m.Store.ApplyMetrics("alpha", model.MetricsFields{MetricsAvailable: false})
	})
	st, _ := m.Store.Get("alpha")
	if line := m.clusterDashIdentity(st, 120); strings.Contains(line, "unavailable") {
		t.Errorf("scoped strip should report the focused poller, got %q", line)
	}
	m.focusedMetrics = focusedMetricsState{seen: true, ok: false}
	if line := m.clusterDashIdentity(st, 120); !strings.Contains(line, "metrics unavailable") {
		t.Errorf("scoped strip after failed pod-metrics poll = %q", line)
	}
}

func TestNodesTileNamesWorstPressureCondition(t *testing.T) {
	m := clusterDashModel(func(m *Model) {
		pf := model.NewProbeFields()
		pf.Reach = model.ReachHealthy
		pf.NodeCount, pf.NodeReady = 3, 3
		pf.NodesMemPressure, pf.NodesDiskPressure, pf.NodesPIDPressure = 1, 1, 1
		m.Store.ApplyProbe("alpha", pf)
	})
	st, _ := m.Store.Get("alpha")
	if got := tileByLabel(t, m.clusterTiles(st, m.clusterStats()), "NODES"); got.sub != "1 MemoryPressure" {
		t.Errorf("one node with three conditions must not read as three nodes: %+v", got)
	}
}

func TestStackedDashboardCarriesEveryPane(t *testing.T) {
	m := clusterDashModel(nil)
	m.width, m.height = 80, 60
	out := ansiRE.ReplaceAllString(m.View(), "")
	for _, want := range []string{"TOP PODS · CPU", "TOP PODS · MEM", "NODES (3)", "WARNINGS · 15m", "UNHEALTHY WORKLOADS"} {
		if !strings.Contains(out, want) {
			t.Errorf("stacked layout lacks %q:\n%s", want, out)
		}
	}
	netRows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.ContainsAny(line, string(sparkGlyphs)) {
			netRows++
		}
	}
	if netRows < 3 {
		t.Errorf("expected cpu, mem and net sparklines in the stacked layout, found %d spark rows", netRows)
	}
}

func TestClusterDashKeysAndFilterParking(t *testing.T) {
	m := clusterDashModel(nil)
	m.view = ViewNodes
	m.filterText = "worker"

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m = next.(Model)
	if m.view != ViewCluster || m.filterText != "" || m.clusterDash.savedFilter != "worker" {
		t.Fatalf("F3: view %v filter %q saved %q", m.view, m.filterText, m.clusterDash.savedFilter)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if m.filterFocused {
		t.Errorf("/ must not focus the filter on the dashboard")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m = next.(Model)
	if m.view != ViewNodes || m.filterText != "worker" {
		t.Errorf("F3 again: view %v filter %q, want nodes/worker", m.view, m.filterText)
	}

	m.enterClusterDash()
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	m = next.(Model)
	if m.view != ViewPods || m.filterText != "worker" {
		t.Errorf("1: view %v filter %q", m.view, m.filterText)
	}

	m.enterClusterDash()
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF1})
	m = next.(Model)
	if m.view != ViewFleet || m.fleet.returnView != ViewPods {
		t.Errorf("F1 from dashboard: view %v return %v", m.view, m.fleet.returnView)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m = next.(Model)
	if m.view != ViewCluster || m.clusterDash.returnView != ViewPods {
		t.Errorf("F3 from fleet: view %v return %v", m.view, m.clusterDash.returnView)
	}
}

func TestClusterDashDropsHeaderKeepsRail(t *testing.T) {
	m := clusterDashModel(nil)
	out := m.View()
	if strings.Contains(out, "kubetin prod-eu") {
		t.Errorf("dashboard should not render the table header")
	}
	if !strings.Contains(out, "CLUSTERS (3)") {
		t.Errorf("dashboard should keep the cluster rail")
	}
	if !strings.Contains(out, "TOP PODS · CPU") || !strings.Contains(out, "UNHEALTHY WORKLOADS") {
		t.Errorf("panes missing:\n%s", out)
	}
	if h, f := m.chromeHeights(); h != 0 || f < 1 {
		t.Errorf("chromeHeights = %d/%d, want 0 header rows", h, f)
	}
}

func TestGridFrameJunctions(t *testing.T) {
	frame, rects := gridFrame(12, []gridBand{
		{h: 1, cols: splitCols(10, 2, nil)},
		{h: 1, cols: splitCols(10, 3, nil)},
		{h: 1, cols: []int{10}},
	}, DefaultTheme())
	plain := ansiRE.ReplaceAllString(frame, "")
	want := strings.Join([]string{
		"┌────┬─────┐",
		"│    │     │",
		"├──┬─┴┬────┤",
		"│  │  │    │",
		"├──┴──┴────┤",
		"│          │",
		"└──────────┘",
	}, "\n")
	if plain != want {
		t.Errorf("frame:\n%s\nwant:\n%s", plain, want)
	}
	if r := rects[1][2]; r.x != 7 || r.y != 3 || r.w != 4 || r.h != 1 {
		t.Errorf("rects[1][2] = %+v", r)
	}
}

func TestScaleSpark(t *testing.T) {
	if got := scaleSpark([]int64{0, 0}); got[0] != 0 || got[1] != 0 {
		t.Errorf("flat window = %v", got)
	}
	if got := scaleSpark([]int64{5, 10, -3}); got[0] != 50 || got[1] != 100 || got[2] != 0 {
		t.Errorf("scaled = %v", got)
	}
}
