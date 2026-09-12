package ui

import (
	"github.com/fmidev/kubetin/internal/model"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestHistorySparklineKeepsTheWholeWindow(t *testing.T) {
	values := make([]int, 60)
	for i := 30; i < len(values); i++ {
		values[i] = 100
	}
	if got := historySparkline(values, 6); got != "▁▁▁███" {
		t.Fatalf("old half of the labelled window was dropped: %q", got)
	}
	if got := historySparkline([]int{0, 100, 0, 100}, 2); got != "▄▄" {
		t.Fatalf("history must average each bucket: %q", got)
	}
	for _, width := range []int{0, 1, 60, 80} {
		if got := historySparkline(values, width); len([]rune(got)) != width {
			t.Fatalf("width=%d: %q", width, got)
		}
	}
	m := New("alpha", model.NewStore(), nil)
	now := time.Now()
	for i, v := range values {
		m.netHistory.push(int64(v), int64(v), now.Add(time.Duration(i-59)*15*time.Second))
	}
	m.clusterNetOK = true
	if line := m.netLines(32)[1]; !strings.Contains(line, "▁") || !strings.Contains(line, "█") || !strings.Contains(line, "14m") {
		t.Fatalf("network history does not cover its labelled time range: %q", line)
	}
	st := model.ClusterState{MetricsAvailable: true, MetricsAt: now}
	if line := m.gaugeLines("CPU", st, 50, 100, values, 29*time.Minute, 0, cpuOrZero, 32)[1]; !strings.Contains(line, "▁") || !strings.Contains(line, "█") || !strings.Contains(line, "29m") {
		t.Fatalf("CPU history does not cover its labelled time range: %q", line)
	}
}

func TestNetRingDedupesCapsAndSpans(t *testing.T) {
	var r netRing
	t0 := time.Now()
	r.push(10, 1, t0)
	r.push(99, 9, t0) // same timestamp: dropped
	if len(r.rx) != 1 || r.rx[0] != 10 || r.span() != 0 {
		t.Fatalf("dedupe failed: rx %v span %v", r.rx, r.span())
	}
	for i := 1; i <= netHistoryCap*2; i++ {
		r.push(int64(i), int64(-i), t0.Add(time.Duration(i)*15*time.Second))
	}
	if len(r.rx) != netHistoryCap || len(r.tx) != netHistoryCap || len(r.at) != netHistoryCap {
		t.Fatalf("len = %d/%d/%d, want %d", len(r.rx), len(r.tx), len(r.at), netHistoryCap)
	}
	if r.rx[len(r.rx)-1] != int64(netHistoryCap*2) || r.tx[len(r.tx)-1] != int64(-netHistoryCap*2) {
		t.Errorf("ring should keep the newest samples, got tail %d/%d", r.rx[len(r.rx)-1], r.tx[len(r.tx)-1])
	}
	if want := time.Duration(netHistoryCap-1) * 15 * time.Second; r.span() != want {
		t.Errorf("span = %v, want %v", r.span(), want)
	}
}

func TestRestartDeltaCountsOnlySinceFirstSeen(t *testing.T) {
	base := make(map[types.UID]int32)
	pods := make(map[types.UID]podRow)

	// Initial list: a pod that already had restarts contributes 0.
	noteRestartBaseline(base, "a", 7, false)
	pods["a"] = podRow{UID: "a", Restarts: 7}
	noteRestartBaseline(base, "b", 0, false)
	pods["b"] = podRow{UID: "b", Restarts: 0}
	if d := restartDelta(pods, base, ""); d != 0 {
		t.Fatalf("fresh baseline delta = %d, want 0", d)
	}

	// Updates keep the original floor.
	noteRestartBaseline(base, "a", 9, false)
	pods["a"] = podRow{UID: "a", Restarts: 9}
	noteRestartBaseline(base, "b", 1, false)
	pods["b"] = podRow{UID: "b", Restarts: 1}
	if d := restartDelta(pods, base, ""); d != 3 {
		t.Errorf("delta = %d, want 3", d)
	}

	// A count that went down clamps to zero for that pod.
	pods["a"] = podRow{UID: "a", Restarts: 2}
	if d := restartDelta(pods, base, ""); d != 1 {
		t.Errorf("delta after reset = %d, want 1", d)
	}

	// Deleted pods leave, and a re-seen UID starts a new floor.
	noteRestartBaseline(base, "b", 1, true)
	delete(pods, "b")
	if _, ok := base["b"]; ok {
		t.Errorf("deleted pod still in baseline")
	}
	noteRestartBaseline(base, "b", 5, false)
	pods["b"] = podRow{UID: "b", Restarts: 5}
	if d := restartDelta(pods, base, ""); d != 0 {
		t.Errorf("delta after re-add = %d, want 0", d)
	}

	// Namespace scope: only in-scope pods contribute.
	pods["a"] = podRow{UID: "a", Namespace: "prod", Restarts: 9}
	pods["c"] = podRow{UID: "c", Namespace: "dev", Restarts: 4}
	noteRestartBaseline(base, "c", 1, false)
	if d := restartDelta(pods, base, "prod"); d != 2 {
		t.Errorf("scoped delta = %d, want 2", d)
	}
	if d := restartDelta(pods, base, "dev"); d != 3 {
		t.Errorf("scoped delta = %d, want 3", d)
	}
}
