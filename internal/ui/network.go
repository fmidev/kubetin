package ui

import (
	"fmt"
	"time"

	"github.com/fmidev/kubetin/internal/cluster"
)

// Allow three scrape intervals, including the poller's 20-second tick budget.
const networkMaxAge = 3 * cluster.NetworkInterval

func networkFresh(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) < networkMaxAge
}

func (m *Model) applyNetworkSnapshot(msg NetworkSnapshotMsg, now time.Time) {
	if !msg.At.IsZero() && msg.At.Before(m.networkSnapshotAt) {
		return
	}
	if !msg.At.IsZero() {
		m.networkSnapshotAt = msg.At
	}
	m.networkExpiresAt = time.Time{}
	if !msg.OK && msg.NodesScraped == 0 {
		m.clusterNetOK = false
		m.clusterNetStatus = "unavailable"
		for uid, row := range m.pods {
			before := row
			row.HasNetwork = false
			m.pods[uid] = row
			m.invalidatePodOrder(before, row)
		}
		return
	}
	byPod := make(map[string]cluster.PodNetwork, len(msg.Pods))
	m.clusterNetAt = msg.At
	for _, pod := range msg.Pods {
		if pod.At.IsZero() {
			pod.At = msg.At
		}
		byPod[pod.Namespace+"/"+pod.Name] = pod
		if pod.At.Before(m.clusterNetAt) {
			m.clusterNetAt = pod.At
		}
	}
	for uid, row := range m.pods {
		before := row
		if pod, ok := byPod[row.Namespace+"/"+row.Name]; ok {
			row.NetRXBps, row.NetTXBps, row.NetAt = pod.RXBytesPerSec, pod.TXBytesPerSec, pod.At
			row.HasNetwork = networkFresh(row.NetAt, now)
		} else {
			row.NetRXBps, row.NetTXBps, row.NetAt, row.HasNetwork = 0, 0, time.Time{}, false
		}
		if row.HasNetwork {
			m.scheduleNetworkExpiry(row.NetAt)
		}
		m.pods[uid] = row
		m.invalidatePodOrder(before, row)
	}
	m.clusterNetRX, m.clusterNetTX = msg.Cluster.RXBytesPerSec, msg.Cluster.TXBytesPerSec
	m.clusterNetOK = networkFresh(m.clusterNetAt, now)
	m.clusterNetStatus, m.clusterNetCoverage = "stale", ""
	if m.clusterNetAt.IsZero() {
		m.clusterNetStatus = "unavailable"
	}
	if m.clusterNetOK {
		m.clusterNetStatus = ""
		m.scheduleNetworkExpiry(m.clusterNetAt)
	}
	if !msg.OK {
		m.clusterNetCoverage = fmt.Sprintf("partial %d/%d nodes · ", msg.NodesScraped, msg.NodesTotal)
	} else if m.clusterNetOK {
		m.netHistory.push(msg.Cluster.RXBytesPerSec, msg.Cluster.TXBytesPerSec, m.clusterNetAt)
	}
}

func (m *Model) scheduleNetworkExpiry(at time.Time) {
	due := at.Add(networkMaxAge)
	if m.networkExpiresAt.IsZero() || due.Before(m.networkExpiresAt) {
		m.networkExpiresAt = due
	}
}

func (m *Model) expireNetwork(now time.Time) {
	// Most UI ticks need no pod scan; only revisit state at an expiry deadline.
	if m.networkExpiresAt.IsZero() || now.Before(m.networkExpiresAt) {
		return
	}
	m.networkExpiresAt = time.Time{}
	if m.clusterNetOK {
		m.clusterNetOK = networkFresh(m.clusterNetAt, now)
		if m.clusterNetOK {
			m.scheduleNetworkExpiry(m.clusterNetAt)
		} else {
			m.clusterNetStatus = "stale"
		}
	}
	for uid, row := range m.pods {
		if !row.HasNetwork {
			continue
		}
		before := row
		row.HasNetwork = networkFresh(row.NetAt, now)
		if row.HasNetwork {
			m.scheduleNetworkExpiry(row.NetAt)
		}
		m.pods[uid] = row
		m.invalidatePodOrder(before, row)
	}
}

func (r podRow) networkDisplay() (string, string) {
	if r.HasNetwork {
		return formatRate(r.NetRXBps), formatRate(r.NetTXBps)
	}
	if !r.NetAt.IsZero() {
		return "stale", "stale"
	}
	return "—", "—"
}

func networkSortValue(rate int64, available bool) int64 {
	if !available {
		return -1
	}
	return rate
}
