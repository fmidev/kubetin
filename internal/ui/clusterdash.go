package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// Cluster dashboard (F3): one screen for "how is this cluster doing
// right now" — KPI tiles, utilisation gauges with history, top pods,
// nodes, recent warnings, unhealthy workloads. Read-only; Tab keeps
// cycling clusters underneath it and `n` scopes it to a namespace.

// Wide-grid breakpoints, measured on the pane the dashboard is handed
// (terminal minus the cluster rail and footer), not the terminal: with
// the rail shown the grid appears from about 130×25, without it from
// 100×25.
const (
	clusterDashWideMinWidth  = 100
	clusterDashWideMinHeight = 24
	clusterDashWarnWindow    = 15 * time.Minute
	clusterDashPendingGrace  = 5 * time.Minute
	// clusterDashMetricsStale mirrors fleetMemAlertMaxAge: a sample
	// older than this is still shown but tagged, never presented as
	// current.
	clusterDashMetricsStale = 2 * time.Minute
)

type clusterDashState struct {
	returnView  View
	savedFilter string
}

func (m *Model) enterClusterDash() {
	m.clusterDash.returnView = m.view
	m.clusterDash.savedFilter = m.filterText
	m.filterText = ""
	m.filterFocused = false
	m.view = ViewCluster
}

func (m *Model) leaveClusterDash(to View) {
	m.filterText = m.clusterDash.savedFilter
	m.clusterDash.savedFilter = ""
	m.view = to
}

func (m Model) handleClusterDashKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "f3":
		m.leaveClusterDash(m.clusterDash.returnView)
		return m, nil
	case "f1":
		m.leaveClusterDash(m.clusterDash.returnView)
		m.enterFleet()
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		m.leaveClusterDash(m.clusterDash.returnView)
		return m.handleKey(k)
	case "j", "k", "g", "G", "up", "down", "home", "end",
		"/", "s", "S", "i", "d", "l", "e", "enter":
		// Row-oriented keys: the dashboard has no cursor.
		return m, nil
	}
	return m.handleKey(k)
}

// inScope applies the namespace picker to a cluster-sourced object.
func (m Model) inScope(ns string) bool {
	return m.namespace == "" || ns == m.namespace
}

// clusterStats is everything the panes derive from the watched
// cluster's caches, computed once per render so tiles and panes agree.
type clusterStats struct {
	podsTotal, running, pending, failed, unknown int
	contReady, contTotal, crashloop              int
	restarts, restartsDelta                      int32
	deploysTotal, deploysReady, degraded         int
	warnEvents, warnObjects                      int
	warnGroups                                   []eventGroup
	topCPU, topMem                               []podRow
	unhealthy                                    []workloadIssue
	nodes                                        []nodeRow
	podsByNode                                   map[string]int
}

type workloadIssue struct {
	bad  bool // ✗ (bad) vs ⚠ (warn)
	text string
}

