package ui

import (
	"testing"

	"github.com/fmidev/kubetin/internal/cluster"
)

// TestClassifiedActions covers the four states the action menu can put
// a row into: allowed (cache hit, true), denied (cache hit, false),
// pending (cache miss with an in-flight SSAR), and unknown (cache miss,
// no SSAR yet — also rendered as pending so the user sees the menu
// resolving instead of a silent blank).
func TestClassifiedActions(t *testing.T) {
	m := New("alpha", nil, []string{"alpha"})
	ref := cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: "default", Name: "p1",
	}

	// Seed the cache: Logs allowed, Exec denied (with reason), Delete
	// pending (in-flight). Events left absent — should classify pending.
	m.permissions[cluster.PermissionKey("alpha", "get", "", "pods/log", "default")] = permState{Allowed: true}
	m.permissions[cluster.PermissionKey("alpha", "create", "", "pods/exec", "default")] = permState{
		Allowed: false,
		Reason:  "forbidden: pods/exec",
	}
	m.permissionsInFlight[cluster.PermissionKey("alpha", "delete", "", "pods", "default")] = struct{}{}

	got := m.classifiedActions(ref)

	want := map[Action]actionStatus{
		ActDescribe: actionAllowed, // ungated
		ActLogs:     actionAllowed,
		ActExec:     actionDenied,
		ActEvents:   actionPending, // not in cache and not in flight → still pending UX-wise
		ActDelete:   actionPending,
	}

	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d (got: %#v)", len(got), len(want), got)
	}
	gotByAction := map[Action]actionItem{}
	for _, it := range got {
		gotByAction[it.Action] = it
	}
	for a, ws := range want {
		g, ok := gotByAction[a]
		if !ok {
			t.Errorf("missing action %v in classifiedActions output", a)
			continue
		}
		if g.Status != ws {
			t.Errorf("action %v: status = %v, want %v", a, g.Status, ws)
		}
	}
	// Reason must flow through for the denied row so the menu can show
	// it; nothing else in the test cares about Reason.
	if gotByAction[ActExec].Reason != "forbidden: pods/exec" {
		t.Errorf("denied Reason = %q, want %q", gotByAction[ActExec].Reason, "forbidden: pods/exec")
	}
}

// TestFirstSelectable pins the cursor-placement rule: when the action
// menu re-renders (after a permission result lands), the cursor should
// land on the first row the user can actually press Enter on, not on
// row 0 if row 0 is denied/pending.
func TestFirstSelectable(t *testing.T) {
	cases := []struct {
		name string
		in   []actionItem
		want int
	}{
		{"first is allowed", []actionItem{
			{Action: ActDescribe, Status: actionAllowed},
			{Action: ActLogs, Status: actionDenied},
		}, 0},
		{"skip denied", []actionItem{
			{Action: ActDescribe, Status: actionDenied},
			{Action: ActLogs, Status: actionAllowed},
		}, 1},
		{"all denied falls back to 0", []actionItem{
			{Action: ActDescribe, Status: actionDenied},
			{Action: ActLogs, Status: actionPending},
		}, 0},
		{"empty", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstSelectable(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
