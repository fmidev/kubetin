package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

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
		{"overview/wide", 200, 50, ViewOverview, nil},
		{"overview/narrow", 80, 24, ViewOverview, nil},

		{"with-filter", 100, 30, ViewPods, func(m *Model) { m.filterText = "kube-system" }},
		{"with-filter-focused", 100, 30, ViewPods, func(m *Model) {
			m.filterFocused = true
			m.filterText = "kube"
		}},

		{"help-open", 120, 40, ViewPods, func(m *Model) { m.helpOpen = true }},
		{"describe-open", 120, 40, ViewPods, func(m *Model) {
			m.describe.open = true
			m.describe.result.YAML = strings.Repeat("yaml line\n", 60)
		}},
		{"action-menu", 120, 40, ViewPods, func(m *Model) { m.actionMenu.open = true }},
		{"exec-picker", 120, 40, ViewPods, func(m *Model) {
			m.exec.pickerOpen = true
			m.exec.containers = []string{"app", "sidecar", "istio-proxy"}
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

func truncForErr(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
