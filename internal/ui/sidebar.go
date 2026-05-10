package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/model"
)

// SidebarWidth is the fixed width of the cluster rail.
const SidebarWidth = 30

// renderSidebar renders the cluster list. Each cluster takes one line:
//
//	▸ ● rke2-ge        v1.30  6n
//
// Reachability is encoded as glyph + color so colourblind users get
// the signal too. Errors and empty fields render as a dim placeholder.
func (m Model) renderSidebar(height int) string {
	snap := m.Store.Snapshot()
	sort.Slice(snap, func(i, j int) bool {
		return sortKey(snap[i]) < sortKey(snap[j])
	})

	header := m.Theme.Header.Render(fmt.Sprintf(" CLUSTERS (%d)", len(snap)))
	// Inner separator: same colour (236) as the rail's right-edge
	// vertical separator, sized to the sidebar's content width. Used
	// to visually group each cluster's row (+ optional bars line) so a
	// long fleet doesn't read as one wall of text.
	innerSep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("236")).
		Render(strings.Repeat("─", SidebarWidth-1))

	var lines []string
	lines = append(lines, header)
	lines = append(lines, innerSep)
	for i, st := range snap {
		if i > 0 {
			lines = append(lines, innerSep)
		}
		lines = append(lines, m.renderSidebarRow(st))
		if st.MetricsAvailable && st.AllocCPUMilli > 0 {
			lines = append(lines, m.renderSidebarBars(st))
		}
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	body := strings.Join(lines, "\n")

	// Pad the right edge with a single-cell separator so the main pane
	// has clean visual demarcation without a heavy box.
	sep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("236")).
		Render(strings.Repeat("│\n", height))
	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(SidebarWidth-1).MaxHeight(height).Render(body),
		sep,
	)
}

func sortKey(st model.ClusterState) string {
	// Healthy and degraded first, unreachable last; then alpha.
	tier := 5
	switch st.Reach {
	case model.ReachHealthy:
		tier = 0
	case model.ReachConnecting:
		tier = 1
	case model.ReachDegraded:
		tier = 2
	case model.ReachAuthFailed:
		tier = 3
	case model.ReachUnreachable:
		tier = 4
	case model.ReachUnknown:
		tier = 5
	}
	return fmt.Sprintf("%d %s", tier, st.Context)
}

func (m Model) renderSidebarRow(st model.ClusterState) string {
	dot := m.Theme.styleForReach(st.Reach).Render(st.Reach.Glyph())

	// Right-side meta: version + node count. When some nodes aren't
	// Ready, surface that as "n/N" (e.g. 2/3) instead of just "Nn".
	meta := ""
	nodeLabel := ""
	switch {
	case st.NodeCount > 0 && st.NodeReady != st.NodeCount:
		nodeLabel = fmt.Sprintf("%d/%dn", st.NodeReady, st.NodeCount)
	case st.NodeCount > 0:
		nodeLabel = fmt.Sprintf("%dn", st.NodeCount)
	}
	switch {
	case st.ServerVersion != "" && nodeLabel != "":
		meta = fmt.Sprintf("%s %s", shortVersion(st.ServerVersion), nodeLabel)
	case st.ServerVersion != "":
		meta = shortVersion(st.ServerVersion)
	case st.Reach == model.ReachAuthFailed:
		meta = "auth"
	case st.Reach == model.ReachUnreachable:
		meta = "down"
	case st.Reach == model.ReachConnecting:
		meta = "…"
	}
	// Compute the visible width before applying styling, since
	// styled strings carry ANSI escape bytes that len() would count.
	metaVisibleWidth := lipgloss.Width(meta)
	meta = m.Theme.Dim.Render(meta)

	nameWidth := SidebarWidth - metaVisibleWidth - 6
	if nameWidth < 1 {
		nameWidth = 1
	}
	displayName := st.RawName
	if displayName == "" {
		displayName = st.Context
	}
	name := truncate(displayName, nameWidth)

	prefix := " "
	if st.Context == m.WatchedContext {
		prefix = m.Theme.Title.Render("▸")
	}

	left := fmt.Sprintf("%s %s %s", prefix, dot, name)
	pad := SidebarWidth - 1 - lipgloss.Width(left) - lipgloss.Width(meta)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + meta
}

// shortVersion strips +rke2r1 etc. so we fit in the rail.
func shortVersion(v string) string {
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		return v[:i]
	}
	return v
}

// renderSidebarBars returns a one-line CPU + MEM bar pair for the
// cluster. Format with breathing room:
//
//	c 45% █████  m 61% █████
func (m Model) renderSidebarBars(st model.ClusterState) string {
	cpuPct := pct(st.UsageCPUMilli, st.AllocCPUMilli)
	memPct := pct(st.UsageMemBytes, st.AllocMemBytes)

	cpuCell := m.Theme.Dim.Render("c") + " " + barWithPct(cpuPct, m.Theme)
	memCell := m.Theme.Dim.Render("m") + " " + barWithPct(memPct, m.Theme)

	return "  " + cpuCell + "  " + memCell
}

func pct(used, alloc int64) int {
	if alloc <= 0 {
		return 0
	}
	p := int(used * 100 / alloc)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

// barWithPct renders "NN% █████" with a btop-style solid bar:
// filled cells in the load colour, empty cells in dim gray. Same
// glyph (█) in both — the difference is colour, not character.
func barWithPct(p int, th Theme) string {
	const cells = 5
	n := p * cells / 100
	if n < 0 {
		n = 0
	}
	if n > cells {
		n = cells
	}

	loadStyle := th.StatusOK
	switch {
	case p >= 80:
		loadStyle = th.StatusBad
	case p >= 60:
		loadStyle = th.StatusWrn
	}
	emptyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("236"))

	// ▬ (BLACK RECTANGLE) is vertically centred in the cell — gives
	// the btop "thin centered bar" look instead of sitting at the
	// row's bottom edge like ▄.
	filled := loadStyle.Render(strings.Repeat("▬", n))
	empty := emptyStyle.Render(strings.Repeat("▬", cells-n))

	return fmt.Sprintf("%2d%% ", p) + filled + empty
}
