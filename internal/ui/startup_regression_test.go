package ui

import (
	"fmt"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
	"testing"
	"time"
)

func reviewPodModel() Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 160, 40
	return m
}
func TestStartupEarlyPodMetrics(t *testing.T) {
	m := reviewPodModel()
	next, _ := m.Update(MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{{Namespace: "prod", Name: "api", CPUMilli: 123, MemBytes: 12345}}})
	m = next.(Model)
	next, _ = m.Update(PodEventMsg{Context: "alpha", Kind: cluster.PodAdded, UID: "p", Namespace: "prod", Name: "api", Phase: corev1.PodRunning})
	m = next.(Model)
	t.Logf("received metrics=%+v pod.HasMetrics=%v cpu=%d", m.focusedMetrics, m.pods["p"].HasMetrics, m.pods["p"].CPUMilli)
	if !m.pods["p"].HasMetrics {
		t.Fatal("first metrics snapshot lost when it precedes initial pod delivery")
	}
}
func TestStartupUnavailableMetricsSort(t *testing.T) {
	for _, sortKey := range []SortKey{SortCPU, SortMem} {
		t.Run(sortKey.label(), func(t *testing.T) {
			m := reviewPodModel()
			m.sortKey = sortKey
			m.sortDesc = true
			m.pods["old"] = podRow{UID: "old", Namespace: "prod", Name: "old", CPUMilli: 9000, MemBytes: 900000, HasMetrics: true}
			m.pods["live"] = podRow{UID: "live", Namespace: "prod", Name: "live", CPUMilli: 10, MemBytes: 1000, HasMetrics: true}
			m.tableUIDs(ViewPods)
			next, _ := m.Update(MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{{Namespace: "prod", Name: "live", CPUMilli: 10, MemBytes: 1000}}})
			m = next.(Model)
			ids := m.tableUIDs(ViewPods)
			t.Logf("order=%v old.HasMetrics=%v", ids, m.pods["old"].HasMetrics)
			if ids[0] != "live" {
				t.Fatal("unavailable sample sorts above largest live reading using invisible stale values")
			}
		})
	}
}
func TestStartupPodHeaderTerminalControls(t *testing.T) {
	m := reviewPodModel()
	p := model.NewProbeFields()
	p.Reach = model.ReachHealthy
	p.ServerVersion = "\x1b[2Jv1.30.0"
	m.Store.ApplyProbe("alpha", p)
	if strings.Contains(m.View(), "\x1b[2J") {
		t.Fatal("API /version clears terminal through default pod header")
	}
}
func BenchmarkPodStartup(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			events := make([]PodEventMsg, count)
			for i := range count {
				uid := fmt.Sprintf("pod-%05d", i)
				events[i] = PodEventMsg{Context: "alpha", Kind: cluster.PodAdded, UID: types.UID(uid), Name: uid, Namespace: "prod", Phase: corev1.PodRunning}
			}
			b.ReportAllocs()
			for b.Loop() {
				m := reviewPodModel()
				for start := 0; start < count; start += 64 {
					batch := ResourceBatchMsg{}
					for _, ev := range events[start:min(start+64, count)] {
						batch = append(batch, ev)
					}
					next, _ := m.Update(batch)
					m = next.(Model)
					m.View()
				}
			}
		})
	}
}

func TestStartupCachedMetricsRejectStaleAndReplacementSamples(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                      string
		sample, received, created time.Time
		want                      bool
	}{
		{"fresh", now, now, now.Add(-time.Hour), true},
		{"replacement", now.Add(-time.Second), now, now, false},
		{"stale snapshot", now, now.Add(-time.Minute), now.Add(-time.Hour), false},
		{"stale sample", now.Add(-time.Minute), now, now.Add(-time.Hour), false},
		{"missing sample timestamp", time.Time{}, now, now.Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := reviewPodModel()
			m = updateTableModel(m, MetricsSnapshotMsg{Context: "alpha", OK: true, At: tc.received, Pods: []cluster.PodMetric{{Namespace: "prod", Name: "api", At: tc.sample, CPUMilli: 123}}})
			m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "new", Namespace: "prod", Name: "api", CreatedAt: tc.created})
			if got := m.pods["new"].HasMetrics; got != tc.want {
				t.Fatalf("HasMetrics=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartupCachedMetricsClearOnFailureDeletionAndFocus(t *testing.T) {
	for _, action := range []string{"failure", "delete", "focus"} {
		t.Run(action, func(t *testing.T) {
			m := reviewPodModel()
			m = updateTableModel(m, MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now(), Pods: []cluster.PodMetric{{Namespace: "prod", Name: "api", CPUMilli: 123}}})
			switch action {
			case "failure":
				m = updateTableModel(m, MetricsSnapshotMsg{Context: "alpha", OK: false, At: time.Now()})
			case "delete":
				m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "old", Namespace: "prod", Name: "api"})
				m = updateTableModel(m, PodEventMsg{Context: "alpha", Kind: cluster.PodDeleted, UID: "old"})
			case "focus":
				m.focusContext("beta")
				m.focusContext("alpha")
			}
			m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "new", Namespace: "prod", Name: "api"})
			if m.pods["new"].HasMetrics {
				t.Fatal("old metrics reused after invalidation")
			}
		})
	}
}

