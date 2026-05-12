package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// TestViewFitsCanvas asserts the invariant that View() output never
// exceeds (m.width × m.height) — no row scrolls off the top, no row
// wraps off the right edge. We check across a matrix of widths,
// heights, and view modes because every layout regression we've hit
// has come from one specific combination triggering an overflow that
// other combinations papered over.
func TestViewFitsCanvas(t *testing.T) {
	store := model.NewStore()
	contexts := []string{"alpha", "beta", "gamma"}

	cases := []struct {
		name   string
		width  int
		height int
		view   View
		setup  func(*Model)
	}{
		{"pods/wide-tall", 200, 50, ViewPods, nil},
		{"pods/narrow-short", 80, 24, ViewPods, nil},
		{"pods/very-narrow", 60, 20, ViewPods, nil},
		{"nodes/wide-tall", 200, 50, ViewNodes, nil},
		{"nodes/narrow", 80, 24, ViewNodes, nil},
		{"deploy/wide", 200, 50, ViewDeployments, nil},
		{"events/wide", 200, 50, ViewEvents, nil},
		{"namespaces/wide", 200, 50, ViewNamespaces, nil},
		{"namespaces/narrow", 80, 24, ViewNamespaces, nil},
		{"overview/wide", 200, 50, ViewOverview, nil},
		{"overview/narrow", 80, 24, ViewOverview, nil},

		{"with-filter", 100, 30, ViewPods, func(m *Model) { m.filterText = "kube-system" }},
		{"with-filter-focused", 100, 30, ViewPods, func(m *Model) {
			m.filterFocused = true
			m.filterText = "kube"
		}},

		{"help-open", 120, 40, ViewPods, func(m *Model) { m.helpOpen = true }},
		{"rbac-open-empty", 120, 40, ViewPods, func(m *Model) { m.rbacOpen = true }},
		{"rbac-open-mixed", 120, 40, ViewPods, func(m *Model) {
			m.rbacOpen = true
			m.permissions = map[string]permState{
				"alpha|get||pods/log|":     {Allowed: true},
				"alpha|create||pods/exec|": {Allowed: false, Reason: "forbidden by RBAC"},
				"alpha|delete||pods|":      {Allowed: false, Err: "timeout"},
			}
			m.permissionsInFlight = map[string]struct{}{
				"alpha|list||events|": {},
			}
		}},
		{"rbac-open-with-ns", 120, 40, ViewPods, func(m *Model) {
			m.rbacOpen = true
			m.namespace = "kube-system"
			m.permissions = map[string]permState{
				"alpha|create||pods/exec|kube-system": {Allowed: true},
			}
		}},
		{"rbac-narrow", 70, 24, ViewPods, func(m *Model) { m.rbacOpen = true }},
		{"action-menu-denied", 120, 40, ViewPods, func(m *Model) {
			m.actionMenu.open = true
			m.actionMenu.ref.Kind = "Pod"
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
				{Action: ActLogs, Status: actionAllowed},
				{Action: ActExec, Status: actionDenied, Reason: "forbidden"},
				{Action: ActEvents, Status: actionPending},
				{Action: ActDelete, Status: actionDenied, Reason: "forbidden"},
			}
		}},
		{"action-menu-long-name", 120, 40, ViewPods, func(m *Model) {
			m.actionMenu.open = true
			m.actionMenu.ref = clusterRef("Pod", "default", strings.Repeat("x", 80))
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
				{Action: ActDelete, Status: actionAllowed},
			}
		}},
		{"action-menu-floating", 120, 40, ViewPods, func(m *Model) {
			// Seed a pod so the table behind the menu has visible
			// content. The layout test asserts dimensions; we test the
			// floating shape itself in TestActionMenuFloating below.
			m.pods["uid-1"] = podRow{UID: "uid-1", Namespace: "default", Name: "behind-menu"}
			m.actionMenu.open = true
			m.actionMenu.ref = clusterRef("Pod", "default", "behind-menu")
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
			}
		}},
		{"describe-open", 120, 40, ViewPods, func(m *Model) {
			m.describe.open = true
			m.describe.result.YAML = strings.Repeat("yaml line\n", 60)
		}},
		{"action-menu", 120, 40, ViewPods, func(m *Model) { m.actionMenu.open = true }},
		{"exec-picker", 120, 40, ViewPods, func(m *Model) {
			m.exec.pickerOpen = true
			m.exec.containers = []string{"app", "sidecar", "istio-proxy"}
		}},
		{"drain-confirm", 120, 40, ViewNodes, func(m *Model) {
			m.drainConfirm.open = true
			m.drainConfirm.node = "node-1"
		}},
		{"drain-progress", 120, 40, ViewNodes, func(m *Model) {
			m.drainProgress.open = true
			m.drainProgress.node = "node-1"
			m.drainProgress.phase = "evicting"
			m.drainProgress.current = "kube-system/coredns-abc"
			m.drainProgress.done = 3
			m.drainProgress.total = 10
			m.drainProgress.blocked = []string{
				"kube-system/etcd-0 (PDB blocked after 5 retries)",
			}
		}},
		{"ns-picker", 120, 40, ViewPods, func(m *Model) {
			m.nsPickerOpen = true
			m.nsPickerOptions = []string{"(all namespaces)", "default", "kube-system"}
		}},
		{"logs-open", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"L1", "L2", "L3"}
			m.logs.cap = 1000
			m.logs.follow = true
			m.logs.container = "main"
		}},
		{"logs-narrow", 70, 20, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"L1"}
			m.logs.cap = 100
			m.logs.follow = true
		}},
		{"logs-search-active", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"hello world", "matchme", "noise", "matchme again"}
			m.logs.cap = 100
			m.logs.follow = false
			m.logs.searchTerm = "matchme"
			m.logs.searchMatches = []int{1, 3}
			m.logs.searchIdx = 0
		}},
		{"logs-search-focused", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"a", "b", "c"}
			m.logs.cap = 100
			m.logs.follow = false
			m.logs.searchTerm = "ab"
			m.logs.searchFocused = true
		}},
		{"delete-confirm", 120, 40, ViewPods, func(m *Model) {
			m.deleteConfirm.open = true
		}},
		{"scale-confirm", 120, 40, ViewDeployments, func(m *Model) {
			m.scaleConfirm.open = true
			m.scaleConfirm.ref.Name = "my-deploy"
			m.scaleConfirm.ref.Namespace = "default"
			m.scaleConfirm.current = 3
			m.scaleConfirm.typed = "5"
		}},
		{"restart-confirm", 120, 40, ViewDeployments, func(m *Model) {
			m.restartConfirm.open = true
			m.restartConfirm.ref.Name = "my-deploy"
			m.restartConfirm.ref.Namespace = "default"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", store, contexts)
			m.width, m.height = tc.width, tc.height
			m.view = tc.view
			if tc.setup != nil {
				tc.setup(&m)
			}

			out := m.View()

			gotH := lipgloss.Height(out)
			if gotH != tc.height {
				t.Errorf("height = %d, want %d", gotH, tc.height)
			}
			for i, line := range strings.Split(out, "\n") {
				w := lipgloss.Width(line)
				if w > tc.width {
					t.Errorf("line %d width = %d, want ≤ %d (%q)", i, w, tc.width, truncForErr(line))
					break
				}
			}
		})
	}
}

