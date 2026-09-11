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

func networkFreshnessFixture(at time.Time) (Model, NetworkSnapshotMsg) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.width, m.height = 240, 24
	m.sortKey = SortNetRX
	m.pods["a"] = podRow{UID: "a", Namespace: "ns", Name: "a", Phase: "Running"}
	m.pods["b"] = podRow{UID: "b", Namespace: "ns", Name: "b", Phase: "Running"}
	snap := NetworkSnapshotMsg{
		Context: "alpha", At: at, OK: true, NodesTotal: 2, NodesScraped: 2,
		Cluster: cluster.ClusterNetwork{RXBytesPerSec: 3000, TXBytesPerSec: 300},
		Pods: []cluster.PodNetwork{
			{At: at, Namespace: "ns", Name: "a", RXBytesPerSec: 2000, TXBytesPerSec: 200},
			{At: at, Namespace: "ns", Name: "b", RXBytesPerSec: 1000, TXBytesPerSec: 100},
		},
	}
	m.applyNetworkSnapshot(snap, at)
	return m, snap
}

func TestNetworkFailureRetainsValuesButClearsAvailability(t *testing.T) {
	for _, sortKey := range []SortKey{SortNetRX, SortNetTX} {
		t.Run(sortKey.label(), func(t *testing.T) {
			at := time.Now().Add(-3 * time.Second)
			m, snap := networkFreshnessFixture(at)
			m.sortKey = sortKey
			if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"b", "a"}) {
				t.Fatal("fixture not sorted by current network rates")
			}
			updated, _ := m.Update(NetworkSnapshotMsg{Context: "alpha", At: at.Add(time.Second), Error: "forbidden"})
			m = updated.(Model)
			if m.clusterNetOK || m.clusterNetRX != 3000 || m.clusterNetTX != 300 || m.clusterNetAt != at || !m.networkExpiresAt.IsZero() {
				t.Fatal("failure did not separate retained aggregate from availability")
			}
			for _, pod := range snap.Pods {
				row := m.pods[types.UID(pod.Name)]
				if row.HasNetwork || row.NetAt != at || row.NetRXBps != pod.RXBytesPerSec || row.NetTXBps != pod.TXBytesPerSec {
					t.Fatal("failure did not retain unavailable per-pod samples")
				}
			}
			if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"a", "b"}) {
				t.Fatalf("retained stale values still determine sorting: %v", got)
			}
			if !strings.Contains(m.renderHeaderMetrics(model.ClusterState{}), "net unavailable") ||
				!strings.Contains(m.renderTable(10, 240), "stale") ||
				!strings.Contains(m.renderDashPodStatus(m.pods["a"], 120, 2), "stale") {
				t.Fatal("failure must mark the aggregate, pod table, and dashboard unavailable/stale")
			}
			if len(m.netHistory.at) != 1 {
				t.Fatal("failure added a history point")
			}
			updated, _ = m.Update(snap)
			m = updated.(Model)
			if m.clusterNetOK || m.pods["a"].HasNetwork {
				t.Fatal("a delayed pre-failure snapshot restored availability")
			}
			snap.At = at.Add(2 * time.Second)
			for i := range snap.Pods {
				snap.Pods[i].At = snap.At
			}
			updated, _ = m.Update(snap)
			m = updated.(Model)
			if !m.clusterNetOK || !m.pods["a"].HasNetwork || !m.pods["b"].HasNetwork || m.clusterNetStatus != "" || len(m.netHistory.at) != 2 {
				t.Fatal("new measurements did not restore availability and history")
			}
			if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"b", "a"}) {
				t.Fatalf("recovery did not restore rate sorting: %v", got)
			}
		})
	}
}

