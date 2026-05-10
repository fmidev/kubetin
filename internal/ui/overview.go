package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/model"
)

// overviewLineCount returns how many body lines the overview will
// render given the current store. Used by handleKey to clamp G to a
// real position rather than 99999 (which would break subsequent k).
//
// We measure by actually composing the same string structure
// renderOverview emits, then counting splits on \n. The previous
// formula-based approach (1 header + 3 per card + 1 blank) drifted
// away from reality whenever the renderer added or removed a
// newline; a render-and-count is always exact at the cost of one
// trivial composition per keystroke.
func (m Model) overviewLineCount() int {
	snap := m.Store.Snapshot()
	groups := groupByReach(snap)
	width := m.width
	if width < 20 {
		width = 80
	}
	var b strings.Builder
	for _, g := range groups {
		if len(g.clusters) == 0 {
			continue
		}
		b.WriteString(m.renderOverviewSection(g.label, g.style(m.Theme), g.clusters, width))
		b.WriteByte('\n')
	}
	return strings.Count(b.String(), "\n") + 1
}

// renderOverview produces the F1 fleet overview — every cluster as a
// card with version, node states, RTT, and CPU/MEM bars. The sidebar
// is hidden in this mode so cards span the full width.
func (m Model) renderOverview(height, width int) string {
	snap := m.Store.Snapshot()
	sort.Slice(snap, func(i, j int) bool { return sortKey(snap[i]) < sortKey(snap[j]) })

	groups := groupByReach(snap)

	var b strings.Builder
	for _, g := range groups {
		if len(g.clusters) == 0 {
			continue
		}
		b.WriteString(m.renderOverviewSection(g.label, g.style(m.Theme), g.clusters, width))
		b.WriteByte('\n')
	}

	out := b.String()
	// Apply scroll: the overview is taller than the viewport on
	// fleets > ~5 clusters, so j/k/g/G drives an integer line offset.
	lines := strings.Split(out, "\n")
	start := m.overviewScroll
	maxStart := len(lines) - height
	if maxStart < 0 {
		maxStart = 0
	}
	if start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	end := start + height
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

type overviewGroup struct {
	label    string
	reach    model.Reach
	clusters []model.ClusterState
}

func (g overviewGroup) style(th Theme) lipgloss.Style {
	switch g.reach {
	case model.ReachHealthy:
		return th.StatusOK
	case model.ReachDegraded, model.ReachConnecting:
		return th.StatusWrn
	case model.ReachUnreachable, model.ReachAuthFailed:
		return th.StatusBad
	}
	return th.StatusDim
}

// groupByReach buckets clusters into ONLINE / DEGRADED / OFFLINE /
// UNKNOWN sections in display order.
func groupByReach(snap []model.ClusterState) []overviewGroup {
	groups := []overviewGroup{
		{label: "ONLINE", reach: model.ReachHealthy},
		{label: "DEGRADED", reach: model.ReachDegraded},
		{label: "OFFLINE", reach: model.ReachUnreachable},
		{label: "AUTH FAILED", reach: model.ReachAuthFailed},
		{label: "CONNECTING", reach: model.ReachConnecting},
		{label: "UNKNOWN", reach: model.ReachUnknown},
	}
	for _, st := range snap {
		for i := range groups {
			if groups[i].reach == st.Reach {
				groups[i].clusters = append(groups[i].clusters, st)
				break
			}
		}
	}
	return groups
}

func (m Model) renderOverviewSection(label string, style lipgloss.Style, clusters []model.ClusterState, width int) string {
	var b strings.Builder
	header := style.Render(" "+label) +
		m.Theme.Dim.Render(fmt.Sprintf("  (%d)", len(clusters)))
	b.WriteString(header)
	b.WriteByte('\n')
	for _, st := range clusters {
		b.WriteString(m.renderOverviewCard(st, width))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderOverviewCard renders a single cluster as a multi-line card.
// The card has two rows:
//
//	● cluster-name        v1.34.6+rke2r3   ✓ Ready    ⏱ 362ms   ●●● 3 nodes
//	  CPU  ███████░░░  35%  1.4 / 4.0 cores      MEM  ████░░░░  28%  2.1 / 8.0 GiB
func (m Model) renderOverviewCard(st model.ClusterState, width int) string {
	dot := m.Theme.styleForReach(st.Reach).Render(st.Reach.Glyph())
	name := st.RawName
	if name == "" {
		name = st.Context
	}

	ver := st.ServerVersion
	if ver == "" {
		ver = "—"
	}
	statusBadge := readyBadge(st, m.Theme)

	rtt := "—"
	if st.ProbeLatency > 0 {
		rtt = fmt.Sprintf("%dms", st.ProbeLatency.Milliseconds())
	}

	dots := nodeDots(st, m.Theme)
	nodeLabel := ""
	if st.NodeCount > 0 {
		if st.NodeReady != st.NodeCount {
			nodeLabel = fmt.Sprintf("%d/%d nodes", st.NodeReady, st.NodeCount)
		} else {
			nodeLabel = fmt.Sprintf("%d nodes", st.NodeCount)
		}
	}

	// Header row: dot + name [pad] version | badge | rtt | dots+count
	left := fmt.Sprintf(" %s %s", dot, name)
	rightParts := []string{ver, statusBadge}
	if rtt != "—" {
		rightParts = append(rightParts, m.Theme.Dim.Render("⏱ "+rtt))
	}
	if dots != "" {
		rightParts = append(rightParts, dots+" "+m.Theme.Dim.Render(nodeLabel))
	}
	right := strings.Join(rightParts, m.Theme.Dim.Render("  ·  "))

	pad := width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if pad < 1 {
		pad = 1
	}
	row1 := left + strings.Repeat(" ", pad) + right

	// Resource bars row.
	row2 := m.renderOverviewBars(st, width)

	// Subtle separator between cards.
	sep := m.Theme.Dim.Render(strings.Repeat("─", width-1))
	return row1 + "\n" + row2 + "\n " + sep
}

func readyBadge(st model.ClusterState, th Theme) string {
	switch st.Reach {
	case model.ReachHealthy:
		return th.StatusOK.Render("✓ Ready")
	case model.ReachDegraded:
		return th.StatusWrn.Render("◐ Degraded")
	case model.ReachUnreachable:
		return th.StatusBad.Render("✕ Unreachable")
	case model.ReachAuthFailed:
		return th.StatusBad.Render("✕ Auth Failed")
	case model.ReachConnecting:
		return th.StatusWrn.Render("◐ Connecting")
	}
	return th.StatusDim.Render("○ Unknown")
}

// nodeDots renders one glyph per node coloured by Ready vs NotReady.
// We don't have per-node identity for non-focused clusters, only the
// (ready, total) counts — that's enough to render the right palette.
func nodeDots(st model.ClusterState, th Theme) string {
	if st.NodeCount <= 0 {
		return ""
	}
	const cap = 12
	total := st.NodeCount
	if total > cap {
		// Big cluster: don't render N dots, just one glyph.
		if st.NodeReady < total {
			return th.StatusWrn.Render("●")
		}
		return th.StatusOK.Render("●")
	}
	ok := st.NodeReady
	if ok > total {
		ok = total
	}
	bad := total - ok
	return th.StatusOK.Render(strings.Repeat("●", ok)) +
		th.StatusBad.Render(strings.Repeat("●", bad))
}

// renderOverviewBars draws the CPU + MEM bars for one card. Bars are
// roughly half the width each so they share the row.
func (m Model) renderOverviewBars(st model.ClusterState, width int) string {
	if !st.MetricsAvailable || st.AllocCPUMilli <= 0 {
		return "   " + m.Theme.Dim.Render("metrics unavailable")
	}

	cellsEach := (width - 12) / 4
	if cellsEach < 4 {
		cellsEach = 4
	}
	if cellsEach > 30 {
		cellsEach = 30
	}

	cpu := overviewBar("CPU", st.UsageCPUMilli, st.AllocCPUMilli, cellsEach, m.Theme,
		fmt.Sprintf("%s / %s cores", coresStr(st.UsageCPUMilli), coresStr(st.AllocCPUMilli)))
	mem := overviewBar("MEM", st.UsageMemBytes, st.AllocMemBytes, cellsEach, m.Theme,
		fmt.Sprintf("%s / %s", memStrFixed(st.UsageMemBytes), memStrFixed(st.AllocMemBytes)))
	return "   " + cpu + "    " + mem
}

func overviewBar(label string, used, alloc int64, cells int, th Theme, suffix string) string {
	p := pct(used, alloc)
	loadStyle := th.StatusOK
	switch {
	case p >= 80:
		loadStyle = th.StatusBad
	case p >= 60:
		loadStyle = th.StatusWrn
	}
	emptyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("236"))
	n := p * cells / 100
	if n < 0 {
		n = 0
	}
	if n > cells {
		n = cells
	}
	filled := loadStyle.Render(strings.Repeat("▬", n))
	empty := emptyStyle.Render(strings.Repeat("▬", cells-n))
	return fmt.Sprintf("%s %s%s %3d%%  %s",
		th.Dim.Render(label),
		filled,
		empty,
		p,
		th.Dim.Render(suffix))
}

// coresStr always returns a 5-char string so the CPU suffix has a
// stable width across rows (otherwise the MEM bar shifts when one
// cluster reads "500m" and another "1.4").
//
//	   1m → "   1m"
//	 500m → " 500m"
//	  1.4 → "  1.4"
//	200.0 → "200.0"
func coresStr(milli int64) string {
	if milli == 0 {
		return "    0"
	}
	if milli < 1000 {
		return fmt.Sprintf("%4dm", milli)
	}
	return fmt.Sprintf("%5.1f", float64(milli)/1000)
}

// memStrFixed returns the human memory string padded to 7 chars so
// the MEM suffix has a stable width.
func memStrFixed(b int64) string {
	s := formatMem(b)
	if len(s) >= 7 {
		return s
	}
	return strings.Repeat(" ", 7-len(s)) + s
}
