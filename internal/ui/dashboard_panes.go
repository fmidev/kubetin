package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// dashSep is the run between labelled fields in the status banner.
const dashSep = "   "

// dashPodStatusHeight is how many content rows the pod banner needs.
// A pod with a failing condition spends a third line explaining it —
// that text is the whole reason the dashboard exists for a stuck pod,
// so it gets its own row rather than being truncated onto the second.
func dashPodStatusHeight(r podRow) int {
	if _, ok := podBlockingCondition(r); ok {
		return 3
	}
	return 2
}

// podBlockingCondition returns the first condition that isn't True,
// which is what stops a pod becoming Ready. Conditions are reported in
// dependency order by kubelet (PodScheduled → Initialized →
// ContainersReady → Ready), so the first failing one is the root cause
// and the ones after it are consequences.
func podBlockingCondition(r podRow) (cluster.PodCondition, bool) {
	for _, c := range r.Conditions {
		if c.Status != "True" && c.Type != "DisruptionTarget" {
			return c, true
		}
	}
	return cluster.PodCondition{}, false
}

// renderDashPodStatus draws the banner: identity and health on line
// one, resource usage on line two, and the blocking condition on line
// three when there is one.
func (m Model) renderDashPodStatus(r podRow, w, h int) string {
	th := m.Theme

	ready, total := containerReadyCount(r)
	phaseStyle := th.styleForPhase(r.Phase)

	line1 := []string{
		phaseStyle.Render("●") + " " + phaseStyle.Render(string(r.Phase)),
		dashField("ready", fmt.Sprintf("%d/%d", ready, total), readyStyle(ready, total, th), th),
		dashField("↻", fmt.Sprintf("%d", r.Restarts), restartStyle(r.Restarts, th), th),
		dashField("age", formatAge(r.CreatedAt), th.Base, th),
	}
	if r.Node != "" {
		line1 = append(line1, dashField("node", shortHost(r.Node), th.Base, th))
	}
	if r.PodIP != "" {
		line1 = append(line1, dashField("ip", r.PodIP, th.Base, th))
	}
	if r.QOSClass != "" {
		line1 = append(line1, dashField("qos", r.QOSClass, th.Base, th))
	}

	memField := dashField("mem", formatMem(r.MemBytes), th.Base, th)
	if p, ok := podMemPct(r); ok {
		memField = dashField("mem", formatMem(r.MemBytes)+" / "+formatMem(r.MemLimitBytes), th.Base, th) +
			" " + bar(p, 10, th) + " " + th.loadStyle(p).Render(fmt.Sprintf("%d%%", p))
	}
	line2 := []string{
		dashField("cpu", formatCPU(r.CPUMilli), th.Base, th),
		memField,
	}
	if r.HasNetwork {
		line2 = append(line2,
			dashField("↓", formatRate(r.NetRXBps), th.Base, th),
			dashField("↑", formatRate(r.NetTXBps), th.Base, th))
	}
	if r.ServiceAccount != "" {
		line2 = append(line2, dashField("sa", r.ServiceAccount, th.Base, th))
	}
	if r.Namespace != "" {
		line2 = append(line2, dashField("ns", r.Namespace, th.Base, th))
	}

	rows := []string{
		padCellANSI(joinFields(line1, w), w),
		padCellANSI(joinFields(line2, w), w),
	}
	if c, ok := podBlockingCondition(r); ok {
		rows = append(rows, padCellANSI(renderCondition(c, w, th), w))
	}
	return clampCanvas(strings.Join(rows, "\n"), w, h)
}

// renderCondition formats a non-True condition as "⚠ Reason · Message",
// falling back to the type when kubelet gave no reason.
func renderCondition(c cluster.PodCondition, w int, th Theme) string {
	label := c.Reason
	if label == "" {
		label = c.Type + "=" + c.Status
	}
	text := label
	if c.Message != "" {
		text += " · " + oneLine(c.Message)
	}
	return th.StatusWrn.Render("⚠ ") + th.StatusWrn.Render(truncate(text, w-2))
}