func (m Model) clusterStats() clusterStats {
	var s clusterStats
	s.podsByNode = map[string]int{}
	now := time.Now()

	type pendingPod struct {
		podRow
		reason string
	}
	var pending []pendingPod
	var broken []workloadIssue
	for _, p := range m.pods {
		s.podsByNode[p.Node]++
		if !m.inScope(p.Namespace) {
			continue
		}
		s.podsTotal++
		switch p.Phase {
		case corev1.PodRunning:
			s.running++
		case corev1.PodPending:
			s.pending++
		case corev1.PodFailed:
			s.failed++
		case corev1.PodUnknown:
			s.unknown++
		}
		r, t := containerReadyCount(p)
		s.contReady += r
		s.contTotal += t
		s.restarts += p.Restarts
		if p.HasMetrics {
			s.topCPU = append(s.topCPU, p)
			s.topMem = append(s.topMem, p)
		}

		name := p.Namespace + "/" + p.Name
		reason := ""
		errored := false
		for _, c := range p.ContainerInfo {
			if c.Reason == "CrashLoopBackOff" {
				s.crashloop++
			}
			if c.State == cluster.ContainerError && reason == "" {
				errored = true
				reason = c.Reason
				if reason == "" {
					reason = "error"
				}
			}
			if reason == "" && c.Reason != "" && c.Reason != "Completed" {
				reason = c.Reason
			}
		}
		switch {
		case p.Phase == corev1.PodFailed || p.Phase == corev1.PodUnknown:
			broken = append(broken, workloadIssue{true, fmt.Sprintf("%s %s", name, p.Phase)})
		case errored:
			broken = append(broken, workloadIssue{true, fmt.Sprintf("%s %s ↺%d", name, reason, p.Restarts)})
		case p.Phase == corev1.PodPending && !p.CreatedAt.IsZero() && now.Sub(p.CreatedAt) > clusterDashPendingGrace:
			pending = append(pending, pendingPod{p, reason})
		}
	}
	s.restartsDelta = restartDelta(m.pods, m.restartBaseline, m.namespace)
	sort.Slice(s.topCPU, func(i, j int) bool {
		if s.topCPU[i].CPUMilli != s.topCPU[j].CPUMilli {
			return s.topCPU[i].CPUMilli > s.topCPU[j].CPUMilli
		}
		return s.topCPU[i].Name < s.topCPU[j].Name
	})
	sort.Slice(s.topMem, func(i, j int) bool {
		if s.topMem[i].MemBytes != s.topMem[j].MemBytes {
			return s.topMem[i].MemBytes > s.topMem[j].MemBytes
		}
		return s.topMem[i].Name < s.topMem[j].Name
	})

	type degradedDeploy struct {
		deploymentRow
		ratio int
	}
	var degraded []degradedDeploy
	for _, d := range m.deployments {
		if !m.inScope(d.Namespace) {
			continue
		}
		s.deploysTotal++
		if d.Ready >= d.Replicas {
			s.deploysReady++
			continue
		}
		s.degraded++
		degraded = append(degraded, degradedDeploy{d, int(d.Ready * 100 / d.Replicas)})
	}
	sort.Slice(degraded, func(i, j int) bool {
		if degraded[i].ratio != degraded[j].ratio {
			return degraded[i].ratio < degraded[j].ratio
		}
		return degraded[i].Name < degraded[j].Name
	})
	for _, d := range degraded {
		s.unhealthy = append(s.unhealthy, workloadIssue{
			bad:  d.Ready == 0,
			text: fmt.Sprintf("%s/%s %d/%d ready", d.Namespace, d.Name, d.Ready, d.Replicas),
		})
	}
	sort.Slice(broken, func(i, j int) bool { return broken[i].text < broken[j].text })
	s.unhealthy = append(s.unhealthy, broken...)
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })
	for _, p := range pending {
		text := fmt.Sprintf("%s/%s Pending %s", p.Namespace, p.Name, formatAge(p.CreatedAt))
		if p.reason != "" {
			text += " " + p.reason
		}
		s.unhealthy = append(s.unhealthy, workloadIssue{false, text})
	}

	warn := make(map[types.UID]eventRow)
	objects := map[string]struct{}{}
	for uid, e := range m.events {
		if e.Type != "Warning" || !m.inScope(e.InvolvedNs) || now.Sub(e.LastSeen) > clusterDashWarnWindow {
			continue
		}
		warn[uid] = e
		s.warnEvents++
		objects[e.InvolvedKind+"/"+e.InvolvedNs+"/"+e.InvolvedName] = struct{}{}
	}
	s.warnObjects = len(objects)
	if len(warn) > 0 {
		s.warnGroups = groupEvents(warn)
	}

	s.nodes = sortedNodeRows(m.nodes)
	return s
}

// ---- tiles -----------------------------------------------------------

type dashTile struct {
	label string
	value string
	sub   string
	style lipgloss.Style
}

