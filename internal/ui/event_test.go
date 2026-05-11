package ui

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// TestGroupEvents_OrderNewestFirst pins the documented sort: most
// recent group first, alphabetical Reason as tie-breaker, fully
// deterministic. Guards against the prior (Warning, Count, LastSeen)
// triple-key sort regressing — that's what the user-reported "order
// changes for no apparent reason" came from.
func TestGroupEvents_OrderNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	rows := map[types.UID]eventRow{
		"a": {UID: "a", Reason: "Pulled", Message: "Image x", Type: "Normal", Count: 1, LastSeen: t0.Add(5 * time.Minute)},
		"b": {UID: "b", Reason: "Failed", Message: "Image y", Type: "Warning", Count: 50, LastSeen: t0.Add(1 * time.Minute)},
		"c": {UID: "c", Reason: "Started", Message: "Image z", Type: "Normal", Count: 2, LastSeen: t0.Add(10 * time.Minute)},
	}

	got := groupEvents(rows)
	if len(got) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(got))
	}

	// Newest LastSeen at top — even though the Warning (b) used to
	// be hoisted by the old severity-first sort, and the high-count
	// b used to be hoisted by Count desc.
	wantReasons := []string{"Started", "Pulled", "Failed"}
	for i, want := range wantReasons {
		if got[i].Reason != want {
			t.Fatalf("position %d: got Reason=%q, want %q (full order: %v)",
				i, got[i].Reason, want, reasonsOf(got))
		}
	}
}

// TestGroupEvents_TieBreakerByReason ensures two groups with the
// exact same LastSeen sort alphabetically by Reason, so the order
// is fully deterministic.
func TestGroupEvents_TieBreakerByReason(t *testing.T) {
	t0 := time.Now()
	rows := map[types.UID]eventRow{
		"a": {UID: "a", Reason: "Zebra", Message: "m", LastSeen: t0},
		"b": {UID: "b", Reason: "Alpha", Message: "n", LastSeen: t0},
		"c": {UID: "c", Reason: "Mango", Message: "o", LastSeen: t0},
	}
	got := groupEvents(rows)
	wantReasons := []string{"Alpha", "Mango", "Zebra"}
	for i, want := range wantReasons {
		if got[i].Reason != want {
			t.Fatalf("position %d: got %q, want %q (full order: %v)",
				i, got[i].Reason, want, reasonsOf(got))
		}
	}
}

// TestGroupEvents_StableAcrossCalls re-runs the sort on the same
// input N times and asserts the output is byte-identical. Catches
// any future regression to sort.Slice (non-stable) or any reliance
// on map iteration order leaking into the result.
func TestGroupEvents_StableAcrossCalls(t *testing.T) {
	t0 := time.Now()
	rows := map[types.UID]eventRow{}
	for i := 0; i < 20; i++ {
		uid := types.UID(byteToHex(i))
		rows[uid] = eventRow{
			UID:      uid,
			Reason:   "Reason-" + byteToHex(i%3),
			Message:  "Message-" + byteToHex(i%3),
			LastSeen: t0.Add(time.Duration(i%5) * time.Second),
			Count:    int32(i),
		}
	}

	first := reasonsOf(groupEvents(rows))
	for i := 0; i < 10; i++ {
		got := reasonsOf(groupEvents(rows))
		if !eqStrings(first, got) {
			t.Fatalf("ordering not stable across calls:\nfirst=%v\nrun %d=%v", first, i, got)
		}
	}
}

func reasonsOf(gs []eventGroup) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Reason
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func byteToHex(n int) string {
	const hex = "0123456789abcdef"
	if n < 16 {
		return string(hex[n])
	}
	return string(hex[n/16]) + string(hex[n%16])
}
