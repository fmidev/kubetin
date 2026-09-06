package ui

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// netHistoryCap bounds the focused cluster's network-rate history:
// 60 samples at the poller's 15s cadence is 15 minutes.
const netHistoryCap = 60

// netRing keeps the cluster-aggregate rx/tx rates of the focused
// cluster, one entry per successful NetworkSnapshot. It is per focus,
// not per context: the poller only runs for the watched cluster, so a
// context-keyed ring would show a contiguous sparkline across a gap
// nobody sampled.
type netRing struct {
	rx, tx []int64
	at     []time.Time
}

func (r *netRing) push(rx, tx int64, at time.Time) {
	if n := len(r.at); n > 0 && !at.After(r.at[n-1]) {
		return
	}
	r.rx = appendCapped(r.rx, rx, netHistoryCap)
	r.tx = appendCapped(r.tx, tx, netHistoryCap)
	r.at = appendCapped(r.at, at, netHistoryCap)
}

// span is the wall-clock distance between the oldest and newest
// sample; zero until two samples exist.
func (r netRing) span() time.Duration {
	if len(r.at) < 2 {
		return 0
	}
	return r.at[len(r.at)-1].Sub(r.at[0])
}

// focusedMetricsState is the outcome of the focused cluster's latest
// pod-metrics poll. Pod rows only carry HasMetrics, which a failed
// snapshot clears for every pod — indistinguishable from an idle
// namespace without this.
type focusedMetricsState struct {
	seen bool // at least one snapshot arrived since focus
	ok   bool
	at   time.Time
}

// noteRestartBaseline records a pod's restart count the first time
// the watcher shows it to us. The informer's initial list arrives as
// ordinary Added events with no end marker, so "restarts since the
// watch started" can only be honest per pod: whatever a pod already
// had when first seen is the floor it is measured against.
func noteRestartBaseline(baseline map[types.UID]int32, uid types.UID, restarts int32, deleted bool) {
	if deleted {
		delete(baseline, uid)
		return
	}
	if _, seen := baseline[uid]; !seen {
		baseline[uid] = restarts
	}
}

// restartDelta sums restarts accumulated by the currently cached pods
// in namespace ("" = all) since each was first seen. Pods that vanished
// take their count with them; a pod's count going down (container
// status reset) clamps to 0 rather than subtracting from the others.
func restartDelta(pods map[types.UID]podRow, baseline map[types.UID]int32, namespace string) int32 {
	var total int32
	for uid, p := range pods {
		if namespace != "" && p.Namespace != namespace {
			continue
		}
		if d := p.Restarts - baseline[uid]; d > 0 {
			total += d
		}
	}
	return total
}
