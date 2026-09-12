package ui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
	"k8s.io/apimachinery/pkg/types"
)

func TestClusterCacheCompletionAndFocusIdentity(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	initial := m.Focus()
	apply := func(msg tea.Msg) { next, _ := m.Update(msg); m = next.(Model) }
	apply(PodEventMsg{Context: "alpha", UID: "p", Name: "api"})
	apply(DeployEventMsg{Context: "alpha", UID: "d", Name: "api"})
	apply(EvtEventMsg{Context: "alpha", UID: "e"})
	if m.syncedPods || m.syncedDeploys || m.syncedEvents {
		t.Fatal("first object marked a cache complete")
	}
	m.focusContext("beta")
	m.focusContext("alpha")
	markers := ResourceBatchMsg{
		PodEventMsg{Context: "alpha", Kind: cluster.PodSynced, WatchNamespace: "prod"},
		DeployEventMsg{Context: "alpha", Kind: cluster.DeploySynced},
		EvtEventMsg{Context: "alpha", Kind: cluster.EvtSynced},
	}
	apply(FocusedMsg{Focus: initial, Msg: markers})
	if m.syncedPods || m.syncedDeploys || m.syncedEvents || m.watchNamespace != "" {
		t.Fatal("stale sync changed the new focus")
	}
	apply(FocusedMsg{Focus: m.Focus(), Msg: markers})
	if !m.syncedPods || !m.syncedDeploys || !m.syncedEvents {
		t.Fatal("empty caches never completed")
	}
	if len(m.pods)+len(m.deployments)+len(m.events) != 0 || len(m.restartBaseline) != 0 {
		t.Fatal("sync marker became a resource")
	}
	for _, tile := range m.clusterTiles(model.ClusterState{}, m.clusterStats()) {
		if tile.label != "NODES" && (tile.value == "—" || tile.sub == "syncing") {
			t.Errorf("empty synced cache: %+v", tile)
		}
	}
	apply(FocusedMsg{Focus: m.Focus(), Msg: MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now()}})
	m.focusContext("beta")
	if m.watchNamespace != "" || m.focusedMetrics.seen {
		t.Fatal("focus retained scope or metric freshness")
	}
}

func TestClusterDashboardEffectiveWatcherScope(t *testing.T) {
	m := clusterDashModel(nil)
	next, _ := m.Update(PodEventMsg{Context: "alpha", Kind: cluster.PodSynced, WatchNamespace: "prod"})
	m = next.(Model)
	m.Store.ApplyMetrics("alpha", model.MetricsFields{MetricsAvailable: false})
	st, _ := m.Store.Get("alpha")
	if m.namespace != "" || m.clusterStats().podsTotal != 4 {
		t.Fatal("effective scope did not constrain dashboard")
	}
	line := m.clusterDashIdentity(st, 160)
	if !strings.Contains(line, "ns:prod") || strings.Contains(line, "metrics unavailable") {
		t.Fatalf("identity ignored watcher scope: %s", line)
	}
	cpu, _ := m.scopedUsage()
	line = strings.Join(m.gaugeLines("CPU", st, 0, 0, nil, 0, cpu, cpuOrZero, 60), "\n")
	if !strings.Contains(line, "Σ pods 4.5") || strings.Contains(line, "metrics unavailable") {
		t.Fatalf("gauge ignored watcher scope: %s", line)
	}
	m.namespace = "default"
	if m.dashboardNamespace() != "default" {
		t.Fatal("picker did not override watcher scope")
	}
}

func TestClusterDashboardTieOrdering(t *testing.T) {
	m := New("alpha", model.NewStore(), nil)
	for _, uid := range []types.UID{"3", "2", "1"} {
		ns := "a"
		if uid == "3" {
			ns = "b"
		}
		m.pods[uid] = podRow{UID: uid, Namespace: ns, Name: "api", HasMetrics: true, CPUMilli: 10, MemBytes: 20}
		m.deployments[uid] = deploymentRow{UID: uid, Namespace: ns, Name: "api", Ready: 1, Replicas: 2}
	}
	for range 100 {
		s := m.clusterStats()
		for _, rows := range [][]podRow{s.topCPU, s.topMem} {
			for i, row := range rows {
				if row.UID != types.UID(fmt.Sprint(i+1)) {
					t.Fatalf("unstable pod tie: %+v", rows)
				}
			}
		}
		if s.unhealthy[0].text != "a/api 1/2 ready" || s.unhealthy[2].text != "b/api 1/2 ready" {
			t.Fatalf("unstable deployment tie: %+v", s.unhealthy)
		}
	}
}

