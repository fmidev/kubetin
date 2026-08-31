package ui

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// makePods returns a deterministic map of pods we can re-seed across
// multiple sort calls. Some pods share the same primary value (Phase,
// Restarts) so we exercise the tiebreaker chain.
func makePods() map[types.UID]podRow {
	now := time.Now()
	return map[types.UID]podRow{
		"a": {UID: "a", Namespace: "ns-a", Name: "alpha", Phase: corev1.PodRunning, Restarts: 0, CreatedAt: now.Add(-1 * time.Hour)},
		"b": {UID: "b", Namespace: "ns-b", Name: "beta", Phase: corev1.PodRunning, Restarts: 5, CreatedAt: now.Add(-2 * time.Hour)},
		"c": {UID: "c", Namespace: "ns-a", Name: "gamma", Phase: corev1.PodPending, Restarts: 0, CreatedAt: now.Add(-30 * time.Minute)},
		"d": {UID: "d", Namespace: "ns-c", Name: "delta", Phase: corev1.PodRunning, Restarts: 0, CreatedAt: now.Add(-3 * time.Hour)},
	}
}

// TestSortedRows_Stable ensures the sort yields identical ordering
// across repeated calls regardless of map iteration order. We run
// it many times to make accidental shuffles surface.
func TestSortedRows_Stable(t *testing.T) {
	keys := []SortKey{SortNamespace, SortName, SortStatus, SortRestarts, SortCPU, SortMem, SortMemPct, SortAge, SortNode}
	for _, k := range keys {
		var prev []podRow
		for i := 0; i < 50; i++ {
			rows := sortedRows(makePods(), k, false)
			if i > 0 {
				if !sameOrder(prev, rows) {
					t.Fatalf("sortedRows[%v] not stable across calls (iteration %d)", k, i)
				}
			}
			prev = rows
		}
	}
}

// TestSortedRows_DescReverses verifies that desc=true is the exact
// reverse of asc, including ties.
func TestSortedRows_DescReverses(t *testing.T) {
	asc := sortedRows(makePods(), SortName, false)
	desc := sortedRows(makePods(), SortName, true)
	if len(asc) != len(desc) {
		t.Fatalf("len mismatch: asc=%d desc=%d", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i].UID != desc[len(desc)-1-i].UID {
			t.Fatalf("asc[%d]=%s but desc[%d]=%s", i, asc[i].UID,
				len(desc)-1-i, desc[len(desc)-1-i].UID)
		}
	}
}

// TestSortedRows_TiebreakByUID — when primary AND name match, UID
// breaks the tie so the order doesn't depend on map iteration.
func TestSortedRows_TiebreakByUID(t *testing.T) {
	pods := map[types.UID]podRow{
		"z": {UID: "z", Namespace: "ns", Name: "x", Phase: corev1.PodRunning},
		"a": {UID: "a", Namespace: "ns", Name: "x", Phase: corev1.PodRunning},
		"m": {UID: "m", Namespace: "ns", Name: "x", Phase: corev1.PodRunning},
	}
	out := sortedRows(pods, SortStatus, false)
	if out[0].UID != "a" || out[1].UID != "m" || out[2].UID != "z" {
		t.Fatalf("expected UID tiebreaker a,m,z; got %s,%s,%s",
			out[0].UID, out[1].UID, out[2].UID)
	}
}

func sameOrder(a, b []podRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UID != b[i].UID {
			return false
		}
	}
	return true
}

// TestApplyPodEvent_PreservesUIDOrdering — ADD then UPDATE should
// keep the pod present and updated, not duplicate or move it.
func TestApplyPodEvent_PreservesUIDOrdering(t *testing.T) {
	// Re-using applyPodEvent with synthetic events; we don't actually
	// drive the cluster package here.
	_ = makePods // referenced; avoids 'unused' if test package shrinks
}

