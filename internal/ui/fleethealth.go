package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmidev/kubetin/internal/model"
)

// Fleet alert thresholds. Deliberately above the 60/80 gradient the
// utilisation bars use — a bar may glow amber without the cluster
// claiming a NEEDS ATTENTION slot.
const (
	fleetMemWarnPct     = 80
	fleetMemCritPct     = 90
	fleetPendingWarn    = 5
	fleetWarnEventsWarn = 10
	fleetTrendCap       = 24
	// fleetMemAlertMaxAge caps how old a metrics sample may be and
	// still raise the memory alert — four missed 30s poll cycles. A
	// frozen last sample must not hold a cluster in NEEDS ATTENTION.
	fleetMemAlertMaxAge = 2 * time.Minute
)

type alertSeverity uint8

const (
	sevInfo alertSeverity = iota
	sevWarn
	sevCrit
)

// clusterAlert is one "this needs a look" finding about a cluster,
// derived purely from fleet-wide probe state. Layout-independent on
// purpose: the triage cards, the sidebar badges, and any future
// render mode (signal matrix, card grid) all read the same truth.
type clusterAlert struct {
	Sev  alertSeverity
	Text string
}

// clusterAlerts derives the alert list for one cluster, worst first.
// The -1 "unknown" sentinels never alert: not knowing is not a
// finding.
func clusterAlerts(st model.ClusterState) []clusterAlert {
	var out []clusterAlert
	add := func(sev alertSeverity, text string) {
		out = append(out, clusterAlert{Sev: sev, Text: text})
	}

	switch st.Reach {
	case model.ReachUnreachable, model.ReachAuthFailed:
		// Not alerts: being unreachable is a reachability tier, and in
		// a fleet with test / laptop / VPN-only clusters a routine
		// one. These group into OFFLINE, rendered last, so NEEDS
		// ATTENTION holds only clusters that actually want work.
		return nil
	case model.ReachConnecting, model.ReachUnknown:
		return nil
	}

	notReady := 0
	if st.NodeCount > 0 && st.NodeReady < st.NodeCount {
		notReady = st.NodeCount - st.NodeReady
		add(sevCrit, nameList(plural(notReady, "node")+" NotReady", st.NodesNotReadyNames, notReady))
	}
	// Degraded for a reason other than NotReady nodes (pod access
	// denied, zero nodes): the node alert above doesn't cover it.
	if st.Reach == model.ReachDegraded && notReady == 0 {
		add(sevWarn, withErr("degraded", st.LastError))
	}

	if st.DeploysDegraded > 0 {
		sev := sevWarn
		if st.DeploysZeroReady > 0 {
			sev = sevCrit
		}
		add(sev, nameList(plural(st.DeploysDegraded, "deployment")+" below desired",
			st.DegradedDeployNames, st.DeploysDegraded))
	}

	pressureTypes := 0
	for _, n := range []int{st.NodesMemPressure, st.NodesDiskPressure, st.NodesPIDPressure} {
		if n > 0 {
			pressureTypes++
		}
	}
	pressure := func(n int, label string) {
		if n <= 0 {
			return
		}
		text := label + " on " + plural(n, "node")
		// The name sample is the union across pressure types, so it
		// only names nodes truthfully when a single type is present.
		if pressureTypes == 1 && len(st.NodesPressureNames) > 0 {
			text = label + " on " + strings.Join(st.NodesPressureNames, ", ")
			if n > len(st.NodesPressureNames) {
				text += fmt.Sprintf(" +%d more", n-len(st.NodesPressureNames))
			}
		}
		add(sevWarn, text)
	}
	pressure(st.NodesMemPressure, "MemoryPressure")
	pressure(st.NodesDiskPressure, "DiskPressure")
	pressure(st.NodesPIDPressure, "PIDPressure")

	if st.NodesCordoned > 0 {
		add(sevWarn, plural(st.NodesCordoned, "node")+" cordoned")
	}

	if st.PodsFailed > 0 {
		add(sevWarn, plural(st.PodsFailed, "pod")+" Failed")
	}
	if st.PodsUnknownPhase > 0 {
		add(sevWarn, plural(st.PodsUnknownPhase, "pod")+" in Unknown phase")
	}
	if st.PodsPending >= fleetPendingWarn {
		add(sevWarn, fmt.Sprintf("%d pods Pending", st.PodsPending))
	}
	if st.WarnEvents15m >= fleetWarnEventsWarn {
		add(sevWarn, fmt.Sprintf("%d warning events /15m", st.WarnEvents15m))
	}

	if st.MetricsAvailable && st.AllocMemBytes > 0 && time.Since(st.MetricsAt) < fleetMemAlertMaxAge {
		p := pct(st.UsageMemBytes, st.AllocMemBytes)
		switch {
		case p >= fleetMemCritPct:
			add(sevCrit, fmt.Sprintf("memory %d%% of allocatable", p))
		case p >= fleetMemWarnPct:
			add(sevWarn, fmt.Sprintf("memory %d%% of allocatable", p))
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Sev > out[j].Sev })
	return out
}

