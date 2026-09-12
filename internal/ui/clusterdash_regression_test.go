package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDashboardRankingsFollowMetricsAndIdentity(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	apply := func(msg tea.Msg) { next, _ := m.Update(msg); m = next.(Model) }
	apply(PodEventMsg{Context: "alpha", UID: "a", Namespace: "prod", Name: "a", Phase: "Running"})
	apply(PodEventMsg{Context: "alpha", UID: "b", Namespace: "dev", Name: "b", Phase: "Running"})
	snapshot := MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{
		{Namespace: "prod", Name: "a", CPUMilli: 100, MemBytes: 200},
		{Namespace: "dev", Name: "b", CPUMilli: 200, MemBytes: 100},
	}}
	apply(snapshot)
	cpu, mem := m.dashboardPodOrder()
	if !slices.Equal(cpu, []types.UID{"b", "a"}) || !slices.Equal(mem, []types.UID{"a", "b"}) {
		t.Fatalf("wrong orders: %v/%v", cpu, mem)
	}
	apply(PodEventMsg{Context: "alpha", UID: "a", Namespace: "prod", Name: "a", Phase: "Running", Restarts: 9})
	cpuAgain, _ := m.dashboardPodOrder()
	if &cpu[0] != &cpuAgain[0] {
		t.Fatal("status-only update rebuilt ranking")
	}
	if m.clusterStats().restarts != 9 {
		t.Fatal("cached ranking hid a status update")
	}
	snapshot.Pods[0].CPUMilli = 300
	apply(snapshot)
	cpu, _ = m.dashboardPodOrder()
	if cpu[0] != "a" {
		t.Fatal("metrics update left stale ranking")
	}
	m.namespace = "dev"
	cpu, mem = m.dashboardPodOrder()
	if !slices.Equal(cpu, []types.UID{"b"}) || !slices.Equal(mem, cpu) {
		t.Fatal("scope retained foreign pods")
	}
	m.namespace = ""
	apply(PodEventMsg{Context: "alpha", UID: "a", Kind: cluster.PodDeleted})
	cpu, _ = m.dashboardPodOrder()
	if !slices.Equal(cpu, []types.UID{"b"}) {
		t.Fatal("ranking retained deleted UID")
	}
	apply(MetricsSnapshotMsg{Context: "alpha", OK: false})
	cpu, mem = m.dashboardPodOrder()
	if len(cpu)+len(mem) != 0 {
		t.Fatal("failed snapshot retained rankings")
	}
	apply(snapshot)
	m.dashboardPodOrder()
	m.focusContext("beta")
	cpu, mem = m.dashboardPodOrder()
	if len(cpu)+len(mem) != 0 {
		t.Fatal("focus retained rankings")
	}
}

func TestScopedGaugeDistinguishesMissingPartialAndZero(t *testing.T) {
	m := New("alpha", model.NewStore(), nil)
	apply := func(msg tea.Msg) { next, _ := m.Update(msg); m = next.(Model) }
	// A successful snapshot need not contain a reading for this pod.
	apply(MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{{Namespace: "prod", Name: "unrelated", CPUMilli: 500}}})
	apply(PodEventMsg{Context: "alpha", UID: "p", Namespace: "prod", Name: "api"})
	apply(PodEventMsg{Context: "alpha", Kind: cluster.PodSynced, WatchNamespace: "prod"})
	gauge := func() string {
		cpu, _ := m.scopedUsage()
		return strings.Join(m.gaugeLines("CPU", model.ClusterState{}, 0, 0, nil, 0, cpu, cpuOrZero, 60), "\n")
	}
	if out := gauge(); !strings.Contains(out, "waiting for pod metrics") || strings.Contains(out, "Σ pods 0") {
		t.Fatalf("missing metrics look measured: %s", out)
	}
	apply(MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{{Namespace: "prod", Name: "api"}}})
	if out := gauge(); !strings.Contains(out, "Σ pods 0") || strings.Contains(out, "partial") {
		t.Fatalf("measured zero lost: %s", out)
	}
	apply(PodEventMsg{Context: "alpha", UID: "q", Namespace: "prod", Name: "other"})
	if out := gauge(); !strings.Contains(out, "partial metrics: 1/2 pods") {
		t.Fatalf("partial coverage hidden: %s", out)
	}
	apply(PodEventMsg{Context: "alpha", UID: "foreign", Namespace: "dev", Name: "foreign"})
	if out := gauge(); !strings.Contains(out, "partial metrics: 1/2 pods") {
		t.Fatalf("coverage counted another namespace: %s", out)
	}
	apply(MetricsSnapshotMsg{Context: "alpha", OK: false})
	if out := gauge(); !strings.Contains(out, "metrics unavailable") {
		t.Fatalf("failed poll looks pending: %s", out)
	}
	apply(PodEventMsg{Context: "alpha", UID: "p", Kind: cluster.PodDeleted})
	apply(PodEventMsg{Context: "alpha", UID: "q", Kind: cluster.PodDeleted})
	apply(MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now()})
	if out := gauge(); !strings.Contains(out, "Σ pods 0") {
		t.Fatalf("empty namespace must be zero: %s", out)
	}
}

func TestDashboardReportsReadinessFailures(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		ready       bool
		conditions  []cluster.PodCondition
		wantIssue   bool
	}{
		{"container readiness", "Running", false, nil, true},
		{"readiness gate", "Running", true, []cluster.PodCondition{{Type: "Ready", Status: "False", Reason: "ReadinessGatesNotReady"}}, true},
		{"healthy", "Running", true, []cluster.PodCondition{{Type: "Ready", Status: "True"}}, false},
		{"completed", "Succeeded", false, []cluster.PodCondition{{Type: "Ready", Status: "False"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", model.NewStore(), nil)
			m.syncedPods, m.syncedDeploys = true, true
			info := cluster.ContainerInfo{Name: "app", Ready: tc.ready, State: cluster.ContainerWaiting}
			if tc.ready {
				info.State = cluster.ContainerReady
			}
			if tc.phase == "Succeeded" {
				info.State = cluster.ContainerTerminated
				info.Reason = "Completed"
			}
			m.pods["p"] = podRow{UID: "p", Namespace: "prod", Name: "api", Phase: corev1.PodPhase(tc.phase), CreatedAt: time.Now().Add(-time.Hour),
				Containers: []string{"app"}, ContainerInfo: []cluster.ContainerInfo{info}, Conditions: tc.conditions}
			s := m.clusterStats()
			if (len(s.unhealthy) > 0) != tc.wantIssue {
				t.Fatalf("unhealthy=%+v", s.unhealthy)
			}
			if tc.wantIssue && !strings.Contains(s.unhealthy[0].text, "prod/api NotReady") {
				t.Fatalf("unhelpful readiness reason: %+v", s.unhealthy)
			}
			if tc.wantIssue {
				m.pods["z"] = podRow{UID: "z", Namespace: "prod", Name: "z-worker", Phase: corev1.PodFailed}
				if issues := m.clusterStats().unhealthy; !issues[0].bad {
					t.Fatal("readiness warning hid a failed pod")
				}
			}
		})
	}
}
