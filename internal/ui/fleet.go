package ui

import (
	"fmt"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// fleetState is the F1 fleet dashboard's view state. Scroll position
// is derived from the cursor at render time (windowed like the
// tables), so the cursor is the only navigation state.
type fleetState struct {
	cursorCtx  string
	expanded   string // context with the detail panel open; "" = none
	returnView View   // restored on Esc / F1
	// savedFilter parks the resource views' filter while the fleet
	// view owns m.filterText — the two filter different things (rows
	// vs cluster names) and must not leak into each other.
	savedFilter string
	detail      fleetDetailState
}

// enterFleet/leaveFleet are the only transitions in and out of the
// fleet view; every path routes through them so the filter swap and
// returnView bookkeeping can't be missed.
func (m *Model) enterFleet() {
	m.fleet.returnView = m.view
	m.fleet.savedFilter = m.filterText
	m.filterText = ""
	m.view = ViewFleet
}

func (m *Model) leaveFleet(to View) {
	m.filterText = m.fleet.savedFilter
	m.fleet.savedFilter = ""
	// Invalidate the expansion: a result landing after the user left
	// is dropped by the receiver, and a kept loading=true would
	// otherwise revive as a forever-loading panel with both manual
	// and automatic refresh disabled.
	m.fleet.expanded = ""
	m.fleet.detail = fleetDetailState{}
	m.view = to
}

type fleetDetailState struct {
	loading bool
	result  cluster.FleetDetailResult
}

// FleetDetailMsg carries an on-demand cluster drill-down back to the
// dashboard. The receiver guards on the cluster it requested
// (m.fleet.expanded), not on WatchedContext — the dashboard shows all
// clusters.
type FleetDetailMsg cluster.FleetDetailResult

// fleetGroupsFiltered derives the triage groups from the store with
// the name filter applied — the single source both the renderer and
// the key handler read, so cursor and rows can never disagree.
func (m Model) fleetGroupsFiltered() fleetGroups {
	snap := m.Store.Snapshot()
	if f := strings.ToLower(strings.TrimSpace(m.filterText)); f != "" {
		var kept []model.ClusterState
		for _, st := range snap {
			name := st.RawName
			if name == "" {
				name = st.Context
			}
			if strings.Contains(strings.ToLower(name), f) ||
				strings.Contains(strings.ToLower(st.Context), f) {
				kept = append(kept, st)
			}
		}
		snap = kept
	}
	return groupFleet(snap)
}

func fleetOrderOf(g fleetGroups) []string {
	out := make([]string, 0, len(g.Attention)+len(g.Healthy)+len(g.Starting))
	for _, e := range g.Attention {
		out = append(out, e.St.Context)
	}
	for _, e := range g.Healthy {
		out = append(out, e.St.Context)
	}
	for _, e := range g.Starting {
		out = append(out, e.St.Context)
	}
	for _, e := range g.Offline {
		out = append(out, e.St.Context)
	}
	return out
}

func (m Model) fleetOrder() []string {
	return fleetOrderOf(m.fleetGroupsFiltered())
}

// fleetCursor resolves the stored cursor against the current order,
// snapping to the first cluster when the stored one filtered away or
// disappeared.
func (m Model) fleetCursor() string {
	order := m.fleetOrder()
	if len(order) == 0 {
		return ""
	}
	for _, c := range order {
		if c == m.fleet.cursorCtx {
			return c
		}
	}
	return order[0]
}

func (m *Model) moveFleetCursor(delta int) {
	order := m.fleetOrder()
	if len(order) == 0 {
		return
	}
	idx := 0
	for i, c := range order {
		if c == m.fleetCursor() {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(order) {
		idx = len(order) - 1
	}
	m.fleet.cursorCtx = order[idx]
}

func (m Model) handleFleetKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "j", "down":
		m.moveFleetCursor(+1)
		return m, nil
	case "k", "up":
		m.moveFleetCursor(-1)
		return m, nil
	case "g", "home":
		if order := m.fleetOrder(); len(order) > 0 {
			m.fleet.cursorCtx = order[0]
		}
		return m, nil
	case "G", "end":
		if order := m.fleetOrder(); len(order) > 0 {
			m.fleet.cursorCtx = order[len(order)-1]
		}
		return m, nil
	case "enter":
		ctx := m.fleetCursor()
		if ctx == "" {
			return m, nil
		}
		if m.fleet.expanded == ctx {
			m.fleet.expanded = ""
			return m, nil
		}
		m.fleet.expanded = ctx
		if m.fleetOffline(ctx) {
			// Nothing to fetch from a cluster we can't reach; the
			// panel renders the offline reason instead. Fetching
			// anyway risks a 10s hang at best and, if the cluster
			// half-answers, a false "all clean".
			return m, nil
		}
		return m, m.fetchFleetDetail(ctx)
	case "r":
		if m.fleet.expanded != "" && !m.fleet.detail.loading && !m.fleetOffline(m.fleet.expanded) {
			return m, m.fetchFleetDetail(m.fleet.expanded)
		}
		return m, nil
	case "o":
		ctx := m.fleetCursor()
		if ctx == "" {
			return m, nil
		}
		var cmd tea.Cmd
		if ctx != m.WatchedContext && m.OnFocusChange != nil {
			cmd = m.focusContext(ctx)
		}
		m.leaveFleet(ViewPods)
		return m, cmd
	case "/":
		m.filterFocused = true
		return m, nil
	case "esc":
		if m.fleet.expanded != "" {
			m.fleet.expanded = ""
			return m, nil
		}
		if m.filterText != "" {
			m.filterText = ""
			return m, nil
		}
		m.leaveFleet(m.fleet.returnView)
		return m, nil
	case "f1":
		m.leaveFleet(m.fleet.returnView)
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		// switchView will set the destination; restore the parked
		// resource filter and drop the expansion before it applies,
		// exactly as leaveFleet does.
		m.filterText = m.fleet.savedFilter
		m.fleet.savedFilter = ""
		m.fleet.expanded = ""
		m.fleet.detail = fleetDetailState{}
		return m.handleKey(k)
	}
	return m.handleKey(k)
}

// fleetBlock is one addressable chunk of the dashboard: a cluster's
// card or row (ctx set) or furniture like section headers (ctx "").
type fleetBlock struct {
	ctx   string
	lines []string
}

// renderFleet draws the adaptive-triage fleet dashboard: a pinned
// pulse line, then NEEDS ATTENTION cards worst-first, then compact
// HEALTHY and STARTING rows. Screen space follows attention need — on
// a clean morning every cluster is one line.
func (m Model) renderFleet(height, width int) string {
	g := m.fleetGroupsFiltered()
	pulse := clampCanvas(m.renderFleetPulse(g, width), width, 1)
	regionH := height - 1
	if regionH < 1 {
		return pulse
	}

	order := fleetOrderOf(g)
	cursor := ""
	if len(order) > 0 {
		cursor = order[0]
		for _, c := range order {
			if c == m.fleet.cursorCtx {
				cursor = c
				break
			}
		}
	}

	blocks := m.fleetBlocks(g, width)
	var lines []string
	cursorStart, cursorEnd := 0, 0
	for _, b := range blocks {
		if b.ctx != "" && b.ctx == cursor {
			cursorStart = len(lines)
			cursorEnd = len(lines) + len(b.lines) - 1
			for _, ln := range b.lines {
				if d := width - lipgloss.Width(ln); d > 0 {
					ln += strings.Repeat(" ", d)
				}
				lines = append(lines, renderSelected(ln))
			}
			continue
		}
		lines = append(lines, b.lines...)
	}

	if len(lines) == 0 {
		placeholder := " no clusters"
		if m.filterText != "" {
			placeholder = " no clusters match"
		}
		lines = []string{m.Theme.Dim.Render(placeholder)}
	}

	if len(lines) > regionH {
		// Centre the cursor block when it fits; anchor an oversized
		// block at its first line so the title and the worst-first
		// alerts stay visible rather than the card's tail.
		blockLen := cursorEnd - cursorStart + 1
		start := cursorStart
		if blockLen < regionH {
			start = cursorStart - (regionH-blockLen)/2
		}
		if start < 0 {
			start = 0
		}
		if start+regionH > len(lines) {
			start = len(lines) - regionH
		}
		lines = lines[start : start+regionH]
	}
	return pulse + "\n" + clampCanvas(strings.Join(lines, "\n"), width, regionH)
}

func (m Model) fleetBlocks(g fleetGroups, width int) []fleetBlock {
	var blocks []fleetBlock
	blank := fleetBlock{lines: []string{""}}

	if len(g.Attention) > 0 {
		blocks = append(blocks, fleetBlock{lines: []string{
			m.fleetSectionHeader("NEEDS ATTENTION", m.Theme.StatusBad, len(g.Attention), width),
		}})
		for _, e := range g.Attention {
			lines := m.renderFleetCard(e, width)
			if e.St.Context == m.fleet.expanded {
				lines = append(lines, m.renderFleetDetail(m.cardSpine(e), width)...)
			}
			blocks = append(blocks, fleetBlock{ctx: e.St.Context, lines: lines})
		}
		blocks = append(blocks, blank)
	}
	if len(g.Healthy) > 0 {
		blocks = append(blocks, fleetBlock{lines: []string{
			m.fleetSectionHeader("HEALTHY", m.Theme.StatusOK, len(g.Healthy), width),
		}})
		for _, e := range g.Healthy {
			lines := []string{m.renderFleetCompactRow(e.St, width)}
			if e.St.Context == m.fleet.expanded {
				lines = append(lines, m.renderFleetDetail(m.Theme.StatusDim.Render("▏"), width)...)
			}
			blocks = append(blocks, fleetBlock{ctx: e.St.Context, lines: lines})
		}
	}
	if len(g.Starting) > 0 {
		if len(g.Healthy) > 0 {
			blocks = append(blocks, blank)
		}
		blocks = append(blocks, fleetBlock{lines: []string{
			m.fleetSectionHeader("STARTING", m.Theme.Dim, len(g.Starting), width),
		}})
		for _, e := range g.Starting {
			lines := []string{m.renderFleetCompactRow(e.St, width)}
			if e.St.Context == m.fleet.expanded {
				lines = append(lines, m.renderFleetDetail(m.Theme.StatusDim.Render("▏"), width)...)
			}
			blocks = append(blocks, fleetBlock{ctx: e.St.Context, lines: lines})
		}
	}
	if len(g.Offline) > 0 {
		if len(g.Healthy) > 0 || len(g.Starting) > 0 {
			blocks = append(blocks, blank)
		}
		blocks = append(blocks, fleetBlock{lines: []string{
			m.fleetSectionHeader("OFFLINE", m.Theme.Dim, len(g.Offline), width),
		}})
		for _, e := range g.Offline {
			lines := []string{m.renderFleetCompactRow(e.St, width)}
			if e.St.Context == m.fleet.expanded {
				lines = append(lines, m.renderFleetOfflineDetail(e.St, width)...)
			}
			blocks = append(blocks, fleetBlock{ctx: e.St.Context, lines: lines})
		}
	}
	return blocks
}

func (m Model) fleetSectionHeader(label string, style lipgloss.Style, count int, width int) string {
	rule := lipgloss.NewStyle().Foreground(lipgloss.Color("236"))
	head := rule.Render("── ") + style.Render(label) +
		m.Theme.Dim.Render(fmt.Sprintf(" (%d) ", count))
	fill := width - lipgloss.Width(head)
	if fill < 0 {
		fill = 0
	}
	return head + rule.Render(strings.Repeat("─", fill))
}

// renderFleetPulse is the pinned one-liner that answers "is anything
// wrong?" before the eye moves anywhere else.
func (m Model) renderFleetPulse(g fleetGroups, width int) string {
	th := m.Theme
	p := derivePulse(g)
	sep := th.Dim.Render(" · ")

	left := " " + th.Title.Render("FLEET") + fmt.Sprintf(" %d clusters", p.Clusters)
	if p.NeedAction > 0 {
		left += sep + th.StatusBad.Render(fmt.Sprintf("%d need action", p.NeedAction))
	} else {
		left += sep + th.StatusOK.Render("all clear ✓")
	}

	if p.Offline > 0 {
		left += sep + th.Dim.Render(fmt.Sprintf("%d offline", p.Offline))
	}

	var parts []string
	if p.Nodes > 0 {
		nodes := fmt.Sprintf("%d nodes", p.Nodes)
		if p.NodesBad > 0 {
			nodes += " (" + th.StatusBad.Render(fmt.Sprintf("%d✗", p.NodesBad)) + ")"
		}
		parts = append(parts, nodes)
	}
	if p.Pods > 0 || p.PodsBad > 0 || !p.AllPodsKnown {
		pods := fmt.Sprintf("%d", p.Pods)
		if !p.AllPodsKnown {
			pods += "+"
		}
		pods += " pods"
		if p.PodsBad > 0 {
			pods += " (" + th.StatusWrn.Render(fmt.Sprintf("%d✗", p.PodsBad)) + ")"
		}
		parts = append(parts, pods)
	}
	if p.HasMetrics {
		parts = append(parts,
			th.Dim.Render("cpu ")+th.loadStyle(p.CPUPct).Render(fmt.Sprintf("%d%%", p.CPUPct)),
			th.Dim.Render("mem ")+th.loadStyle(p.MemPct).Render(fmt.Sprintf("%d%%", p.MemPct)))
	}
	right := strings.Join(parts, sep)

	pad := width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if pad < 1 {
		// Narrow: the verdict outranks the totals.
		return left
	}
	return left + strings.Repeat(" ", pad) + right
}

func (m Model) cardSpine(e fleetEntry) string {
	if worstSeverity(e.Alerts) == sevCrit {
		return m.Theme.StatusBad.Render("▍")
	}
	return m.Theme.StatusWrn.Render("▍")
}

// renderFleetCard is one NEEDS ATTENTION cluster: the same row every
// section uses (plus severity spine and alert-count badges), then one
// line per alert. Same fact, same column, whichever section a cluster
// is in.
func (m Model) renderFleetCard(e fleetEntry, width int) []string {
	th := m.Theme
	spine := m.cardSpine(e)

	crit, warn := alertCounts(e.Alerts)
	badges := ""
	if crit > 0 {
		badges += " " + th.StatusBad.Render(fmt.Sprintf("✗%d", crit))
	}
	if warn > 0 {
		badges += " " + th.StatusWrn.Render(fmt.Sprintf("⚠%d", warn))
	}
	lines := []string{m.fleetRow(e.St, spine, badges, width)}

	for _, a := range e.Alerts {
		glyph, style := "·", th.Dim
		switch a.Sev {
		case sevCrit:
			glyph, style = "✗", th.StatusBad
		case sevWarn:
			glyph, style = "⚠", th.StatusWrn
		}
		lines = append(lines, spine+"   "+style.Render(glyph)+" "+truncate(a.Text, width-7))
	}
	return lines
}

// renderFleetCompactRow is one healthy or starting cluster in a
// single line; offline clusters swap the stat cells for the reason.
func (m Model) renderFleetCompactRow(st model.ClusterState, width int) string {
	th := m.Theme

	// Offline rows: name plus the reason, nothing else pretends to be
	// known.
	if st.Reach == model.ReachUnreachable || st.Reach == model.ReachAuthFailed {
		name := st.RawName
		if name == "" {
			name = st.Context
		}
		nameW := width / 3
		if nameW > 24 {
			nameW = 24
		}
		if nameW < 8 {
			nameW = 8
		}
		spine := th.StatusDim.Render("▏")
		glyph := th.styleForReach(st.Reach).Render(st.Reach.Glyph())
		msg := truncate(withErr(st.Reach.String(), st.LastError), width-nameW-7)
		return spine + " " + glyph + " " + padCol(truncate(name, nameW), nameW, th.Base) +
			"  " + th.Dim.Render(msg)
	}

	return m.fleetRow(st, th.StatusDim.Render("▏"), "", width)
}

// fleetRow is the shared one-line cluster layout: name, version,
// probe latency, nodes, cpu/mem %, trend, pods. Cells drop from the
// right as the terminal narrows.
func (m Model) fleetRow(st model.ClusterState, spine, badges string, width int) string {
	th := m.Theme

	name := st.RawName
	if name == "" {
		name = st.Context
	}
	badgesW := lipgloss.Width(badges)

	type cell struct {
		s string
		w int
	}
	var cells []cell
	add := func(s string, w int) { cells = append(cells, cell{padCellANSI(s, w), w}) }
	addRight := func(s string, w int) { cells = append(cells, cell{padCellANSIRight(s, w), w}) }

	if v := shortVersion(st.ServerVersion); v != "" {
		add(th.Dim.Render(v), 8)
	} else if st.Reach == model.ReachConnecting || st.Reach == model.ReachUnknown {
		add(th.Dim.Render(st.Reach.String()+"…"), 12)
	}
	if st.ProbeLatency > 0 && width >= 75 {
		addRight(th.Dim.Render(fmt.Sprintf("%dms", st.ProbeLatency.Milliseconds())), 6)
	}
	switch {
	case st.NodeCount > 0 && width >= 110:
		add(nodeDots(st, th)+" "+th.Dim.Render(fmt.Sprintf("%dn", st.NodeCount)), 17)
	case st.NodeCount > 0:
		lbl := fmt.Sprintf("%dn", st.NodeCount)
		style := th.Dim
		if st.NodesCordonedReady > 0 {
			style = th.StatusWrn
		}
		if st.NodeReady != st.NodeCount {
			lbl = fmt.Sprintf("%d/%dn", st.NodeReady, st.NodeCount)
			style = th.StatusWrn
		}
		addRight(style.Render(lbl), 6)
	}
	if width >= 70 && st.MetricsAvailable && st.AllocCPUMilli > 0 {
		cp := pct(st.UsageCPUMilli, st.AllocCPUMilli)
		mp := pct(st.UsageMemBytes, st.AllocMemBytes)
		add(th.Dim.Render("c ")+th.loadStyle(cp).Render(fmt.Sprintf("%2d%%", cp))+
			th.Dim.Render("  m ")+th.loadStyle(mp).Render(fmt.Sprintf("%2d%%", mp)), 14)
	}
	if width >= 110 {
		add(th.Dim.Render(sparkline(m.trendVals(st.Context), 5)), 5)
	}
	if width >= 85 && st.PodsTotal >= 0 {
		addRight(th.Dim.Render(fmt.Sprintf("%dp", st.PodsTotal)), 6)
	}

	rightW := 0
	for _, c := range cells {
		rightW += c.w + 2
	}
	nameW := width - 5 - badgesW - rightW
	for nameW < 8 && len(cells) > 0 {
		last := cells[len(cells)-1]
		cells = cells[:len(cells)-1]
		nameW += last.w + 2
	}
	if nameW < 1 {
		nameW = 1
	}

	glyph := th.styleForReach(st.Reach).Render(st.Reach.Glyph())
	row := spine + " " + glyph + " " +
		padCellANSI(truncate(name, nameW)+badges, nameW+badgesW)
	for _, c := range cells {
		row += "  " + c.s
	}
	return row
}

func (m Model) trendVals(ctx string) []int {
	if r := m.fleetTrends[ctx]; r != nil {
		return r.vals
	}
	return nil
}

// sampleFleetTrends records one mem% sample per cluster whenever its
// metrics timestamp advances. Driven by the 1 Hz ProbeTickMsg; the
// ring dedupes on MetricsAt so the effective cadence is the metrics
// poller's, not the tick's.
func (m Model) sampleFleetTrends() {
	for _, st := range m.Store.Snapshot() {
		if !st.MetricsAvailable || st.AllocMemBytes <= 0 {
			continue
		}
		r := m.fleetTrends[st.Context]
		if r == nil {
			r = &trendRing{}
			m.fleetTrends[st.Context] = r
		}
		r.push(pct(st.UsageMemBytes, st.AllocMemBytes), st.MetricsAt)
	}
}

// fetchFleetDetail dispatches the on-demand drill-down for ctx. The
// previous result stays visible through a same-cluster refresh, but a
// different cluster's rows must never show under this card.
func (m *Model) fetchFleetDetail(ctx string) tea.Cmd {
	if m.OnFleetDetail == nil {
		return nil
	}
	prev := m.fleet.detail.result
	if prev.Context != ctx {
		prev = cluster.FleetDetailResult{}
	}
	m.fleet.detail = fleetDetailState{loading: true, result: prev}
	cb := m.OnFleetDetail
	return func() tea.Msg { return cb(ctx) }
}

// renderFleetDetail renders the fetched drill-down under an expanded
// cluster. Everything here is cluster-produced text: it goes through
// cleanDetail + the plain truncate pipeline, never raw ANSI.
func (m Model) renderFleetDetail(spine string, width int) []string {
	th := m.Theme
	d := m.fleet.detail
	pad := spine + "     "
	if d.result.At.IsZero() {
		return []string{pad + th.Dim.Render("… fetching details")}
	}
	r := d.result
	var lines []string
	if r.Err != "" {
		lines = append(lines, pad+th.StatusBad.Render("✗ ")+
			truncate("detail fetch: "+cleanDetail(r.Err), width-10))
	}
	half := width / 2
	if half < 20 {
		half = 20
	}
	for _, p := range r.Pods {
		s := pad + th.Dim.Render("pod ") + truncate(p.Namespace+"/"+p.Name, half) +
			" " + th.styleForPhase(corev1.PodPhase(p.Phase)).Render(p.Phase)
		if p.Reason != "" {
			s += " " + th.StatusWrn.Render(truncate(cleanDetail(p.Reason), 24))
		}
		if p.Restarts > 0 {
			s += " " + restartStyle(p.Restarts, th).Render(fmt.Sprintf("↻%d", p.Restarts))
		}
		lines = append(lines, s)
	}
	for _, dep := range r.Deploys {
		lines = append(lines, pad+th.Dim.Render("dep ")+
			truncate(dep.Namespace+"/"+dep.Name, half)+" "+
			readyStyle(int(dep.Ready), int(dep.Desired), th).Render(
				fmt.Sprintf("%d/%d", dep.Ready, dep.Desired)))
	}
	for _, ev := range r.Events {
		head := pad + th.Dim.Render("evt ") +
			th.StatusWrn.Render(truncate(cleanDetail(ev.Reason), 24)) +
			th.Dim.Render(fmt.Sprintf(" ×%d %s ", ev.Count, formatAge(ev.LastSeen)))
		if avail := width - lipgloss.Width(head) - 1; avail > 4 {
			head += truncate(cleanDetail(ev.Message), avail)
		}
		lines = append(lines, head)
	}
	if r.Err == "" && len(r.Pods)+len(r.Deploys)+len(r.Events) == 0 {
		lines = append(lines, pad+th.Dim.Render("no problem detail — pods, deployments and events look clean"))
	}
	status := fmt.Sprintf("fetched %s ago · r to refresh · Enter to collapse", formatAge(r.At))
	if d.loading {
		status = "… refreshing"
	}
	lines = append(lines, pad+th.Dim.Render(status))
	return lines
}

// cleanDetail strips terminal control characters (ESC/CSI/OSC, BEL,
// the whole C0/C1 range) out of cluster-sourced strings and flattens
// whitespace, so an event message can neither inject layout rows nor
// smuggle escape sequences to the terminal. strings.Fields alone only
// handles whitespace — ESC is not whitespace.
func cleanDetail(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// fleetOffline reports whether ctx is currently classified offline —
// the states whose detail cannot be fetched, only explained.
func (m Model) fleetOffline(ctx string) bool {
	st, ok := m.Store.Get(ctx)
	return ok && (st.Reach == model.ReachUnreachable || st.Reach == model.ReachAuthFailed)
}

// renderFleetOfflineDetail is the expanded panel for an offline
// cluster: the full reason and the probe age — everything kubetin
// actually knows, and no claim about anything it cannot.
func (m Model) renderFleetOfflineDetail(st model.ClusterState, width int) []string {
	th := m.Theme
	pad := th.StatusDim.Render("▏") + "     "
	label := "unreachable"
	if st.Reach == model.ReachAuthFailed {
		label = "auth failed — a re-login may fix this"
	}
	lines := []string{pad + th.StatusBad.Render("✕ ") + th.Dim.Render(label+" — details can't be fetched")}
	if st.LastError != "" {
		lines = append(lines, pad+truncate(cleanDetail(st.LastError), width-8))
	}
	status := "never probed successfully"
	if !st.LastProbe.IsZero() {
		status = fmt.Sprintf("last probe attempt %s ago", formatAge(st.LastProbe))
	}
	lines = append(lines, pad+th.Dim.Render(status+" · Enter to collapse"))
	return lines
}