func withErr(label, err string) string {
	if err == "" {
		return label
	}
	return label + ": " + err
}

// nameList appends a bounded name sample to a count phrase:
// "2 nodes NotReady (n2, n5)", "4 deployments below desired
// (a/d2 0/2, a/d1 3/5, … +1 more)".
func nameList(label string, names []string, total int) string {
	if len(names) == 0 {
		return label
	}
	s := label + " (" + strings.Join(names, ", ")
	if total > len(names) {
		s += fmt.Sprintf(" +%d more", total-len(names))
	}
	return s + ")"
}

func worstSeverity(alerts []clusterAlert) alertSeverity {
	worst := sevInfo
	for _, a := range alerts {
		if a.Sev > worst {
			worst = a.Sev
		}
	}
	return worst
}

func alertCounts(alerts []clusterAlert) (crit, warn int) {
	for _, a := range alerts {
		switch a.Sev {
		case sevCrit:
			crit++
		case sevWarn:
			warn++
		}
	}
	return
}

// fleetEntry pairs a cluster's state with its derived alerts so
// grouping, ordering, and rendering all judge from one derivation.
type fleetEntry struct {
	St     model.ClusterState
	Alerts []clusterAlert
}

type fleetGroups struct {
	Attention []fleetEntry // any warn/crit alert; worst first
	Healthy   []fleetEntry // alphabetical
	Starting  []fleetEntry // Connecting/Unknown; alphabetical
	Offline   []fleetEntry // Unreachable/AuthFailed; auth first, then alphabetical
}

func groupFleet(snap []model.ClusterState) fleetGroups {
	var g fleetGroups
	for _, st := range snap {
		e := fleetEntry{St: st, Alerts: clusterAlerts(st)}
		switch {
		case st.Reach == model.ReachUnreachable || st.Reach == model.ReachAuthFailed:
			g.Offline = append(g.Offline, e)
		case st.Reach == model.ReachConnecting || st.Reach == model.ReachUnknown:
			g.Starting = append(g.Starting, e)
		case worstSeverity(e.Alerts) >= sevWarn:
			g.Attention = append(g.Attention, e)
		default:
			g.Healthy = append(g.Healthy, e)
		}
	}
	sort.Slice(g.Attention, func(i, j int) bool {
		ci, wi := alertCounts(g.Attention[i].Alerts)
		cj, wj := alertCounts(g.Attention[j].Alerts)
		if ci != cj {
			return ci > cj
		}
		if wi != wj {
			return wi > wj
		}
		return g.Attention[i].St.Context < g.Attention[j].St.Context
	})
	byName := func(s []fleetEntry) {
		sort.Slice(s, func(i, j int) bool { return s[i].St.Context < s[j].St.Context })
	}
	byName(g.Healthy)
	byName(g.Starting)
	// Auth failures lead the offline tail: an expired token is the one
	// offline state a keypress (re-login) can fix.
	sort.Slice(g.Offline, func(i, j int) bool {
		ai := g.Offline[i].St.Reach == model.ReachAuthFailed
		aj := g.Offline[j].St.Reach == model.ReachAuthFailed
		if ai != aj {
			return ai
		}
		return g.Offline[i].St.Context < g.Offline[j].St.Context
	})
	return g
}

