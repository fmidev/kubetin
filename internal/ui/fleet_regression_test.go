package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func TestFleetSummarySanitizesTerminalControls(t *testing.T) {
	for _, reach := range []model.Reach{model.ReachHealthy, model.ReachDegraded, model.ReachUnreachable, model.ReachAuthFailed} {
		t.Run(reach.String(), func(t *testing.T) {
			m := fleetTestModel()
			seedFleetCluster(m.Store, "bad", func(p *model.ProbeFields) {
				p.Reach = reach
				p.LastError = "upstream \x1b[2J\x1b]52;c;ZXZpbA==\a\nnext row"
				p.ServerVersion = "v1\x1b[2J"
			})
			out := m.View()
			for _, control := range []string{"\x1b[2J", "\x1b]52", "\a"} {
				if strings.Contains(out, control) {
					t.Fatalf("terminal control %q survived in canvas", control)
				}
			}
			assertFleetCanvas(t, m)
		})
	}
}

func assertFleetCanvas(t *testing.T, m Model) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("height = %d, want %d", len(lines), m.height)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w != m.width {
			t.Fatalf("row %d width = %d, want %d", i, w, m.width)
		}
	}
}

func TestFleetRunningNotReadyNeedsAttention(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.view, m.width, m.height = ViewFleet, 120, 24
	seedFleetCluster(m.Store, "alpha", func(p *model.ProbeFields) {
		p.PodsNotReady = 1
		p.PodsPending, p.PodsFailed, p.PodsUnknownPhase = 0, 0, 0
		p.DeploysDegraded, p.WarnEvents15m = 0, 1
	})
	out := m.View()
	if strings.Contains(out, "all clear") || !strings.Contains(out, "Running but NotReady") || !strings.Contains(out, "NEEDS ATTENTION") {
		t.Fatalf("unready running pod missing from fleet health:\n%s", out)
	}
	if got := derivePulse(m.fleetGroupsFiltered()).PodsBad; got != 1 {
		t.Fatalf("problem pods = %d, want 1", got)
	}
}

func TestFleetExpandedCardScroll(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {80, 24}, {120, 30}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := fleetTestModel()
			m.width, m.height = size[0], size[1]
			m.fleet.cursorCtx, m.fleet.expanded = "bad", "bad"
			r := cluster.FleetDetailResult{Context: "bad", At: time.Now()}
			for i := range 15 {
				r.Pods = append(r.Pods, cluster.FleetPodIssue{Namespace: "prod", Name: fmt.Sprintf("pod-%02d", i), Phase: "Failed"})
			}
			for i := range 10 {
				r.Deploys = append(r.Deploys, cluster.FleetDeployIssue{Namespace: "prod", Name: fmt.Sprintf("dep-%02d", i), Desired: 3})
			}
			for i := range 10 {
				r.Events = append(r.Events, cluster.FleetEventGroup{Reason: fmt.Sprintf("EVENT-%02d", i), Count: 1, LastSeen: time.Now()})
			}
			m.fleet.detail.result = r
			if strings.Contains(m.View(), "EVENT-09") {
				t.Fatal("fixture must overflow the initial viewport")
			}
			for range 40 {
				next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
				m = next.(Model)
				assertFleetCanvas(t, m)
			}
			if !strings.Contains(m.View(), "EVENT-09") {
				t.Fatal("last event remains unreachable")
			}
			bottom := m.fleet.scroll
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
			m = next.(Model)
			if m.fleet.scroll >= bottom {
				t.Fatal("scroll up must respond immediately at the bottom")
			}
			for range 40 {
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
				m = next.(Model)
			}
			if m.fleet.scroll != 0 || !strings.Contains(m.View(), "pod-00") {
				t.Fatal("scroll up must return to the first detail row")
			}
			m, _ = fleetPress(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
			m, _ = fleetPress(t, m, key("j"))
			m, _ = fleetPress(t, m, key("k"))
			if m.fleet.scroll != 0 {
				t.Fatal("changing clusters must reset the scroll")
			}
			m, _ = fleetPress(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
			m.fleet.detail.result = cluster.FleetDetailResult{Context: "bad", At: time.Now()}
			assertFleetCanvas(t, m)
			if !strings.Contains(m.View(), "look clean") {
				t.Fatal("shorter refresh must clamp the old scroll position")
			}
		})
	}
}

func TestFleetTruncatedDetailDoesNotClaimClean(t *testing.T) {
	m := fleetTestModel()
	m.fleet.detail.result = cluster.FleetDetailResult{At: time.Now(), Truncated: []string{"deployments", "pods"}}
	out := strings.Join(m.renderFleetDetail("", 120), "\n")
	if strings.Contains(out, "look clean") || !strings.Contains(out, "partial detail: deployments, pods") {
		t.Fatalf("incomplete result needs a visible qualifier:\n%s", out)
	}
}

func TestFleetMoveWithMissingCursor(t *testing.T) {
	m := fleetTestModel()
	m.fleet.cursorCtx = "removed"
	m.moveFleetCursor(1)
	if m.fleet.cursorCtx != "fine" {
		t.Fatalf("cursor = %q, want the row after the first", m.fleet.cursorCtx)
	}
}

func BenchmarkFleetMoveNearBottom(b *testing.B) {
	for _, count := range []int{100, 500} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store := model.NewStore()
			var names []string
			for i := range count {
				name := fmt.Sprintf("cluster-%04d", i)
				names = append(names, name)
				seedFleetCluster(store, name, nil)
			}
			m := New(names[0], store, names)
			b.ReportAllocs()
			for b.Loop() {
				m.fleet.cursorCtx = names[count-1]
				m.moveFleetCursor(-1)
			}
		})
	}
}