// dashField renders "label value" with the label dimmed. Value keeps
// its own style so health colours survive.
func dashField(label, value string, valueStyle lipgloss.Style, th Theme) string {
	return th.Dim.Render(label) + " " + valueStyle.Render(value)
}

// joinFields concatenates as many fields as fit in w cells, dropping
// the overflow rather than truncating mid-field — a half-rendered
// "ip 10.42." is worse than no ip at all.
func joinFields(fields []string, w int) string {
	var b strings.Builder
	for _, f := range fields {
		add := f
		if b.Len() > 0 {
			add = dashSep + f
		}
		if lipgloss.Width(b.String())+lipgloss.Width(add) > w {
			break
		}
		b.WriteString(add)
	}
	return b.String()
}

func containerReadyCount(r podRow) (ready, total int) {
	for _, ci := range r.ContainerInfo {
		total++
		if ci.Ready {
			ready++
		}
	}
	if total == 0 {
		total = len(r.Containers)
	}
	return ready, total
}

func readyStyle(ready, total int, th Theme) lipgloss.Style {
	switch {
	case total == 0:
		return th.Dim
	case ready == total:
		return th.StatusOK
	case ready == 0:
		return th.StatusBad
	}
	return th.StatusWrn
}

func restartStyle(n int32, th Theme) lipgloss.Style {
	if n > 0 {
		return th.StatusWrn
	}
	return th.Base
}

// dashContainerColumns: NAME never drops, IMAGE absorbs spare width
// but is the first to go when the pane narrows — the state and restart
// count are what you scan for.
var dashContainerColumns = []column{
	{min: 12, max: 22, prio: 0}, // NAME
	{min: 10, max: 16, prio: 1}, // STATE
	{min: 3, max: 4, prio: 2},   // RESTARTS
	{min: 9, max: 13, prio: 3},  // MEM
	{min: 14, max: 46, prio: 4}, // IMAGE
}

// renderDashContainers lists init containers (prefixed) then regular
// ones, with a continuation row spelling out the reason for any
// container that is failing.
func (m Model) renderDashContainers(r podRow, w, h, scroll int) string {
	th := m.Theme
	cw := fitColumns(dashContainerColumns, w-1)

	var lines []string
	emit := func(ci cluster.ContainerInfo, init bool) {
		name := ci.Name
		if init {
			name = "init:" + name
		}
		state, style := containerStateLabel(ci, th)
		lines = append(lines, " "+joinCells(
			padCol(name, cw[0], th.Base),
			padCol(state, cw[1], style),
			padColRight(fmt.Sprintf("%d", ci.Restarts), cw[2], restartStyle(ci.Restarts, th)),
			padCellANSIRight(containerMemCell(ci, r, th), cw[3]),
			padCol(shortImage(ci.Image), cw[4], th.Dim),
		))
		if detail := containerDetail(ci); detail != "" {
			lines = append(lines, " "+th.Dim.Render("  └ ")+
				th.StatusBad.Render(truncate(detail, w-5)))
		}
	}

	for _, ci := range r.InitContainerInfo {
		emit(ci, true)
	}
	for _, ci := range r.ContainerInfo {
		emit(ci, false)
	}

	// Before kubelet reports any status the pod still has spec
	// container names — show those rather than an empty pane, so a
	// freshly-scheduled pod doesn't look broken.
	if len(lines) == 0 {
		for _, name := range r.Containers {
			lines = append(lines, " "+joinCells(
				padCol(name, cw[0], th.Base),
				padCol("pending", cw[1], th.Dim),
			))
		}
	}
	if len(lines) == 0 {
		return dashPaneBody([]string{" " + th.Dim.Render("no containers reported")}, w, h, 0)
	}

	return dashPaneBody(lines, w, h, scroll)
}