func TestClusterDashboardInitErrorsAndDeploymentSeverity(t *testing.T) {
	m := New("alpha", model.NewStore(), nil)
	m.syncedPods, m.syncedDeploys = true, true
	m.pods["p"] = podRow{UID: "p", Namespace: "prod", Name: "api", Phase: "Pending", Restarts: 2,
		ContainerInfo:     []cluster.ContainerInfo{{State: cluster.ContainerWaiting, Reason: "PodInitializing"}},
		InitContainerInfo: []cluster.ContainerInfo{{State: cluster.ContainerError, Reason: "CrashLoopBackOff"}},
	}
	m.deployments["d"] = deploymentRow{UID: "d", Name: "other", Ready: 1, Replicas: 2}
	s := m.clusterStats()
	if s.crashloop != 1 || len(s.unhealthy) != 2 || !s.unhealthy[1].bad || !strings.Contains(s.unhealthy[1].text, "CrashLoopBackOff") {
		t.Fatalf("init failure hidden: %+v", s)
	}
	tile := tileByLabel(t, m.clusterTiles(model.ClusterState{}, s), "DEPLOYS")
	if !reflect.DeepEqual(tile.style.GetForeground(), m.Theme.StatusWrn.GetForeground()) {
		t.Fatal("pod failure made an unrelated deployment red")
	}
	d := m.deployments["d"]
	d.Ready = 0
	m.deployments["d"] = d
	tile = tileByLabel(t, m.clusterTiles(model.ClusterState{}, m.clusterStats()), "DEPLOYS")
	if !reflect.DeepEqual(tile.style.GetForeground(), m.Theme.StatusBad.GetForeground()) {
		t.Fatal("zero-ready deployment was not red")
	}
}

func TestClusterDashboardUnknownAndNetworkStates(t *testing.T) {
	m := clusterDashModel(nil)
	m.width, m.height = 80, 40
	m.syncedPods, m.syncedDeploys, m.syncedEvents = false, false, false
	m.clusterNetOK = false
	out := ansiRE.ReplaceAllString(m.View(), "")
	for _, want := range []string{"PODS — syncing", "DEPLOYS — syncing", "WARNINGS — syncing", "NET no network data"} {
		if !strings.Contains(out, want) {
			t.Errorf("stacked dashboard missing %q", want)
		}
	}
	st := model.ClusterState{Reach: model.ReachHealthy, NodeCount: -1}
	if tile := tileByLabel(t, m.clusterTiles(st, m.clusterStats()), "NODES"); tile.sub != "nodes not visible" {
		t.Errorf("unknown node count: %+v", tile)
	}
	for _, status := range []string{"stale", "unavailable"} {
		m.clusterNetStatus = status
		if line := m.netLines(80)[0]; !strings.Contains(line, "NET "+status) || strings.Contains(line, "↓") {
			t.Fatalf("misleading network status: %s", line)
		}
	}
	m.clusterNetOK, m.clusterNetCoverage = true, "partial 1/3 nodes · "
	if lines := m.netLines(80); len(lines) != 2 || !strings.Contains(lines[1], "partial 1/3") {
		t.Fatalf("partial rates lost coverage or drew complete history: %q", lines)
	}
	m.width = 40
	if !strings.Contains(m.View(), "partial 1/3") {
		t.Fatal("narrow layout hid partial network coverage")
	}
	if got, total := m.filterCounts(); got != 0 || total != 0 {
		t.Fatal("dashboard has table counts")
	}
	if len(m.visibleUIDs()) != 0 {
		t.Fatal("dashboard has table cursor")
	}
}