func TestStartupPodSyncNoticeRecoversAndIgnoresStaleFocus(t *testing.T) {
	m := reviewPodModel()
	m.width, m.height = 40, 12
	old := m.Focus()
	m = updateTableModel(m, PodEventMsg{Context: "alpha", Kind: cluster.PodSyncDelayed})
	if !strings.Contains(m.View(), "retrying") || len(m.pods) != 0 {
		t.Fatal("missing retry notice or synthetic pod inserted")
	}
	assertFleetCanvas(t, m)
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "p", Name: "p"})
	assertFleetCanvas(t, m)
	m = updateTableModel(m, PodEventMsg{Context: "alpha", Kind: cluster.PodSynced})
	if strings.Contains(m.View(), "retrying") || !m.syncedPods {
		t.Fatal("successful sync did not clear notice")
	}
	m.focusContext("beta")
	m.focusContext("alpha")
	next, _ := m.Update(FocusedMsg{Focus: old, Msg: PodEventMsg{Context: "alpha", Kind: cluster.PodSyncDelayed}})
	m = next.(Model)
	if m.podSyncDelayed {
		t.Fatal("stale focus revived retry notice")
	}
}

func TestStartupIncrementalOrderMatchesFullRebuild(t *testing.T) {
	for sortKey := SortNamespace; sortKey < sortKeyCount; sortKey++ {
		for _, desc := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/desc=%v", sortKey.label(), desc), func(t *testing.T) {
				m := reviewPodModel()
				m.sortKey, m.sortDesc = sortKey, desc
				for i := range 150 {
					uid := types.UID(fmt.Sprintf("pod-%02d", (i*17)%43))
					ev := PodEventMsg{Context: "alpha", UID: uid, Name: fmt.Sprintf("name-%02d", i%23), Namespace: fmt.Sprintf("ns-%d", i%3), Phase: corev1.PodRunning, Restarts: int32(i % 9), CreatedAt: time.Unix(int64(i), 0), NodeName: fmt.Sprintf("node-%d", i%4)}
					if i%5 == 0 {
						ev.Kind = cluster.PodDeleted
					}
					m = updateTableModel(m, ev)
					if i%7 == 0 {
						metrics := MetricsSnapshotMsg{Context: "alpha", OK: true, At: time.Now()}
						for id, p := range m.pods {
							if len(id)+i%3 > 8 {
								metrics.Pods = append(metrics.Pods, cluster.PodMetric{Namespace: p.Namespace, Name: p.Name, CPUMilli: int64(i % 11), MemBytes: int64(i * 100)})
							}
						}
						m = updateTableModel(m, metrics)
					}
					if i%11 == 0 {
						if m.filterText == "" {
							m.filterText = "name-1"
						} else {
							m.filterText = ""
						}
					}
					if i%13 == 0 {
						if m.namespace == "" {
							m.namespace = "ns-1"
						} else {
							m.namespace = ""
						}
					}
					got, want := m.tableUIDs(ViewPods), m.buildTableUIDs(ViewPods)
					if fmt.Sprint(got) != fmt.Sprint(want) {
						t.Fatalf("step %d: incremental=%v full=%v", i, got, want)
					}
					if m.tableCount(ViewPods) != len(want) {
						t.Fatalf("step %d: stale filtered count", i)
					}
				}
			})
		}
	}
}