// containerMemCell renders "usage/limit" with the usage coloured by
// its share of the limit. Mixed styles — ANSI-aware padding only.
func containerMemCell(ci cluster.ContainerInfo, r podRow, th Theme) string {
	usage, hasUsage := r.ContainerMemBytes[ci.Name]
	hasUsage = hasUsage && r.HasMetrics
	switch {
	case !hasUsage && ci.MemLimitBytes <= 0:
		return th.Dim.Render("—")
	case !hasUsage:
		return th.Dim.Render("—/" + formatMem(ci.MemLimitBytes))
	case ci.MemLimitBytes <= 0:
		return th.Base.Render(formatMem(usage)) + th.Dim.Render("/—")
	}
	p := int(usage * 100 / ci.MemLimitBytes)
	return th.loadStyle(p).Render(formatMem(usage)) +
		th.Dim.Render("/"+formatMem(ci.MemLimitBytes))
}

// containerStateLabel maps the coarse state onto a word plus its
// colour, preferring kubelet's reason when there is one — "Running"
// tells you less than "CrashLoopBackOff".
func containerStateLabel(ci cluster.ContainerInfo, th Theme) (string, lipgloss.Style) {
	switch ci.State {
	case cluster.ContainerReady:
		return "Running", th.StatusOK
	case cluster.ContainerError:
		if ci.Reason != "" {
			return ci.Reason, th.StatusBad
		}
		return "Error", th.StatusBad
	case cluster.ContainerTerminated:
		if ci.Reason != "" {
			return ci.Reason, th.StatusDim
		}
		return "Terminated", th.StatusDim
	}
	if ci.Reason != "" {
		return ci.Reason, th.StatusWrn
	}
	return "Waiting", th.StatusWrn
}

// containerDetail is the continuation line for a failing container:
// the exit code is the part that distinguishes an OOM kill from a
// config error, and it has nowhere to live in the row itself.
func containerDetail(ci cluster.ContainerInfo) string {
	if ci.State != cluster.ContainerError {
		return ""
	}
	if ci.ExitCode != 0 {
		return fmt.Sprintf("exit %d", ci.ExitCode)
	}
	return ""
}

// shortImage drops the registry host so the tag stays visible when the
// column is tight — "ghcr.io/fmidev/api:1.2" → "fmidev/api:1.2".
func shortImage(img string) string {
	if i := strings.IndexByte(img, '/'); i >= 0 && strings.ContainsAny(img[:i], ".:") {
		return img[i+1:]
	}
	return img
}

// dashEventColumns: MESSAGE soaks up the width, AGE and TYPE shed
// first — the reason plus message is the payload. OBJECT sits between
// them and is only present when the pane aggregates several objects.
var dashEventColumns = []column{
	{min: 4, max: 5, prio: 2},    // AGE
	{min: 4, max: 7, prio: 3},    // TYPE
	{min: 10, max: 20, prio: 1},  // REASON
	{min: 14, max: 100, prio: 0}, // MESSAGE
}

var dashEventObjectColumns = []column{
	{min: 4, max: 5, prio: 3},    // AGE
	{min: 4, max: 7, prio: 4},    // TYPE
	{min: 12, max: 28, prio: 2},  // OBJECT
	{min: 10, max: 20, prio: 1},  // REASON
	{min: 14, max: 100, prio: 0}, // MESSAGE
}

// renderDashEvents lists the scoped events newest first. withObject
// adds the involved object's name, which the pod pane doesn't need
// (every row is the same pod) but the deployment pane does — knowing
// *which* replica is backing off is most of the signal.
func (m Model) renderDashEvents(events []eventRow, w, h, scroll int, withObject bool) string {
	th := m.Theme
	if len(events) == 0 {
		return dashPaneBody([]string{" " + th.Dim.Render("no events")}, w, h, 0)
	}
	cols := dashEventColumns
	if withObject {
		cols = dashEventObjectColumns
	}
	cw := fitColumns(cols, w-1)

	// Format only the rows that will be shown. Every cell costs a
	// lipgloss Style.Render, which dominates this pane's cost, and the
	// event cache is cluster-wide: formatting 2000 rows to display 20
	// was ~100x the work needed. h<=0 means "natural height", where
	// the caller does want them all.
	visible := events
	if h > 0 {
		start := clampEventScroll(len(events), scroll, h)
		visible = events[start : start+min(h, len(events)-start)]
	}

	lines := make([]string, 0, len(visible))
	for _, e := range visible {
		typeStyle, msgStyle := th.Dim, th.Base
		if e.Type == "Warning" {
			typeStyle, msgStyle = th.StatusWrn, th.StatusWrn
		}
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" ×%d", e.Count)
		}
		cells := []string{
			padCol(formatAge(e.LastSeen), cw[0], th.Dim),
			padCol(shortEventType(e.Type), cw[1], typeStyle),
		}
		if withObject {
			cells = append(cells, padCol(e.InvolvedName, cw[2], th.Dim))
		}
		cells = append(cells,
			padCol(e.Reason+count, cw[len(cw)-2], msgStyle),
			padCol(oneLine(e.Message), cw[len(cw)-1], th.Base),
		)
		lines = append(lines, " "+joinCells(cells...))
	}

	// Already windowed above, so the offset here is zero.
	return dashPaneBody(lines, w, h, 0)
}