// fleetPulse is the one-line fleet summary at the top of the
// dashboard.
type fleetPulse struct {
	Clusters   int
	NeedAction int
	Nodes      int
	NodesBad   int // NotReady
	Pods       int
	PodsBad    int // Pending + Failed + Unknown
	// AllPodsKnown is false when any reachable cluster's total is a
	// -1 sentinel; the pulse then renders "N+ pods" instead of
	// claiming a fleet total it doesn't have.
	AllPodsKnown bool
	CPUPct       int
	MemPct       int
	HasMetrics   bool
	Offline      int
}

func derivePulse(g fleetGroups) fleetPulse {
	p := fleetPulse{AllPodsKnown: true, NeedAction: len(g.Attention), Offline: len(g.Offline)}
	var usageCPU, allocCPU, usageMem, allocMem int64
	each := func(entries []fleetEntry) {
		for _, e := range entries {
			st := e.St
			p.Clusters++
			if st.NodeCount > 0 {
				p.Nodes += st.NodeCount
				if st.NodeReady >= 0 && st.NodeReady < st.NodeCount {
					p.NodesBad += st.NodeCount - st.NodeReady
				}
			}
			if st.PodsTotal >= 0 {
				p.Pods += st.PodsTotal
			} else {
				p.AllPodsKnown = false
			}
			for _, n := range []int{st.PodsPending, st.PodsFailed, st.PodsUnknownPhase} {
				if n > 0 {
					p.PodsBad += n
				}
			}
			if st.MetricsAvailable && st.AllocCPUMilli > 0 {
				p.HasMetrics = true
				usageCPU += st.UsageCPUMilli
				allocCPU += st.AllocCPUMilli
				usageMem += st.UsageMemBytes
				allocMem += st.AllocMemBytes
			}
		}
	}
	// Only reachable clusters contribute to the fleet totals — an
	// offline cluster's carried-forward counts are claims we can't
	// verify, and a starting one has nothing yet.
	each(g.Attention)
	each(g.Healthy)
	p.Clusters += len(g.Starting) + len(g.Offline)
	p.CPUPct = pct(usageCPU, allocCPU)
	p.MemPct = pct(usageMem, allocMem)
	return p
}

// trendRing holds the last fleetTrendCap memory-utilisation samples
// for one cluster, deduplicated on the metrics timestamp so the 1 Hz
// UI tick doesn't record the same 30s sample thirty times.
type trendRing struct {
	vals   []int
	lastAt time.Time
}

func (r *trendRing) push(v int, at time.Time) {
	if !at.After(r.lastAt) {
		return
	}
	r.lastAt = at
	r.vals = append(r.vals, v)
	if len(r.vals) > fleetTrendCap {
		r.vals = append(r.vals[:0], r.vals[len(r.vals)-fleetTrendCap:]...)
	}
}

var sparkGlyphs = []rune("▁▂▃▄▅▆▇█")

// sparkline renders the last `width` samples right-aligned in a
// width-cell field. Fewer than two samples is no trend yet: blanks,
// so columns don't jitter while history accumulates.
func sparkline(vals []int, width int) string {
	if width <= 0 {
		return ""
	}
	if len(vals) < 2 {
		return strings.Repeat(" ", width)
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	var b strings.Builder
	for i := 0; i < width-len(vals); i++ {
		b.WriteByte(' ')
	}
	for _, v := range vals {
		idx := v * (len(sparkGlyphs) - 1) / 100
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkGlyphs) {
			idx = len(sparkGlyphs) - 1
		}
		b.WriteRune(sparkGlyphs[idx])
	}
	return b.String()
}
