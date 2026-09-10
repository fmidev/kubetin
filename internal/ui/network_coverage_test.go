package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func partialNetworkSnapshot() NetworkSnapshotMsg {
	return NetworkSnapshotMsg{
		Context: "alpha", At: time.Unix(100, 0),
		NodesTotal: 3, NodesScraped: 2,
		Cluster: cluster.ClusterNetwork{RXBytesPerSec: 2048, TXBytesPerSec: 1024},
		Pods:    []cluster.PodNetwork{{Namespace: "default", Name: "api", RXBytesPerSec: 2048, TXBytesPerSec: 1024}},
	}
}

func TestNetworkPartialCoverageAndRecovery(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.width, m.height = 240, 20
	m.pods["api"] = podRow{UID: "api", Namespace: "default", Name: "api"}
	m.netHistory.push(10000, 5000, time.Unix(90, 0))
	updated, _ := m.Update(partialNetworkSnapshot())
	m = updated.(Model)
	if !m.clusterNetOK || m.clusterNetRX != 2048 || !m.pods["api"].HasNetwork || m.pods["api"].NetRXBps != 2048 {
		t.Fatal("partial snapshot must retain usable measurements")
	}
	if !strings.Contains(m.renderHeaderMetrics(model.ClusterState{}), "partial 2/3 nodes") {
		t.Fatal("partial rates were displayed without coverage")
	}
	if len(m.netHistory.rx) != 1 || m.netHistory.rx[0] != 10000 {
		t.Fatal("partial totals must not enter complete-cluster history")
	}

	complete := partialNetworkSnapshot()
	complete.OK, complete.NodesScraped, complete.At = true, 3, time.Unix(110, 0)
	updated, _ = m.Update(complete)
	m = updated.(Model)
	if m.clusterNetCoverage != "" || strings.Contains(m.renderHeaderMetrics(model.ClusterState{}), "partial") || len(m.netHistory.rx) != 2 {
		t.Fatal("complete snapshot did not restore full coverage and history")
	}

	updated, _ = m.Update(partialNetworkSnapshot())
	m = updated.(Model)
	m.focusContext("beta")
	if m.clusterNetCoverage != "" || m.clusterNetOK {
		t.Fatal("focus switch retained old network coverage")
	}
	updated, _ = m.Update(partialNetworkSnapshot())
	m = updated.(Model)
	if m.clusterNetCoverage != "" || m.clusterNetOK {
		t.Fatal("old context's partial snapshot contaminated the new focus")
	}
}

func TestNetworkSnapshotClearsAbsentPodRates(t *testing.T) {
	for _, sortKey := range []SortKey{SortNetRX, SortNetTX} {
		for _, tc := range []struct {
			name            string
			complete, empty bool
		}{
			{"partial", false, false},
			{"complete", true, false},
			{"partial-empty", false, true},
			{"complete-empty", true, true},
		} {
			t.Run(sortKey.label()+"/"+tc.name, func(t *testing.T) {
				m := New("alpha", model.NewStore(), []string{"alpha"})
				m.sortKey = sortKey
				m.pods["a"] = podRow{UID: "a", Namespace: "ns", Name: "a"}
				m.pods["b"] = podRow{UID: "b", Namespace: "ns", Name: "b"}
				complete := NetworkSnapshotMsg{
					Context: "alpha", OK: true, NodesTotal: 2, NodesScraped: 2,
					Pods: []cluster.PodNetwork{
						{Namespace: "ns", Name: "a", RXBytesPerSec: 1000, TXBytesPerSec: 100},
						{Namespace: "ns", Name: "b", RXBytesPerSec: 2000, TXBytesPerSec: 200},
					},
				}
				updated, _ := m.Update(complete)
				m = updated.(Model)
				if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"a", "b"}) {
					t.Fatalf("initial network sort = %v", got)
				}
				snap := complete
				snap.OK = tc.complete
				if !tc.complete {
					snap.NodesScraped = 1
				}
				snap.Pods = snap.Pods[:1]
				if tc.empty {
					snap.Pods = nil
				}
				updated, _ = m.Update(snap)
				m = updated.(Model)
				if row := m.pods["b"]; row.HasNetwork || row.NetRXBps != 0 || row.NetTXBps != 0 {
					t.Errorf("absent pod retained HasNetwork=%t RX=%d TX=%d", row.HasNetwork, row.NetRXBps, row.NetTXBps)
				}
				if tc.empty {
					if row := m.pods["a"]; row.HasNetwork || row.NetRXBps != 0 || row.NetTXBps != 0 {
						t.Error("empty measurement did not clear all pod rates")
					}
				} else {
					if row := m.pods["a"]; !row.HasNetwork || row.NetRXBps != 1000 || row.NetTXBps != 100 {
						t.Error("present pod lost its current reading")
					}
					if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"b", "a"}) {
						t.Errorf("cleared network rate left stale cached sorting: %v", got)
					}
				}
				updated, _ = m.Update(complete)
				m = updated.(Model)
				if row := m.pods["b"]; !row.HasNetwork || row.NetRXBps != 2000 || row.NetTXBps != 200 {
					t.Fatal("recovery did not restore the missing pod's reading")
				}
				if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"a", "b"}) {
					t.Fatalf("recovery left stale cached sorting: %v", got)
				}
			})
		}
	}
}

func TestNetworkPartialCoverageFitsCanvas(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120, 200, 240} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.width, m.height = width, 24
			updated, _ := m.Update(partialNetworkSnapshot())
			out := updated.(Model).View()
			if lipgloss.Height(out) != m.height {
				t.Fatalf("height = %d, want %d", lipgloss.Height(out), m.height)
			}
			for i, line := range strings.Split(out, "\n") {
				if lipgloss.Width(line) != width {
					t.Errorf("line %d width = %d, want %d", i, lipgloss.Width(line), width)
				}
			}
			if width == 240 && !strings.Contains(out, "partial 2/3 nodes") {
				t.Fatal("coverage label missing from the rendered UI")
			}
		})
	}
}