// clampEventScroll bounds a scroll offset to the rows available,
// picking the same first row scrollWindow would have selected from the
// fully-formatted set. clampCanvas pads the short tail, so nothing
// needs adding above.
func clampEventScroll(total, scroll, h int) int {
	maxStart := total - h
	if maxStart < 0 {
		maxStart = 0
	}
	if scroll > maxStart {
		scroll = maxStart
	}
	if scroll < 0 {
		scroll = 0
	}
	return scroll
}

// dashEventLineCount is the natural height of the events pane without
// formatting a single row — one line per event, or one for the empty
// placeholder. Lets the layout resolve heights without paying for a
// render it is only going to measure.
func dashEventLineCount(events []eventRow) int {
	if len(events) == 0 {
		return 1
	}
	return len(events)
}

// dashPaneBody windows pane rows to h and pads to the pane rect. A
// non-positive h means "natural height": the stacked layout sizes each
// box to its content, so it needs the rows before it knows how tall the
// box should be.
func dashPaneBody(lines []string, w, h, scroll int) string {
	joined := strings.Join(lines, "\n")
	if h <= 0 {
		return joined
	}
	body, _ := scrollWindow(joined, scroll, h)
	return clampCanvas(body, w, h)
}

// lineCount counts rendered rows, ignoring a trailing newline.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

func shortEventType(t string) string {
	if t == "Warning" {
		return "Warn"
	}
	return t
}

// oneLine flattens embedded newlines. Event messages routinely carry
// them (the scheduler's "0/5 nodes are available:" text especially),
// and a stray \n inside a pane pushes every row below it down and
// breaks the canvas height contract.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\n\r") {
		return s
	}
	r := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// renderDashLogs draws the log tail. It reads the same logsState ring
// buffer *and the same scroll offset* the full-screen viewer uses —
// the dashboard pane is a second renderer over one stream, not a
// second stream with its own position.
func (m Model) renderDashLogs(w, h int) string {
	scroll := m.logs.scroll
	th := m.Theme
	// A non-positive height reaches the slice arithmetic below as a
	// negative capacity and panics the whole TUI (makeslice: cap out
	// of range). The stacked layout's floor keeps that from happening
	// today, but a pane renderer shouldn't depend on its caller for
	// that — clampCanvas already treats h<1 as 1, so match it.
	if h < 1 {
		h = 1
	}
	if m.logs.err != "" && len(m.logs.lines) == 0 {
		return clampCanvas(" "+th.StatusBad.Render("✕ "+summariseStreamErr(m.logs.err)), w, h)
	}
	if len(m.logs.lines) == 0 {
		return clampCanvas(" "+th.Dim.Render("waiting for log lines…"), w, h)
	}

	lines := m.logs.lines
	end := len(lines) - scroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	start := end - h
	if start < 0 {
		start = 0
	}

	out := make([]string, 0, h)
	for _, ln := range lines[start:end] {
		out = append(out, " "+fitLogLine(oneLine(ln), w-1))
	}
	return clampCanvas(strings.Join(out, "\n"), w, h)
}