func (m Model) clusterTiles(st model.ClusterState, s clusterStats) []dashTile {
	th := m.Theme
	reachable := st.Reach == model.ReachHealthy || st.Reach == model.ReachDegraded
	unknown := func(label, sub string) dashTile {
		return dashTile{label: label, value: "—", sub: sub, style: th.Dim}
	}

	var tiles []dashTile

	switch {
	case !reachable || st.NodeCount < 0:
		sub := "no probe yet"
		if !reachable {
			sub = strings.ToLower(st.Reach.String())
		}
		tiles = append(tiles, unknown("NODES", sub))
	default:
		t := dashTile{label: "NODES", value: fmt.Sprintf("%d", st.NodeCount), sub: "all ready", style: th.StatusOK}
		notReady := st.NodeCount - st.NodeReady
		// The probe counts nodes per condition, not distinct pressured
		// nodes, so name the worst condition rather than sum them.
		pressureN, pressureKind := st.NodesMemPressure, "MemoryPressure"
		if st.NodesDiskPressure > pressureN {
			pressureN, pressureKind = st.NodesDiskPressure, "DiskPressure"
		}
		if st.NodesPIDPressure > pressureN {
			pressureN, pressureKind = st.NodesPIDPressure, "PIDPressure"
		}
		switch {
		case notReady > 0:
			t.value = fmt.Sprintf("%d/%d", st.NodeReady, st.NodeCount)
			t.sub = fmt.Sprintf("%d not ready", notReady)
			t.style = th.StatusBad
		case pressureN > 0:
			t.sub = fmt.Sprintf("%d %s", pressureN, pressureKind)
			t.style = th.StatusWrn
		case st.NodesCordoned > 0:
			t.sub = fmt.Sprintf("%d cordoned", st.NodesCordoned)
			t.style = th.StatusWrn
		}
		tiles = append(tiles, t)
	}

	if !m.syncedPods {
		tiles = append(tiles, unknown("PODS", "syncing"))
	} else {
		t := dashTile{label: "PODS", value: fmt.Sprintf("%d", s.podsTotal), style: th.StatusOK}
		parts := []string{fmt.Sprintf("%d running", s.running)}
		if s.pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pend", s.pending))
			t.style = th.StatusWrn
		}
		if bad := s.failed + s.unknown; bad > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", bad))
			t.style = th.StatusBad
		}
		t.sub = strings.Join(parts, ", ")
		tiles = append(tiles, t)
	}

	if !m.syncedDeploys {
		tiles = append(tiles, unknown("DEPLOYS", "syncing"))
	} else {
		t := dashTile{label: "DEPLOYS", value: fmt.Sprintf("%d/%d", s.deploysReady, s.deploysTotal), sub: "all available", style: th.StatusOK}
		if s.degraded > 0 {
			t.sub = fmt.Sprintf("%d degraded", s.degraded)
			t.style = th.StatusWrn
			for _, u := range s.unhealthy {
				if u.bad {
					t.style = th.StatusBad
					break
				}
			}
		}
		tiles = append(tiles, t)
	}

	if !m.syncedPods {
		tiles = append(tiles, unknown("CONTAINERS", "syncing"))
	} else {
		t := dashTile{label: "CONTAINERS", value: fmt.Sprintf("%d/%d", s.contReady, s.contTotal), sub: "all ready", style: th.StatusOK}
		switch {
		case s.crashloop > 0:
			t.sub = fmt.Sprintf("%d crashloop", s.crashloop)
			t.style = th.StatusBad
		case s.contReady < s.contTotal:
			t.sub = fmt.Sprintf("%d not ready", s.contTotal-s.contReady)
			t.style = th.StatusWrn
		}
		tiles = append(tiles, t)
	}

	if !m.syncedPods {
		tiles = append(tiles, unknown("RESTARTS", "syncing"))
	} else {
		t := dashTile{label: "RESTARTS", value: fmt.Sprintf("%d", s.restarts), style: th.StatusOK}
		window := "since start"
		if !m.syncStartedAt.IsZero() {
			window = "in " + formatAge(m.syncStartedAt)
		}
		if s.restartsDelta > 0 {
			t.sub = fmt.Sprintf("+%d %s", s.restartsDelta, window)
			t.style = th.StatusWrn
		} else {
			t.sub = "none " + window
		}
		tiles = append(tiles, t)
	}

	if !m.syncedEvents {
		tiles = append(tiles, unknown("WARNINGS", "syncing"))
	} else {
		t := dashTile{label: "WARNINGS", value: fmt.Sprintf("%d", s.warnEvents), sub: "none in 15m", style: th.StatusOK}
		if s.warnEvents > 0 {
			t.sub = fmt.Sprintf("15m, %s", plural(s.warnObjects, "object"))
			t.style = th.StatusWrn
		}
		tiles = append(tiles, t)
	}
	return tiles
}

