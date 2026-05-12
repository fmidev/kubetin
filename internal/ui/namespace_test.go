package ui

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
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

// TestSortedNsRows covers each sort key's primary ordering plus the
// desc flag. The tiebreaker chain (Name → UID) keeps ordering total,
// so equal-key rows have a deterministic fallback — the test inputs
// give each row a unique primary value so we're really exercising the
// per-key path, not the tiebreaker.
func TestSortedNsRows(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := map[types.UID]nsRow{
		"a": {UID: "a", Name: "beta", Phase: corev1.NamespaceActive, CreatedAt: base.Add(2 * time.Hour)},
		"b": {UID: "b", Name: "alpha", Phase: corev1.NamespaceTerminating, CreatedAt: base.Add(1 * time.Hour)},
		"c": {UID: "c", Name: "gamma", Phase: corev1.NamespaceActive, CreatedAt: base},
	}
	counts := map[string]nsCount{
		"alpha": {pods: 5, deploys: 1, warnings: 0},
		"beta":  {pods: 2, deploys: 3, warnings: 7},
		"gamma": {pods: 0, deploys: 2, warnings: 1},
	}

	cases := []struct {
		name string
		key  NsSortKey
		desc bool
		want []string // expected Name order
	}{
		{"name asc", NsSortName, false, []string{"alpha", "beta", "gamma"}},
		{"name desc", NsSortName, true, []string{"gamma", "beta", "alpha"}},
		// Active < Terminating lexicographically — beta and gamma are
		// Active, alpha is Terminating. Within Active, name tiebreaks.
		{"status asc", NsSortStatus, false, []string{"beta", "gamma", "alpha"}},
		// Older first ascending; gamma is oldest.
		{"age asc", NsSortAge, false, []string{"gamma", "alpha", "beta"}},
		{"pods asc", NsSortPods, false, []string{"gamma", "beta", "alpha"}},
		{"pods desc", NsSortPods, true, []string{"alpha", "beta", "gamma"}},
		{"deps asc", NsSortDeps, false, []string{"alpha", "gamma", "beta"}},
		{"warn desc", NsSortWarn, true, []string{"beta", "gamma", "alpha"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedNsRows(rows, tc.key, tc.desc, counts)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, r := range got {
				if r.Name != tc.want[i] {
					t.Errorf("row %d = %q, want %q (full order: %v)", i, r.Name, tc.want[i], rowNames(got))
				}
			}
		})
	}
}

func rowNames(rs []nsRow) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// TestNsSortKeyNextCycle pins the cycle order — `s` walks columns
// left-to-right and wraps. Regressing this would silently swap which
// column the visible arrow lands on next.
func TestNsSortKeyNextCycle(t *testing.T) {
	want := []NsSortKey{NsSortStatus, NsSortAge, NsSortPods, NsSortDeps, NsSortWarn, NsSortName}
	k := NsSortName
	for i, w := range want {
		k = k.next()
		if k != w {
			t.Errorf("step %d: got %v, want %v", i, k, w)
		}
	}
}

// TestApplyNsEvent_Project covers the OpenShift Project mapping —
// the watcher carries ResourceKind="Project" and the openshift.io
// display-name annotation; both must land on the row so the renderer
// can swap the table title and show "name · display-name".
func TestApplyNsEvent_Project(t *testing.T) {
	m := map[types.UID]nsRow{}
	applyNsEvent(m, cluster.NamespaceEvent{
		Kind:         cluster.NsAdded,
		Context:      "ocp",
		UID:          "p1",
		Name:         "smartmetserver",
		Phase:        corev1.NamespaceActive,
		CreatedAt:    time.Now(),
		ResourceKind: "Project",
		DisplayName:  "SmartMet Server",
	})
	r, ok := m["p1"]
	if !ok {
		t.Fatal("expected p1 in map after Added event")
	}
	if r.ResourceKind != "Project" {
		t.Errorf("ResourceKind = %q, want Project", r.ResourceKind)
	}
	if r.DisplayName != "SmartMet Server" {
		t.Errorf("DisplayName = %q, want %q", r.DisplayName, "SmartMet Server")
	}
}

// TestNamespacesNoun returns "projects" if any cached row is a
// Project, otherwise "namespaces" — drives the table title and the
// empty-state placeholder.
func TestNamespacesNoun(t *testing.T) {
	t.Run("plain namespaces", func(t *testing.T) {
		m := Model{namespaces: map[types.UID]nsRow{
			"a": {UID: "a", Name: "default", ResourceKind: "Namespace"},
		}}
		if got := m.namespacesNoun(); got != "namespaces" {
			t.Errorf("got %q, want namespaces", got)
		}
	})
	t.Run("with project rows", func(t *testing.T) {
		m := Model{namespaces: map[types.UID]nsRow{
			"a": {UID: "a", Name: "default", ResourceKind: "Namespace"},
			"b": {UID: "b", Name: "smartmet", ResourceKind: "Project"},
		}}
		if got := m.namespacesNoun(); got != "projects" {
			t.Errorf("got %q, want projects", got)
		}
	})
	t.Run("empty falls back to namespaces", func(t *testing.T) {
		m := Model{namespaces: map[types.UID]nsRow{}}
		if got := m.namespacesNoun(); got != "namespaces" {
			t.Errorf("got %q, want namespaces", got)
		}
	})
}

// TestNsNameCell renders the NAME column. Plain namespaces show only
// the name; Projects with a display-name show "name · display"; no
// duplication when the display name equals the resource name.
func TestNsNameCell(t *testing.T) {
	cases := []struct {
		name string
		in   nsRow
		want string
	}{
		{"plain namespace", nsRow{Name: "default"}, "default"},
		{"project no display", nsRow{Name: "smartmet", ResourceKind: "Project"}, "smartmet"},
		{"project with display", nsRow{Name: "smartmet", DisplayName: "SmartMet Server"}, "smartmet · SmartMet Server"},
		{"display equals name", nsRow{Name: "smartmet", DisplayName: "smartmet"}, "smartmet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nsNameCell(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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