// dashEventsFor returns the events involving ref, newest first. For a
// Pod that's an exact involvedObject match; the Deployment case adds
// its owned pods and ReplicaSets on top (see dashDeployEvents).
func (m Model) dashEventsFor(ref cluster.DescribeRef) []eventRow {
	scope := eventScopeRef{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
	out := make([]eventRow, 0, 16)
	for _, e := range m.events {
		if scope.matches(e) {
			out = append(out, e)
		}
	}
	sortEventsNewestFirst(out)
	return out
}

// sortEventsNewestFirst orders by LastSeen descending with a stable
// tiebreaker chain, so rows don't shuffle when two events share a
// timestamp and the informer re-fires.
func sortEventsNewestFirst(rows []eventRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.After(b.LastSeen)
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.UID < b.UID
	})
}

// dashDeployStatusHeight mirrors dashPodStatusHeight: a third line
// appears only when a condition explains why the rollout isn't
// converging.
func dashDeployStatusHeight(d deploymentRow) int {
	if _, ok := deployBlockingCondition(d); ok {
		return 3
	}
	return 2
}

// deployBlockingCondition returns the condition that explains a stuck
// deployment. Progressing=True is the healthy steady state *and* the
// mid-rollout state, so only False counts; Available=False means the
// deployment is below its minimum ready replicas right now.
func deployBlockingCondition(d deploymentRow) (cluster.DeployCondition, bool) {
	for _, c := range d.Conditions {
		if c.Type == "Available" && c.Status != "True" {
			return c, true
		}
		if c.Type == "Progressing" && c.Status == "False" {
			return c, true
		}
	}
	return cluster.DeployCondition{}, false
}

// renderDashDeployStatus draws the replica counts on line one and the
// rollout strategy on line two.
func (m Model) renderDashDeployStatus(d deploymentRow, w, h int) string {
	th := m.Theme

	rs := readyStyle(int(d.Ready), int(d.Replicas), th)
	line1 := []string{
		rs.Render("●") + " " + rs.Render(fmt.Sprintf("%d/%d ready", d.Ready, d.Replicas)),
		dashField("up-to-date", fmt.Sprintf("%d", d.UpToDate), th.Base, th),
		dashField("available", fmt.Sprintf("%d", d.Available), th.Base, th),
	}
	if d.Unavailable > 0 {
		line1 = append(line1, dashField("unavailable",
			fmt.Sprintf("%d", d.Unavailable), th.StatusWrn, th))
	}
	line1 = append(line1,
		dashField("age", formatAge(d.CreatedAt), th.Base, th),
		dashField("ns", d.Namespace, th.Base, th))

	line2 := []string{}
	if d.StrategyType != "" {
		line2 = append(line2, dashField("strategy", d.StrategyType, th.Base, th))
	}
	if d.MaxSurge != "" {
		line2 = append(line2, dashField("surge", d.MaxSurge, th.Base, th))
	}
	if d.MaxUnavailable != "" {
		line2 = append(line2, dashField("maxUnavail", d.MaxUnavailable, th.Base, th))
	}
	if sel := formatSelector(d.Selector); sel != "" {
		line2 = append(line2, dashField("selector", sel, th.Base, th))
	}

	rows := []string{
		padCellANSI(joinFields(line1, w), w),
		padCellANSI(joinFields(line2, w), w),
	}
	if c, ok := deployBlockingCondition(d); ok {
		rows = append(rows, padCellANSI(renderCondition(cluster.PodCondition{
			Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message,
		}, w, th), w))
	}
	return clampCanvas(strings.Join(rows, "\n"), w, h)
}

// formatSelector renders match labels in the k=v,k=v form kubectl
// prints, sorted so the string is stable across renders.
func formatSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}

// dashPodColumns for the deployment's owned-pod list. POD never drops;
// NODE goes first, then AGE — restarts and status are what you scan a
// replica list for.
var dashPodColumns = []column{
	{min: 18, max: 40, prio: 0}, // POD
	{min: 10, max: 16, prio: 1}, // STATUS
	{min: 5, max: 5, prio: 2},   // READY
	{min: 3, max: 4, prio: 3},   // RESTARTS
	{min: 4, max: 5, prio: 4},   // AGE
	{min: 10, max: 20, prio: 5}, // NODE
}