func (m Model) tileLines(t dashTile, w int) []string {
	return []string{
		padCellANSI(t.style.Bold(true).Render(truncate(t.value, w)), w),
		padCol(t.sub, w, m.Theme.Dim),
	}
}

// ---- identity strip --------------------------------------------------

func (m Model) clusterDashIdentity(st model.ClusterState, w int) string {
	th := m.Theme
	display := strings.TrimSpace(st.RawName)
	if display == "" {
		display = m.WatchedContext
	}
	ns := m.namespace
	if ns == "" {
		ns = "all"
	}
	left := th.styleForReach(st.Reach).Render(st.Reach.Glyph()) + " " +
		th.Title.Render(cleanDetail(display))
	if v := cleanDetail(shortVersion(st.ServerVersion)); v != "" {
		left += th.Dim.Render(" " + v)
	}
	left += th.Dim.Render(" · ns:" + ns)
	if st.Reach != model.ReachHealthy && st.Reach != model.ReachDegraded {
		msg := strings.ToLower(st.Reach.String())
		if e := cleanDetail(st.LastError); e != "" {
			msg += ": " + e
		}
		left += th.StatusBad.Render(" · " + msg)
	}

	// Report whichever poller feeds the gauges: the fleet node-metrics
	// loop cluster-wide, the focused pod-metrics poller under a
	// namespace scope (where the fleet loop's node list is denied).
	available, at := st.MetricsAvailable, st.MetricsAt
	if m.namespace != "" {
		available, at = m.focusedMetrics.ok, m.focusedMetrics.at
	}
	var right []string
	switch {
	case m.namespace != "" && !m.focusedMetrics.seen:
		right = append(right, th.Dim.Render("metrics pending"))
	case !available:
		right = append(right, th.Dim.Render("metrics unavailable"))
	case time.Since(at) > clusterDashMetricsStale:
		right = append(right, th.StatusWrn.Render("metrics "+formatAge(at)+" ago (stale)"))
	default:
		right = append(right, th.Dim.Render("metrics "+formatAge(at)+" ago"))
	}
	if m.clusterNetOK && len(m.netHistory.at) > 0 {
		right = append(right, th.Dim.Render("net "+formatAge(m.netHistory.at[len(m.netHistory.at)-1])+" ago"))
	}
	right = append(right, th.Dim.Render(time.Now().Format("15:04:05")))
	r := strings.Join(right, th.Dim.Render(" · "))

	pad := w - lipgloss.Width(left) - lipgloss.Width(r) - 1
	if pad < 1 {
		return padCellANSI(left, w)
	}
	return padCellANSI(left+strings.Repeat(" ", pad)+r+" ", w)
}

// ---- gauges ----------------------------------------------------------

// scaleSpark maps raw samples onto 0..100 against the window maximum
// so sparkline can draw them; an all-zero window stays flat.
func scaleSpark(vals []int64) []int {
	var max int64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	out := make([]int, len(vals))
	if max <= 0 {
		return out
	}
	for i, v := range vals {
		if v < 0 {
			v = 0
		}
		out[i] = int(v * 100 / max)
	}
	return out
}