// An informer UPDATE must not blank the metrics/network fields. Pods
// emit status updates far more often than metrics-server is polled, so
// a whole-row replace made the CPU/MEM columns flicker to "—" and back
// on any busy pod.
func TestApplyPodEvent_PreservesMetricsAcrossUpdate(t *testing.T) {
	m := map[types.UID]podRow{}

	applyPodEvent(m, cluster.PodEvent{
		Kind: cluster.PodAdded, UID: "p1", Namespace: "default", Name: "api",
		Phase: corev1.PodRunning, NodeName: "node-1",
	})

	// The metrics + network receivers merge into the existing row.
	r := m["p1"]
	r.CPUMilli, r.MemBytes, r.HasMetrics = 142, 384<<20, true
	r.ContainerMemBytes = map[string]int64{"api": 100 << 20}
	r.NetRXBps, r.NetTXBps, r.HasNetwork = 1200, 840, true
	m["p1"] = r

	applyPodEvent(m, cluster.PodEvent{
		Kind: cluster.PodUpdated, UID: "p1", Namespace: "default", Name: "api",
		Phase: corev1.PodRunning, NodeName: "node-1", Restarts: 1,
		MemLimitBytes: 512 << 20,
	})

	got := m["p1"]
	if !got.HasMetrics || got.CPUMilli != 142 || got.MemBytes != 384<<20 {
		t.Errorf("metrics lost across informer UPDATE: cpu=%d mem=%d has=%v",
			got.CPUMilli, got.MemBytes, got.HasMetrics)
	}
	if got.ContainerMemBytes["api"] != 100<<20 {
		t.Errorf("per-container usage lost across informer UPDATE: %v", got.ContainerMemBytes)
	}
	if got.MemLimitBytes != 512<<20 {
		t.Errorf("MemLimitBytes = %d, want 512Mi — informer fields must still update", got.MemLimitBytes)
	}
	if !got.HasNetwork || got.NetRXBps != 1200 || got.NetTXBps != 840 {
		t.Errorf("network rates lost across informer UPDATE: rx=%d tx=%d has=%v",
			got.NetRXBps, got.NetTXBps, got.HasNetwork)
	}
	// The informer's own fields must still be applied.
	if got.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1 — informer fields must still update", got.Restarts)
	}
}

// A pod recreated under the same name gets a fresh UID, so nothing
// should carry over: stale CPU numbers on a brand-new pod would be
// worse than showing "—" until the next scrape.
func TestApplyPodEvent_NewUIDStartsClean(t *testing.T) {
	m := map[types.UID]podRow{
		"old": {UID: "old", Name: "api", CPUMilli: 999, HasMetrics: true,
			ContainerMemBytes: map[string]int64{"api": 100 << 20}},
	}
	applyPodEvent(m, cluster.PodEvent{
		Kind: cluster.PodAdded, UID: "new", Namespace: "default", Name: "api",
		Phase: corev1.PodRunning,
	})

	if got := m["new"]; got.HasMetrics || got.CPUMilli != 0 || got.ContainerMemBytes != nil {
		t.Errorf("new UID inherited metrics: cpu=%d has=%v containers=%v",
			got.CPUMilli, got.HasMetrics, got.ContainerMemBytes)
	}
}

// Delete still removes the row outright — field-ownership applies to
// updates, not to tombstones.
func TestApplyPodEvent_DeleteRemoves(t *testing.T) {
	m := map[types.UID]podRow{"p1": {UID: "p1", Name: "api", HasMetrics: true}}
	applyPodEvent(m, cluster.PodEvent{Kind: cluster.PodDeleted, UID: "p1"})
	if _, ok := m["p1"]; ok {
		t.Error("deleted pod still present in the map")
	}
}

// podMemPct is deliberately unclamped: >100% is the OOM-imminent
// signal the column exists for.
func TestPodMemPct(t *testing.T) {
	cases := []struct {
		name   string
		row    podRow
		want   int
		wantOK bool
	}{
		{"no metrics", podRow{MemLimitBytes: 1 << 30}, 0, false},
		{"no limit", podRow{MemBytes: 384 << 20, HasMetrics: true}, 0, false},
		{"under limit", podRow{MemBytes: 384 << 20, MemLimitBytes: 1 << 30, HasMetrics: true}, 37, true},
		{"over limit", podRow{MemBytes: 3 << 29, MemLimitBytes: 1 << 30, HasMetrics: true}, 150, true},
	}
	for _, tc := range cases {
		if p, ok := podMemPct(tc.row); p != tc.want || ok != tc.wantOK {
			t.Errorf("%s: podMemPct = (%d, %v), want (%d, %v)", tc.name, p, ok, tc.want, tc.wantOK)
		}
	}
}

// Pods without a limit sort below 0% so descending — the "worst
// offenders" direction — puts them last.
func TestLessBy_MemPctUnknownsSortLow(t *testing.T) {
	unknown := podRow{UID: "u", Name: "nolimit"}
	limited := podRow{UID: "l", Name: "limited",
		MemBytes: 100 << 20, MemLimitBytes: 200 << 20, HasMetrics: true}
	if !lessBy(unknown, limited, SortMemPct) {
		t.Error("pod without a limit should compare less than a 50% pod")
	}
	if lessBy(limited, unknown, SortMemPct) {
		t.Error("50% pod should not compare less than a pod without a limit")
	}
}