// TestClampCanvasContract — clampCanvas is the geometric backbone, so
// its contract gets its own test independent of View().
func TestClampCanvasContract(t *testing.T) {
	cases := []struct {
		name string
		s    string
		w, h int
	}{
		{"shorter", "a\nb", 10, 5},
		{"taller", "a\nb\nc\nd\ne\nf\ng", 10, 4},
		{"wider", strings.Repeat("X", 30), 10, 3},
		{"trailing-newline", "a\nb\nc\n", 10, 4},
		{"empty", "", 10, 5},
		{"styled", lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("hi"), 10, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := clampCanvas(tc.s, tc.w, tc.h)
			if h := lipgloss.Height(out); h != tc.h {
				t.Errorf("height = %d, want %d", h, tc.h)
			}
			for i, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w != tc.w {
					t.Errorf("row %d width = %d, want %d", i, w, tc.w)
					break
				}
			}
		})
	}
}

// clusterRef is a tiny test helper so the action-menu cases above
// can construct a DescribeRef inline without dragging cluster types
// into every case literal.
func clusterRef(kind, namespace, name string) cluster.DescribeRef {
	return cluster.DescribeRef{Kind: kind, Namespace: namespace, Name: name}
}

// TestActionMenuFloating asserts the action menu does NOT blank out
// the underlying table — at least one row outside the menu's column
// band still carries the pod name behind it. Pins the "non-blocking
// overlay" behaviour so a regression to the old full-canvas modal
// trips the test instead of just looking wrong.
func TestActionMenuFloating(t *testing.T) {
	store := model.NewStore()
	m := New("alpha", store, []string{"alpha"})
	m.width, m.height = 200, 50
	m.pods["uid-1"] = podRow{UID: "uid-1", Namespace: "default", Name: "pod-behind-menu-zzz"}
	m.actionMenu.open = true
	m.actionMenu.ref = clusterRef("Pod", "default", "pod-behind-menu-zzz")
	m.actionMenu.options = []actionItem{
		{Action: ActDescribe, Status: actionAllowed},
	}

	out := m.View()

	// The pod's name should still appear somewhere in the rendered
	// canvas — the floating modal sits centred, leaving the body
	// columns to the left/right intact.
	if !strings.Contains(out, "pod-behind-menu-zzz") {
		t.Errorf("expected pod name visible behind floating menu, but not present in render")
	}
}

func truncForErr(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
