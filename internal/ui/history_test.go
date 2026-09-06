package ui

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

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
	if d := restartDelta(pods, base); d != 0 {
		t.Fatalf("fresh baseline delta = %d, want 0", d)
	}

	// Updates keep the original floor.
	noteRestartBaseline(base, "a", 9, false)
	pods["a"] = podRow{UID: "a", Restarts: 9}
	noteRestartBaseline(base, "b", 1, false)
	pods["b"] = podRow{UID: "b", Restarts: 1}
	if d := restartDelta(pods, base); d != 3 {
		t.Errorf("delta = %d, want 3", d)
	}

	// A count that went down clamps to zero for that pod.
	pods["a"] = podRow{UID: "a", Restarts: 2}
	if d := restartDelta(pods, base); d != 1 {
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
	if d := restartDelta(pods, base); d != 0 {
		t.Errorf("delta after re-add = %d, want 0", d)
	}
}
