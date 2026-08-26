package ui

import (
	"strings"
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func sidebarModel(contexts []string, w, h int) Model {
	m := New("alpha", model.NewStore(), contexts)
	m.width, m.height = w, h
	m.view = ViewPods
	m.pods["p1"] = podRow{UID: "p1", Namespace: "default", Name: "payments-api", Phase: "Running"}
	m.syncedPods = true
	return m
}

// New seeds HideSidebar from the kubeconfig: one cluster and the rail
// would list the cluster you're already looking at, which the header
// already covers (name, reach, version, node counts, bars).
func TestSidebarHiddenForSingleCluster(t *testing.T) {
	one := sidebarModel([]string{"alpha"}, 120, 20).View()
	if strings.Contains(one, "CLUSTERS") {
		t.Errorf("single-cluster render should omit the rail:\n%s", one)
	}

	many := sidebarModel([]string{"alpha", "beta", "gamma"}, 120, 20).View()
	if !strings.Contains(many, "CLUSTERS") {
		t.Errorf("multi-cluster render should keep the rail:\n%s", many)
	}
}

// Zero contexts is the same shape as one — there is no rail worth
// drawing, and the pane should not be indented by a phantom sidebar.
func TestSidebarHiddenForZeroContexts(t *testing.T) {
	if out := sidebarModel(nil, 120, 20).View(); strings.Contains(out, "CLUSTERS") {
		t.Errorf("zero-context render should omit the rail:\n%s", out)
	}
}

// The reclaimed 30 columns should reach the table, not be padding: at
// 120 cells the pod table can afford columns it has to drop at 90.
func TestSingleClusterTableGetsFullWidth(t *testing.T) {
	one := sidebarModel([]string{"alpha"}, 120, 20).View()
	many := sidebarModel([]string{"alpha", "beta"}, 120, 20).View()

	// At 120 cells the full-width table fits NODE; the same terminal
	// with a 30-column rail leaves 90, at which fitColumns drops it.
	if !strings.Contains(one, "NODE") {
		t.Errorf("single-cluster table should keep the NODE column at 120 cells:\n%s", one)
	}
	if strings.Contains(many, "NODE") {
		t.Errorf("multi-cluster table at 90 usable cells should have dropped NODE:\n%s", many)
	}
}

// The footer must not advertise a cluster switch that can't happen.
func TestFooterHintDropsTabForSingleCluster(t *testing.T) {
	if h := sidebarModel([]string{"alpha"}, 120, 20).renderFooter(); strings.Contains(h, "Tab:cluster") {
		t.Errorf("single-cluster footer should not offer Tab:cluster: %q", h)
	}
	if h := sidebarModel([]string{"alpha", "beta"}, 120, 20).renderFooter(); !strings.Contains(h, "Tab:cluster") {
		t.Errorf("multi-cluster footer should offer Tab:cluster: %q", h)
	}
}

// Tab with a single context used to re-focus the cluster already
// focused: that cancels every watcher, blanks the tables through
// PodsClearedMsg and pays for a full re-list, all for no navigation.
func TestTabIsNoOpWithSingleContext(t *testing.T) {
	m := sidebarModel([]string{"alpha"}, 120, 20)
	m.Store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachHealthy})

	called := false
	m.OnFocusChange = func(string) { called = true }

	cmd := m.cycleFocus(+1)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Errorf("cycleFocus returned a command (%T) for a single context", msg)
		}
	}
	if called {
		t.Error("cycleFocus triggered a watcher swap with nowhere to switch to")
	}
	if m.WatchedContext != "alpha" {
		t.Errorf("WatchedContext = %q, want it unchanged", m.WatchedContext)
	}
}

// …but it must still work when there is somewhere to go.
func TestTabStillSwitchesWithMultipleContexts(t *testing.T) {
	m := sidebarModel([]string{"alpha", "beta"}, 120, 20)
	m.Store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachHealthy})
	m.Store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachHealthy})

	var switched string
	m.OnFocusChange = func(c string) { switched = c }

	cmd := m.cycleFocus(+1)
	if cmd == nil {
		t.Fatal("cycleFocus returned no command with two healthy contexts")
	}
	if msg := cmd(); msg == nil {
		t.Error("expected PodsClearedMsg from the focus swap")
	} else if _, ok := msg.(PodsClearedMsg); !ok {
		t.Errorf("got %T, want PodsClearedMsg", msg)
	}
	if switched != "beta" || m.WatchedContext != "beta" {
		t.Errorf("switched to %q / watching %q, want beta", switched, m.WatchedContext)
	}
}

// C toggles the rail in both directions, from whatever the kubeconfig
// defaulted to — so the key always does something visible rather than
// no-opping on a single-cluster setup.
func TestToggleSidebarKey(t *testing.T) {
	cases := []struct {
		name     string
		contexts []string
		want     bool // rail visible after one C press
	}{
		{"fleet starts shown, C hides", []string{"alpha", "beta", "gamma"}, false},
		{"lone cluster starts hidden, C reveals", []string{"alpha"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sidebarModel(tc.contexts, 120, 20)
			before := strings.Contains(m.View(), "CLUSTERS")
			if before == tc.want {
				t.Fatalf("fixture starts in the post-toggle state; want initial visible=%v", !tc.want)
			}

			out, _ := m.handleKey(key("C"))
			if got := strings.Contains(out.(Model).View(), "CLUSTERS"); got != tc.want {
				t.Errorf("after C: rail visible = %v, want %v", got, tc.want)
			}

			back, _ := out.(Model).handleKey(key("C"))
			if got := strings.Contains(back.(Model).View(), "CLUSTERS"); got != before {
				t.Errorf("after a second C: rail visible = %v, want %v (back to start)", got, before)
			}
		})
	}
}

// Hiding the rail is cosmetic: Tab must still switch clusters, and the
// footer must still say so.
func TestHiddenRailKeepsClusterSwitching(t *testing.T) {
	m := sidebarModel([]string{"alpha", "beta"}, 120, 20)
	m.HideSidebar = true
	m.Store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachHealthy})
	m.Store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachHealthy})
	m.OnFocusChange = func(string) {}

	if !strings.Contains(m.renderFooter(), "Tab:cluster") {
		t.Error("footer dropped Tab:cluster even though two clusters are configured")
	}
	if cmd := m.cycleFocus(+1); cmd == nil {
		t.Error("cycleFocus refused to switch with the rail merely hidden")
	}
	if m.WatchedContext != "beta" {
		t.Errorf("WatchedContext = %q, want beta", m.WatchedContext)
	}
}
