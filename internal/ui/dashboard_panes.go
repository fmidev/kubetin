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

	line2 := []string{
		dashField("cpu", formatCPU(r.CPUMilli), th.Base, th),
		dashField("mem", formatMem(r.MemBytes), th.Base, th),
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
	{min: 14, max: 46, prio: 3}, // IMAGE
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
			padCol(shortImage(ci.Image), cw[3], th.Dim),
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
// first — the reason plus message is the payload.
var dashEventColumns = []column{
	{min: 4, max: 5, prio: 2},    // AGE
	{min: 4, max: 7, prio: 3},    // TYPE
	{min: 10, max: 20, prio: 1},  // REASON
	{min: 14, max: 100, prio: 0}, // MESSAGE
}

// renderDashEvents lists the scoped events newest first.
func (m Model) renderDashEvents(events []eventRow, w, h, scroll int) string {
	th := m.Theme
	if len(events) == 0 {
		return dashPaneBody([]string{" " + th.Dim.Render("no events")}, w, h, 0)
	}
	cw := fitColumns(dashEventColumns, w-1)

	lines := make([]string, 0, len(events))
	for _, e := range events {
		typeStyle, msgStyle := th.Dim, th.Base
		if e.Type == "Warning" {
			typeStyle, msgStyle = th.StatusWrn, th.StatusWrn
		}
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" ×%d", e.Count)
		}
		lines = append(lines, " "+joinCells(
			padCol(formatAge(e.LastSeen), cw[0], th.Dim),
			padCol(shortEventType(e.Type), cw[1], typeStyle),
			padCol(e.Reason+count, cw[2], msgStyle),
			padCol(oneLine(e.Message), cw[3], th.Base),
		))
	}

	return dashPaneBody(lines, w, h, scroll)
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
// buffer the full-screen viewer uses — the dashboard pane is a second
// renderer over one stream, not a second stream.
func (m Model) renderDashLogs(w, h, scroll int) string {
	th := m.Theme
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
		out = append(out, " "+truncate(oneLine(ln), w-1))
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