func TestNetworkExpiresAtEachSampleDeadline(t *testing.T) {
	at := time.Now()
	m, snap := networkFreshnessFixture(at)
	snap.Pods[1].At = at.Add(5 * time.Second)
	m.applyNetworkSnapshot(snap, at.Add(5*time.Second))
	order := m.visibleUIDs()
	before, _ := m.Update(ProbeTickMsg(at.Add(networkMaxAge - time.Nanosecond)))
	m = before.(Model)
	if !m.clusterNetOK || !m.pods["a"].HasNetwork || &m.visibleUIDs()[0] != &order[0] {
		t.Fatal("fresh state was expired or ordering rebuilt before its deadline")
	}
	expired, _ := m.Update(ProbeTickMsg(at.Add(networkMaxAge)))
	m = expired.(Model)
	if m.clusterNetOK || m.pods["a"].HasNetwork || !m.pods["b"].HasNetwork || m.clusterNetStatus != "stale" {
		t.Fatal("pod and aggregate freshness did not respect individual timestamps")
	}
	if m.pods["a"].NetRXBps != 2000 || m.clusterNetRX != 3000 || m.networkExpiresAt != at.Add(networkMaxAge+5*time.Second) {
		t.Fatal("expiry lost retained rates or the next expiry deadline")
	}
	if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"a", "b"}) {
		t.Fatalf("expiry did not invalidate cached rate ordering: %v", got)
	}
	expired, _ = m.Update(ProbeTickMsg(at.Add(networkMaxAge + 5*time.Second)))
	m = expired.(Model)
	if m.pods["b"].HasNetwork || !m.networkExpiresAt.IsZero() || len(m.netHistory.at) != 1 {
		t.Fatal("last pod did not expire without adding a false history sample")
	}
}

func TestNetworkRejectsOldOrUnknownMeasurements(t *testing.T) {
	for _, at := range []time.Time{time.Now().Add(-time.Hour), {}} {
		t.Run(at.String(), func(t *testing.T) {
			m, snap := networkFreshnessFixture(at)
			updated, _ := m.Update(snap)
			m = updated.(Model)
			if m.clusterNetOK || m.pods["a"].HasNetwork || !m.networkExpiresAt.IsZero() {
				t.Fatal("old or timestamp-free readings were marked available")
			}
			fresh := New("alpha", model.NewStore(), []string{"alpha"})
			updated, _ = fresh.Update(snap)
			if len(updated.(Model).netHistory.at) != 0 {
				t.Fatal("old or unknown measurements entered current history")
			}
		})
	}
}

func TestNetworkFocusAndLateMessagesPreserveFreshness(t *testing.T) {
	at := time.Now()
	m, snap := networkFreshnessFixture(at)
	old := snap
	old.At = at.Add(-time.Second)
	old.OK, old.NodesScraped = false, 0
	updated, _ := m.Update(old)
	m = updated.(Model)
	if !m.clusterNetOK {
		t.Fatal("an older failure replaced a newer successful measurement")
	}
	m.focusContext("beta")
	if !m.networkExpiresAt.IsZero() || !m.networkSnapshotAt.IsZero() || !m.clusterNetAt.IsZero() || m.clusterNetStatus != "" {
		t.Fatal("focus change retained old freshness state")
	}
	updated, _ = m.Update(snap)
	m = updated.(Model)
	if m.clusterNetOK || !m.networkExpiresAt.IsZero() {
		t.Fatal("old context's snapshot scheduled new freshness state")
	}
}

func TestNetworkUnavailableLayout(t *testing.T) {
	for _, width := range []int{40, 80, 120, 240} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/failed=%t", width, failed), func(t *testing.T) {
				at := time.Now()
				m, _ := networkFreshnessFixture(at)
				m.width = width
				if failed {
					m.applyNetworkSnapshot(NetworkSnapshotMsg{At: at.Add(time.Second)}, at.Add(time.Second))
				} else {
					m.expireNetwork(at.Add(networkMaxAge))
				}
				out := m.View()
				if lipgloss.Height(out) != m.height {
					t.Fatal("network status changed the canvas height")
				}
				for i, line := range strings.Split(out, "\n") {
					if lipgloss.Width(line) != width {
						t.Fatalf("line %d width=%d, want %d", i, lipgloss.Width(line), width)
					}
				}
			})
		}
	}
}