func spanLabel(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// gaugeLines renders one utilisation gauge: a bar with the absolute
// used/alloc suffix, then the history sparkline. Namespace scope has
// no allocatable to divide by, so it degrades to the summed pod usage.
func (m Model) gaugeLines(label string, st model.ClusterState, used, alloc int64, hist []int, span time.Duration,
	scopedSum int64, format func(int64) string, w int) []string {
	th := m.Theme
	if m.namespace != "" {
		fm := m.focusedMetrics
		switch {
		case !m.syncedPods || !fm.seen:
			return []string{padCol(label+" waiting for first metrics sample", w, th.Dim)}
		case !fm.ok:
			return []string{padCol(label+" metrics unavailable", w, th.Dim)}
		case time.Since(fm.at) > clusterDashMetricsStale:
			return []string{padCellANSI(th.Dim.Render(label+" ")+th.StatusWrn.Render("stale")+
				th.Dim.Render(" Σ pods "+format(scopedSum)), w)}
		}
		return []string{
			padCellANSI(th.Dim.Render(label+" ")+th.Header.Render("Σ pods "+format(scopedSum)), w),
			padCol("no allocatable for a namespace", w, th.Dim),
		}
	}
	if !st.MetricsAvailable || alloc <= 0 {
		return []string{padCol(label+" metrics unavailable", w, th.Dim)}
	}
	suffix := fmt.Sprintf("%s/%s", format(used), format(alloc))
	cells := w - (len(label) + 1 + 1 + 4 + 2 + lipgloss.Width(suffix))
	if cells < 4 {
		cells = 4
	}
	if cells > 40 {
		cells = 40
	}
	bar := overviewBar(label, used, alloc, cells, th, suffix)
	if time.Since(st.MetricsAt) > clusterDashMetricsStale {
		bar = th.Dim.Render(label+" ") + th.StatusWrn.Render("stale") + th.Dim.Render(" "+suffix)
	}
	lines := []string{padCellANSI(bar, w)}
	if sl := spanLabel(span); sl != "" && w >= 12 {
		spark := sparkline(hist, w-len(sl)-2)
		lines = append(lines, padCellANSI(th.Dim.Render(spark+"  "+sl), w))
	}
	return lines
}

func (m Model) netLines(w int) []string {
	th := m.Theme
	if !m.clusterNetOK {
		return []string{padCol("NET no network data", w, th.Dim)}
	}
	head := th.Dim.Render("NET ") + th.Header.Render("↓ "+formatRate(m.clusterNetRX)+"  ↑ "+formatRate(m.clusterNetTX))
	lines := []string{padCellANSI(head, w)}
	if sl := spanLabel(m.netHistory.span()); sl != "" && w >= 16 {
		each := (w - len(sl) - 3) / 2
		rx := sparkline(scaleSpark(m.netHistory.rx), each)
		tx := sparkline(scaleSpark(m.netHistory.tx), each)
		lines = append(lines, padCellANSI(th.Dim.Render(rx+" "+tx+"  "+sl), w))
	}
	return lines
}

// cpuOrZero / memOrZero: the table formatters print "—" for zero
// because a zero reading there means "no sample"; in a gauge suffix the
// sample exists and zero is the honest number.
func cpuOrZero(v int64) string {
	if v == 0 {
		return "0"
	}
	return formatCPU(v)
}

func memOrZero(v int64) string {
	if v == 0 {
		return "0"
	}
	return formatMem(v)
}

func (m Model) scopedUsage() (cpu, mem int64) {
	for _, p := range m.pods {
		if p.HasMetrics && m.inScope(p.Namespace) {
			cpu += p.CPUMilli
			mem += p.MemBytes
		}
	}
	return cpu, mem
}

// ---- list panes ------------------------------------------------------

func (m Model) topPodLines(pods []podRow, w, h int) []string {
	th := m.Theme
	if h < 1 {
		return nil
	}
	if len(pods) == 0 {
		switch {
		case !m.syncedPods:
			return []string{padCol("syncing", w, th.Dim)}
		case !m.focusedMetrics.seen:
			return []string{padCol("waiting for first metrics sample", w, th.Dim)}
		case !m.focusedMetrics.ok:
			return []string{padCol("metrics unavailable", w, th.Dim)}
		default:
			return []string{padCol("no pod metrics in scope", w, th.Dim)}
		}
	}
	showPct := w >= 40
	fixed := 1 + 6 + 1 + 7
	if showPct {
		fixed += 1 + 5
	}
	nameW := w - fixed
	if nameW < 6 {
		nameW = 6
		showPct = false
	}
	var out []string
	for _, p := range pods {
		if len(out) >= h {
			break
		}
		line := padCol(cleanDetail(p.Namespace+"/"+p.Name), nameW, th.Base) +
			" " + padColRight(formatCPU(p.CPUMilli), 6, th.Base) +
			" " + padColRight(formatMem(p.MemBytes), 7, th.Base)
		if showPct {
			cell := padColRight("", 5, th.Dim)
			if pc, ok := podMemPct(p); ok {
				style := th.loadStyle(pc)
				cell = padColRight(fmt.Sprintf("%d%%", pc), 5, style)
			}
			line += " " + cell
		}
		out = append(out, padCellANSI(line, w))
	}
	return out
}

func (m Model) nodeLines(st model.ClusterState, s clusterStats, w, h int) []string {
	th := m.Theme
	if h < 1 {
		return nil
	}
	if len(s.nodes) == 0 {
		switch {
		case m.syncedNodes:
			return []string{padCol("no nodes", w, th.Dim)}
		case st.NodeCount < 0 && st.Reach == model.ReachHealthy:
			// The probe could not list nodes either: namespace-scoped
			// credentials, so no node watcher runs for this context.
			return []string{padCol("nodes not visible with namespace-scoped access", w, th.Dim)}
		default:
			return []string{padCol("syncing", w, th.Dim)}
		}
	}
	bars := w >= 46
	fixed := 2 + 1 + 4 + 1 + 4 + 1 + 5 // dot, cpu%, mem%, pods
	if bars {
		fixed += 2 * 6
	}
	nameW := w - fixed
	if nameW < 6 {
		nameW = 6
	}
	pctCell := func(used, alloc int64, has bool) string {
		if !has || alloc <= 0 {
			cell := padColRight("—", 4, th.Dim)
			if bars {
				cell = strings.Repeat(" ", 6) + cell
			}
			return cell
		}
		p := pct(used, alloc)
		cell := padColRight(fmt.Sprintf("%d%%", p), 4, th.loadStyle(p))
		if bars {
			cell = bar(p, 5, th) + " " + cell
		}
		return cell
	}
	var out []string
	for _, n := range s.nodes {
		if len(out) >= h {
			break
		}
		dot := th.StatusOK.Render("●")
		switch {
		case !n.Ready:
			dot = th.StatusBad.Render("●")
		case !n.Schedulable:
			dot = th.StatusWrn.Render("●")
		}
		line := dot + " " + padCol(cleanDetail(n.Name), nameW, th.Base) +
			" " + pctCell(n.CPUMilli, n.AllocCPUMilli, n.HasMetrics) +
			" " + pctCell(n.MemBytes, n.AllocMemBytes, n.HasMetrics) +
			" " + padColRight(fmt.Sprintf("%dp", s.podsByNode[n.Name]), 5, th.Dim)
		out = append(out, padCellANSI(line, w))
	}
	return out
}

func (m Model) warningLines(s clusterStats, w, h int) []string {
	th := m.Theme
	if h < 1 {
		return nil
	}
	if len(s.warnGroups) == 0 {
		if !m.syncedEvents {
			return []string{padCol("syncing", w, th.Dim)}
		}
		return []string{padCol("no warning events in 15m", w, th.StatusOK)}
	}
	var out []string
	for _, g := range s.warnGroups {
		if len(out) >= h {
			break
		}
		obj := cleanDetail(g.InvolvedNs + "/" + g.InvolvedName)
		if g.InvolvedNs == "" {
			obj = cleanDetail(g.InvolvedName)
		}
		count := ""
		if g.Count > 1 {
			count = fmt.Sprintf("×%d", g.Count)
		}
		line := padColRight(formatAge(g.LastSeen), 3, th.Dim) +
			" " + padCol(cleanDetail(g.Reason), 14, th.StatusWrn)
		rest := w - 3 - 1 - 14
		switch {
		case rest >= 40:
			objW := rest / 3
			if objW > 32 {
				objW = 32
			}
			line += " " + padCol(obj, objW, th.Base) +
				" " + padColRight(count, 4, th.Dim) +
				" " + padCol(cleanDetail(g.Message), rest-objW-7, th.Dim)
		case rest >= 8:
			line += " " + padCol(obj, rest-1, th.Base)
		}
		out = append(out, padCellANSI(line, w))
	}
	return out
}

func (m Model) unhealthyLines(s clusterStats, w, h int) []string {
	th := m.Theme
	if h < 1 {
		return nil
	}
	if len(s.unhealthy) == 0 {
		if !m.syncedPods || !m.syncedDeploys {
			return []string{padCol("syncing", w, th.Dim)}
		}
		return []string{padCol("all watched workloads healthy", w, th.StatusOK)}
	}
	var out []string
	for _, u := range s.unhealthy {
		if len(out) >= h {
			break
		}
		glyph := th.StatusWrn.Render("⚠")
		if u.bad {
			glyph = th.StatusBad.Render("✗")
		}
		out = append(out, padCellANSI(glyph+" "+padCol(cleanDetail(u.text), w-2, th.Base), w))
	}
	return out
}

// ---- assembly --------------------------------------------------------

func (m Model) renderClusterDash(height, width int) string {
	st, _ := m.Store.Get(m.WatchedContext)
	s := m.clusterStats()
	if width >= clusterDashWideMinWidth && height >= clusterDashWideMinHeight {
		return clampCanvas(m.renderClusterDashWide(st, s, width, height), width, height)
	}
	return clampCanvas(m.renderClusterDashStacked(st, s, width, height), width, height)
}

func (m Model) renderClusterDashWide(st model.ClusterState, s clusterStats, w, h int) string {
	th := m.Theme
	tiles := m.clusterTiles(st, s)
	// One-cell gutter so the frame's left border doesn't fuse with the
	// cluster rail's separator into a double line.
	w--
	innerW := w - 2
	nTiles := len(tiles)
	if maxTiles := innerW / 14; maxTiles < nTiles {
		nTiles = maxTiles
	}
	// identity row + top border + 3 separators + bottom border = 6
	// rows of chrome; tiles and gauges are 2 rows each.
	flex := h - 6 - 2 - 2
	midH := flex * 55 / 100
	botH := flex - midH
	bands := []gridBand{
		{h: 2, cols: splitCols(innerW, nTiles, nil)},
		{h: 2, cols: splitCols(innerW, 3, nil)},
		{h: midH, cols: splitCols(innerW, 3, nil)},
		{h: botH, cols: splitCols(innerW, 2, []int{3, 2})},
	}
	canvas, rects := gridFrame(w, bands, th)

	title := func(bi, ci int, label string) {
		r := rects[bi][ci]
		canvas = dashPaneTitle(canvas, th.Header.Render(label), r.x+1, r.y-1)
	}
	put := func(bi, ci int, lines []string) {
		r := rects[bi][ci]
		canvas = splicePane(canvas, strings.Join(lines, "\n"), r)
	}

	for i := 0; i < nTiles; i++ {
		title(0, i, tiles[i].label)
		put(0, i, m.tileLines(tiles[i], rects[0][i].w))
	}

	ring := m.fleetTrends[m.WatchedContext]
	var cpuHist, memHist []int
	if ring != nil {
		cpuHist, memHist = ring.cpu, ring.mem
	}
	scopedCPU, scopedMem := m.scopedUsage()
	put(1, 0, m.gaugeLines("CPU", st, st.UsageCPUMilli, st.AllocCPUMilli, cpuHist, ring.span(), scopedCPU, cpuOrZero, rects[1][0].w))
	put(1, 1, m.gaugeLines("MEM", st, st.UsageMemBytes, st.AllocMemBytes, memHist, ring.span(), scopedMem, memOrZero, rects[1][1].w))
	put(1, 2, m.netLines(rects[1][2].w))

	title(2, 0, "TOP PODS · CPU")
	put(2, 0, m.topPodLines(s.topCPU, rects[2][0].w, midH))
	title(2, 1, "TOP PODS · MEM")
	put(2, 1, m.topPodLines(s.topMem, rects[2][1].w, midH))
	title(2, 2, "NODES")
	put(2, 2, m.nodeLines(st, s, rects[2][2].w, midH))

	title(3, 0, "WARNINGS · 15m")
	put(3, 0, m.warningLines(s, rects[3][0].w, botH))
	title(3, 1, "UNHEALTHY WORKLOADS")
	put(3, 1, m.unhealthyLines(s, rects[3][1].w, botH))

	return " " + m.clusterDashIdentity(st, w) + "\n " + strings.ReplaceAll(canvas, "\n", "\n ")
}

func (m Model) renderClusterDashStacked(st model.ClusterState, s clusterStats, w, h int) string {
	th := m.Theme
	lines := []string{m.clusterDashIdentity(st, w)}

	// Tiles pack greedily into as few rows as fit.
	var row string
	for _, t := range m.clusterTiles(st, s) {
		cell := th.Dim.Render(t.label+" ") + t.style.Render(t.value)
		if row == "" {
			row = cell
		} else if lipgloss.Width(row)+3+lipgloss.Width(cell) <= w {
			row += th.Dim.Render(" · ") + cell
		} else {
			lines = append(lines, padCellANSI(row, w))
			row = cell
		}
	}
	if row != "" {
		lines = append(lines, padCellANSI(row, w))
	}

	ring := m.fleetTrends[m.WatchedContext]
	var cpuHist, memHist []int
	if ring != nil {
		cpuHist, memHist = ring.cpu, ring.mem
	}
	span := ring.span()
	if w < 60 {
		span = 0 // no room for a sparkline row
	}
	scopedCPU, scopedMem := m.scopedUsage()
	lines = append(lines, m.gaugeLines("CPU", st, st.UsageCPUMilli, st.AllocCPUMilli, cpuHist, span, scopedCPU, cpuOrZero, w)...)
	lines = append(lines, m.gaugeLines("MEM", st, st.UsageMemBytes, st.AllocMemBytes, memHist, span, scopedMem, memOrZero, w)...)
	if m.clusterNetOK {
		net := m.netLines(w)
		if w < 60 {
			net = net[:1]
		}
		lines = append(lines, net...)
	}

	type section struct {
		label string
		count int
		body  func(h int) []string
	}
	sections := []section{
		{"WARNINGS · 15m", len(s.warnGroups), func(h int) []string { return m.warningLines(s, w, h) }},
		{"UNHEALTHY WORKLOADS", len(s.unhealthy), func(h int) []string { return m.unhealthyLines(s, w, h) }},
		{"TOP PODS · CPU", len(s.topCPU), func(h int) []string { return m.topPodLines(s.topCPU, w, h) }},
		{"TOP PODS · MEM", len(s.topMem), func(h int) []string { return m.topPodLines(s.topMem, w, h) }},
		{"NODES", len(s.nodes), func(h int) []string { return m.nodeLines(st, s, w, h) }},
	}
	for i, sec := range sections {
		remaining := h - len(lines)
		if remaining < 2 {
			break
		}
		// Share the leftover rows across the sections still to come so
		// a long warning list cannot starve the rest; each keeps at
		// least its empty-state line.
		budget := (remaining - 1) / (len(sections) - i)
		if budget < 1 {
			budget = 1
		}
		want := sec.count
		if want < 1 {
			want = 1
		}
		if want > budget {
			want = budget
		}
		lines = append(lines, m.fleetSectionHeader(sec.label, th.Header, sec.count, w))
		lines = append(lines, sec.body(want)...)
	}
	return strings.Join(lines, "\n")
}