// renderDashPods lists the deployment's pods with a cursor, so the
// user can drill into one. The window follows the cursor rather than
// starting at row 0 — otherwise replicas past the pane height are
// unreachable, the same bug the main tables had.
func (m Model) renderDashPods(pods []podRow, w, h, cursor int) string {
	th := m.Theme
	if len(pods) == 0 {
		return dashPaneBody([]string{" " + th.Dim.Render("no pods match this deployment")}, w, h, 0)
	}
	cw := fitColumns(dashPodColumns, w-1)

	lines := make([]string, 0, len(pods))
	for i, p := range pods {
		ready, total := containerReadyCount(p)
		line := " " + joinCells(
			padCol(p.Name, cw[0], th.Base),
			padCol(string(p.Phase), cw[1], th.styleForPhase(p.Phase)),
			padColRight(fmt.Sprintf("%d/%d", ready, total), cw[2], readyStyle(ready, total, th)),
			padColRight(fmt.Sprintf("%d", p.Restarts), cw[3], restartStyle(p.Restarts, th)),
			padColRight(formatAge(p.CreatedAt), cw[4], th.Base),
			padCol(shortHost(p.Node), cw[5], th.Dim),
		)
		if i == cursor {
			line = renderSelected(padCellANSI(line, w))
		}
		lines = append(lines, line)
	}

	if h <= 0 {
		return strings.Join(lines, "\n")
	}
	return clampCanvas(strings.Join(windowAround(lines, cursor, h), "\n"), w, h)
}

// windowAround returns the h-row slice of lines that keeps index
// cursor visible, centred where possible and clamped at both ends.
func windowAround(lines []string, cursor, h int) []string {
	if h >= len(lines) {
		return lines
	}
	start := cursor - h/2
	if start < 0 {
		start = 0
	}
	if start+h > len(lines) {
		start = len(lines) - h
	}
	return lines[start : start+h]
}

// deployOwnedPods resolves the deployment's pods by label selector.
// That's exact, unlike matching on the "<name>-" prefix, which also
// catches a "payments-api-worker" deployment's pods when you're
// looking at "payments-api". The prefix heuristic stays as a fallback
// for the case where no selector was projected at all.
func (m Model) deployOwnedPods(d deploymentRow) []podRow {
	out := make([]podRow, 0, 8)
	if len(d.Selector) > 0 {
		for _, p := range m.pods {
			if p.Namespace == d.Namespace && labelsMatch(p.Labels, d.Selector) {
				out = append(out, p)
			}
		}
	} else {
		prefix := d.Name + "-"
		for _, p := range m.pods {
			if p.Namespace == d.Namespace && strings.HasPrefix(p.Name, prefix) {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// labelsMatch reports whether labels satisfies every key/value in sel.
func labelsMatch(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// dashDeployEvents gathers the three event sources that matter for a
// deployment: the object itself (scaling), its ReplicaSets (rollout
// progress), and its pods — the last of which is where the failures
// that actually block a rollout show up (FailedScheduling, BackOff).
func (m Model) dashDeployEvents(d deploymentRow, owned []podRow) []eventRow {
	podNames := make(map[string]struct{}, len(owned))
	for _, p := range owned {
		podNames[p.Name] = struct{}{}
	}
	rsPrefix := d.Name + "-"

	out := make([]eventRow, 0, 16)
	for _, e := range m.events {
		if e.InvolvedNs != d.Namespace {
			continue
		}
		switch e.InvolvedKind {
		case "Deployment":
			if e.InvolvedName == d.Name {
				out = append(out, e)
			}
		case "ReplicaSet":
			if strings.HasPrefix(e.InvolvedName, rsPrefix) {
				out = append(out, e)
			}
		case "Pod":
			if _, ok := podNames[e.InvolvedName]; ok {
				out = append(out, e)
			}
		}
	}
	sortEventsNewestFirst(out)
	return out
}
