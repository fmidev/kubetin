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

// focusOrder returns the switchable contexts, with the state each was
// ordered by, in the order the rail draws them. Tab used to walk
// m.Contexts — kubeconfig discovery order — while the rail (and the
// overview, and the debug view) all sort by sortKey, so every press
// jumped to a seemingly random row and the order changed again whenever
// a probe moved a cluster between tiers.
//
// The state travels with the context so callers judge reachability from
// the same read that produced the order: one Snapshot, as renderSidebar
// takes, rather than per-cluster Store.Get calls that each land at a
// different instant and can assemble an order from states that never
// coexisted.
func (m Model) focusOrder() []model.ClusterState {
	states := make(map[string]model.ClusterState)
	for _, st := range m.Store.Snapshot() {
		states[st.Context] = st
	}
	type entry struct {
		st  model.ClusterState
		key string
	}
	entries := make([]entry, 0, len(m.Contexts))
	for _, c := range m.Contexts {
		st, ok := states[c]
		if !ok {
			// Not probed yet: same tier the rail gives ReachUnknown.
			st = model.ClusterState{Context: c}
		}
		entries = append(entries, entry{st: st, key: sortKey(st)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	out := make([]model.ClusterState, len(entries))
	for i, e := range entries {
		out[i] = e.st
	}
	return out
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

	// Alert badge: the fleet dashboard's derivation, compressed to
	// "✗2"/"⚠1" so degradation elsewhere is visible while working in
	// another cluster.
	if crit, warn := alertCounts(clusterAlerts(st)); crit > 0 || warn > 0 {
		badge := fmt.Sprintf("⚠%d", warn)
		style := m.Theme.StatusWrn
		if crit > 0 {
			badge = fmt.Sprintf("✗%d", crit)
			style = m.Theme.StatusBad
		}
		metaVisibleWidth += lipgloss.Width(badge) + 1
		meta = style.Render(badge) + " " + meta
	}

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

// barWithPct renders "NN% █████".
func barWithPct(p int, th Theme) string {
	return fmt.Sprintf("%2d%% ", p) + bar(p, 5, th)
}

// bar renders a `cells`-wide btop-style solid bar: filled cells in
// the load colour, empty cells in dim gray. Same glyph (▬) in both —
// the difference is colour, not character. Fill is clamped; the
// caller's number stays honest for >100%.
func bar(p, cells int, th Theme) string {
	n := p * cells / 100
	if n < 0 {
		n = 0
	}
	if n > cells {
		n = cells
	}

	emptyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("236"))

	// ▬ (BLACK RECTANGLE) is vertically centred in the cell — gives
	// the btop "thin centered bar" look instead of sitting at the
	// row's bottom edge like ▄.
	filled := th.loadStyle(p).Render(strings.Repeat("▬", n))
	empty := emptyStyle.Render(strings.Repeat("▬", cells-n))
	return filled + empty
}
