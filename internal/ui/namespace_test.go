package ui

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// TestCollectNsCounts pins the three documented invariants of the
// resource-count column:
//
//  1. Pods and Deployments count by Namespace (one row per cache entry).
//  2. Only Warning-class events count — Normal events are noise and
//     would dominate the WRN cell.
//  3. The walk is a single pass per cache; the output map's keys are
//     namespaces that have at least one matching entry.
func TestCollectNsCounts(t *testing.T) {
	m := Model{
		pods: map[types.UID]podRow{
			"p1": {UID: "p1", Namespace: "default"},
			"p2": {UID: "p2", Namespace: "default"},
			"p3": {UID: "p3", Namespace: "kube-system"},
		},
		deployments: map[types.UID]deploymentRow{
			"d1": {UID: "d1", Namespace: "default"},
			"d2": {UID: "d2", Namespace: "kube-system"},
			"d3": {UID: "d3", Namespace: "kube-system"},
		},
		events: map[types.UID]eventRow{
			// Only Warning events should count. Two warnings in
			// kube-system, one normal that we expect to be ignored.
			"e1": {UID: "e1", Namespace: "kube-system", Type: "Warning"},
			"e2": {UID: "e2", Namespace: "kube-system", Type: "Warning"},
			"e3": {UID: "e3", Namespace: "default", Type: "Normal"},
			"e4": {UID: "e4", Namespace: "default", Type: "Warning"},
		},
	}

	got := m.collectNsCounts()

	want := map[string]nsCount{
		"default":     {pods: 2, deploys: 1, warnings: 1},
		"kube-system": {pods: 1, deploys: 2, warnings: 2},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d namespaces, want %d (got: %#v)", len(got), len(want), got)
	}
	for ns, w := range want {
		g, ok := got[ns]
		if !ok {
			t.Errorf("missing namespace %q in counts (got: %#v)", ns, got)
			continue
		}
		if g != w {
			t.Errorf("ns %q: got %+v, want %+v", ns, g, w)
		}
	}
}

// TestCollectNsCounts_NormalEventsIgnored isolates the "only Warning
// events count" invariant from the multi-ns test above, so a regression
// to that rule (counting all events) shows up as an obvious failure
// instead of a slightly-too-high warnings count buried in a struct
// comparison.
func TestCollectNsCounts_NormalEventsIgnored(t *testing.T) {
	m := Model{
		events: map[types.UID]eventRow{
			"a": {UID: "a", Namespace: "default", Type: "Normal"},
			"b": {UID: "b", Namespace: "default", Type: "Normal"},
			"c": {UID: "c", Namespace: "default", Type: "Normal"},
		},
	}
	got := m.collectNsCounts()
	if len(got) != 0 {
		t.Fatalf("Normal-only events should not appear in counts; got %#v", got)
	}
}
