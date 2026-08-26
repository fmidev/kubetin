package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func dashModel(w, h int, extra func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = w, h
	m.view = ViewPods
	dashSetup(extra)(&m)
	return m
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	panic("unhandled key " + s)
}

// The wide frame's rects must tile the canvas exactly: no overlap, no
// gap, and every pane inside the border. A rect that runs one cell
// wide pushes the right border off and the whole render loses a column.
func TestDashLayoutTiles(t *testing.T) {
	for _, dim := range [][2]int{{120, 20}, {160, 30}, {200, 50}, {300, 80}} {
		w, h := dim[0], dim[1]
		for _, statusH := range []int{2, 3} {
			lay := dashLayoutFor(w, h, statusH, DefaultTheme())
			if !lay.wide {
				t.Fatalf("%dx%d statusH=%d: expected wide layout", w, h, statusH)
			}

			if lipgloss.Height(lay.frame) != h {
				t.Errorf("%dx%d: frame height = %d, want %d", w, h, lipgloss.Height(lay.frame), h)
			}
			for i, line := range strings.Split(lay.frame, "\n") {
				if got := lipgloss.Width(line); got != w {
					t.Errorf("%dx%d: frame row %d width = %d, want %d", w, h, i, got, w)
					break
				}
			}

			// Horizontal: full-width panes span the interior; the middle
			// row's two panes plus their divider must span the same.
			if lay.status.x != 1 || lay.status.w != w-2 {
				t.Errorf("%dx%d: status = %+v, want x=1 w=%d", w, h, lay.status, w-2)
			}
			if lay.logs.x != 1 || lay.logs.w != w-2 {
				t.Errorf("%dx%d: logs = %+v, want x=1 w=%d", w, h, lay.logs, w-2)
			}
			if got := lay.left.x + lay.left.w + 1; got != lay.right.x {
				t.Errorf("%dx%d: right.x = %d, want %d (left + divider)", w, h, lay.right.x, got)
			}
			if got := lay.left.w + 1 + lay.right.w; got != w-2 {
				t.Errorf("%dx%d: left+divider+right = %d, want %d", w, h, got, w-2)
			}

			// Vertical: one separator row between each band, and the
			// bottom border must land on the last row.
			if lay.status.y != 1 {
				t.Errorf("%dx%d: status.y = %d, want 1", w, h, lay.status.y)
			}
			if got := lay.status.y + lay.status.h + 1; got != lay.left.y {
				t.Errorf("%dx%d: mid row y = %d, want %d", w, h, lay.left.y, got)
			}
			if lay.right.y != lay.left.y || lay.right.h != lay.left.h {
				t.Errorf("%dx%d: mid panes misaligned: left=%+v right=%+v", w, h, lay.left, lay.right)
			}
			if got := lay.left.y + lay.left.h + 1; got != lay.logs.y {
				t.Errorf("%dx%d: logs.y = %d, want %d", w, h, lay.logs.y, got)
			}
			if got := lay.logs.y + lay.logs.h; got != h-1 {
				t.Errorf("%dx%d: logs bottom = %d, want %d (bottom border row)", w, h, got, h-1)
			}
		}
	}
}

// Below either breakpoint the wide frame can't hold panes worth
// reading, so the layout must fall back rather than squeeze.
func TestDashLayoutFallsBackToStacked(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		statusH int
	}{
		{"one column short", dashWideMinWidth - 1, 40, 2},
		{"one row short", 160, dashWideMinHeight - 1, 2},
		{"tall enough but status eats it", 160, 20, 12},
		{"tiny", 40, 12, 2},
	}
	for _, tc := range cases {
		if lay := dashLayoutFor(tc.w, tc.h, tc.statusH, DefaultTheme()); lay.wide {
			t.Errorf("%s (%dx%d statusH=%d): expected stacked, got wide",
				tc.name, tc.w, tc.h, tc.statusH)
		}
	}
	// And one cell over the line in both dimensions is wide.
	if lay := dashLayoutFor(dashWideMinWidth, dashWideMinHeight, 2, DefaultTheme()); !lay.wide {
		t.Error("exactly at the breakpoint should be wide")
	}
}

// Scrolling past the end must saturate, not run away: an unclamped
// offset means the user presses k ten times before the pane moves.
func TestDashScrollClampsAtBothEnds(t *testing.T) {
	m := dashModel(120, 24, nil)
	m.dashboard.focus = dashPaneEvents

	lay, r, ok := m.dashLayoutNow()
	if !ok || !lay.wide {
		t.Fatalf("expected a wide layout for the fixture, got ok=%v wide=%v", ok, lay.wide)
	}
	max := m.dashScrollMax(dashPaneEvents, lay, r)
	if max <= 0 {
		t.Fatalf("fixture should overflow the events pane; max = %d", max)
	}

	var mm tea.Model = m
	for i := 0; i < max+10; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("j"))
	}
	if got := mm.(Model).dashboard.scroll[dashPaneEvents]; got != max {
		t.Errorf("after scrolling past the end: scroll = %d, want %d", got, max)
	}

	// One k must move exactly one row back — proof nothing accumulated
	// beyond the clamp.
	mm, _ = mm.(Model).handleDashboardKey(key("k"))
	if got := mm.(Model).dashboard.scroll[dashPaneEvents]; got != max-1 {
		t.Errorf("after one k: scroll = %d, want %d", got, max-1)
	}

	for i := 0; i < max+10; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("k"))
	}
	if got := mm.(Model).dashboard.scroll[dashPaneEvents]; got != 0 {
		t.Errorf("after scrolling past the top: scroll = %d, want 0", got)
	}
}

// Logs count from the tail: 0 is "following", and j (down, towards
// newest) must not push the offset negative. The offset lives in
// logsState, shared with the full-screen viewer.
func TestDashLogScrollDirection(t *testing.T) {
	m := dashModel(120, 24, nil)
	m.dashboard.focus = dashPaneLogs

	var mm tea.Model = m
	mm, _ = mm.(Model).handleDashboardKey(key("k")) // up = older
	if got := mm.(Model).logs.scroll; got != 1 {
		t.Errorf("k should step back one line from the tail: got %d, want 1", got)
	}
	mm, _ = mm.(Model).handleDashboardKey(key("j")) // down = newer
	if got := mm.(Model).logs.scroll; got != 0 {
		t.Errorf("j should return to the tail: got %d, want 0", got)
	}
	mm, _ = mm.(Model).handleDashboardKey(key("j"))
	if got := mm.(Model).logs.scroll; got != 0 {
		t.Errorf("j at the tail should stay at 0, got %d", got)
	}
}

// Scrolling back in the dashboard's log pane must pause the tail, the
// way it does in the full-screen viewer. It didn't: the pane kept its
// own offset and never touched logs.follow, so applyLogLines' pinning
// never ran and the view slid forward under the user as lines arrived.
func TestDashLogScrollPausesAndPinsPosition(t *testing.T) {
	m := dashModel(120, 24, nil)
	m.dashboard.focus = dashPaneLogs

	// Scroll back a few lines.
	var mm tea.Model = m
	for i := 0; i < 3; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("k"))
	}
	cur := mm.(Model)
	if cur.logs.follow {
		t.Error("scrolling back should pause follow")
	}
	if cur.logs.scroll != 3 {
		t.Fatalf("scroll = %d, want 3", cur.logs.scroll)
	}
	if !strings.Contains(cur.logsPaneLabel(), "paused") {
		t.Errorf("pane label should report paused, got %q", cur.logsPaneLabel())
	}

	// The visible window must not move when new lines land.
	before := cur.renderDashLogs(60, 5)
	cur.applyLogLines([]string{"NEW-1", "NEW-2", "NEW-3"})
	if after := cur.renderDashLogs(60, 5); after != before {
		t.Errorf("paused pane slid with the tail:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if cur.logs.scroll != 6 {
		t.Errorf("scroll = %d, want 6 (3 back + 3 new lines to stay pinned)", cur.logs.scroll)
	}

	// G resumes following and jumps back to the tail.
	back, _ := cur.handleDashboardKey(key("G"))
	resumed := back.(Model)
	if !resumed.logs.follow || resumed.logs.scroll != 0 {
		t.Errorf("G should resume follow at the tail, got follow=%v scroll=%d",
			resumed.logs.follow, resumed.logs.scroll)
	}
	if !strings.Contains(resumed.renderDashLogs(60, 5), "NEW-3") {
		t.Error("after resuming, the pane should show the newest line")
	}
}

// f toggles follow from inside the dashboard, matching the viewer.
func TestDashLogFollowToggle(t *testing.T) {
	m := dashModel(120, 24, nil)
	m.dashboard.focus = dashPaneLogs

	off, _ := m.handleDashboardKey(key("f"))
	if off.(Model).logs.follow {
		t.Error("f should pause follow")
	}
	on, _ := off.(Model).handleDashboardKey(key("f"))
	if !on.(Model).logs.follow || on.(Model).logs.scroll != 0 {
		t.Errorf("f should resume follow at the tail, got follow=%v scroll=%d",
			on.(Model).logs.follow, on.(Model).logs.scroll)
	}
}

// Position carries into the full-screen viewer and back, since both
// render the same buffer at the same offset.
func TestDashLogScrollSurvivesViewerRoundTrip(t *testing.T) {
	m := dashModel(120, 24, nil)
	m.dashboard.focus = dashPaneLogs

	var mm tea.Model = m
	for i := 0; i < 4; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("k"))
	}
	want := mm.(Model).logs.scroll

	opened, _ := mm.(Model).handleDashboardKey(key("l"))
	if got := opened.(Model).logs.scroll; got != want {
		t.Errorf("viewer opened at scroll %d, want %d", got, want)
	}
	closed, _ := opened.(Model).closeLogs()
	if got := closed.(Model).logs.scroll; got != want {
		t.Errorf("back in the dashboard at scroll %d, want %d", got, want)
	}
}

// Tab cycles panes and wraps in both directions.
func TestDashTabCyclesPanes(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.dashboard.focus = dashPaneContainers
	var mm tea.Model = m

	want := []dashboardPane{dashPaneEvents, dashPaneLogs, dashPaneContainers}
	for i, w := range want {
		mm, _ = mm.(Model).handleDashboardKey(key("tab"))
		if got := mm.(Model).dashboard.focus; got != w {
			t.Fatalf("tab %d: focus = %d, want %d", i+1, got, w)
		}
	}
	mm, _ = mm.(Model).handleDashboardKey(key("shift+tab"))
	if got := mm.(Model).dashboard.focus; got != dashPaneLogs {
		t.Errorf("shift+tab should wrap backwards to logs, got %d", got)
	}
}

// Event messages routinely carry newlines (the scheduler's "0/5 nodes
// are available:" text especially). One unflattened \n inside a pane
// shifts every row below it and breaks the canvas height contract.
func TestOneLineFlattens(t *testing.T) {
	cases := map[string]string{
		"plain":            "plain",
		"a\nb":             "a b",
		"a\r\nb":           "a b",
		"0/5 nodes\n\n  x": "0/5 nodes x",
		"trailing\n":       "trailing",
	}
	for in, want := range cases {
		if got := oneLine(in); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The events pane is scoped to the target and ordered newest-first —
// an unsorted pane buries the event that explains the current state.
func TestDashEventsScopedAndSorted(t *testing.T) {
	m := dashModel(160, 40, func(m *Model) {
		// An event for a different pod must not leak into the pane.
		m.events["other"] = eventRow{
			UID: "other", Reason: "Scheduled", Type: "Normal",
			InvolvedKind: "Pod", InvolvedName: "some-other-pod", InvolvedNs: "default",
		}
	})
	t0, _ := m.dashboard.target()

	got := m.dashEventsFor(t0.Ref)
	if len(got) != 8 {
		t.Fatalf("len = %d, want 8 (the other pod's event must be excluded)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].LastSeen.Before(got[i].LastSeen) {
			t.Fatalf("events out of order at %d: %v before %v",
				i, got[i-1].LastSeen, got[i].LastSeen)
		}
	}
}

// Opening the full-screen viewer from the dashboard and closing it
// again must leave the stream alive — the dashboard's log pane renders
// that same buffer, and killing it would blank the pane.
func TestLogViewerCloseKeepsDashboardStream(t *testing.T) {
	stopped := false
	m := dashModel(160, 40, nil)
	m.OnLogsStop = func() { stopped = true }

	mm, _ := m.handleDashboardKey(key("l"))
	if !mm.(Model).logs.open {
		t.Fatal("l should open the full-screen log viewer")
	}

	back, _ := mm.(Model).closeLogs()
	if back.(Model).logs.open {
		t.Error("closeLogs should close the viewer")
	}
	if stopped {
		t.Error("closing the viewer killed the stream the dashboard is rendering")
	}
	if !back.(Model).logs.streaming {
		t.Error("stream should still be marked live after the viewer closes")
	}
}

// Closing the dashboard itself is the point at which the stream should
// actually stop.
func TestCloseDashboardStopsStream(t *testing.T) {
	stopped := false
	m := dashModel(160, 40, nil)
	m.OnLogsStop = func() { stopped = true }

	out, _ := m.handleDashboardKey(key("esc"))
	if out.(Model).dashboard.open {
		t.Error("esc on the last target should close the dashboard")
	}
	if !stopped {
		t.Error("closing the dashboard should stop the log stream")
	}
}

// A pod that is deleted while its dashboard is open must degrade to a
// message, not render a zero-value row as if it were live data.
func TestDashboardTargetGone(t *testing.T) {
	m := dashModel(160, 40, func(m *Model) {
		m.pods = nil
	})
	out := m.renderDashboard(30, 160)
	if !strings.Contains(out, "no longer present") {
		t.Errorf("expected a 'no longer present' notice, got:\n%s", out)
	}
}

// Known-denied pods/log must not fire a request we know will fail.
func TestDashboardSkipsLogsWhenDenied(t *testing.T) {
	m := dashModel(160, 40, nil)
	started := false
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { started = true; return nil }
	m.permissions = map[string]permState{
		cluster.PermissionKey("alpha", "get", "", "pods/log", "default"): {Allowed: false, Reason: "forbidden"},
	}

	ref := cluster.DescribeRef{Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: "default", Name: "dash-pod"}
	out, cmd := m.openDashboard(ref, "dash-uid")
	if cmd != nil {
		cmd()
	}
	if started {
		t.Error("dashboard fired a log request despite a cached RBAC denial")
	}
	if out.(Model).logs.err == "" {
		t.Error("expected the log pane to carry a permission message")
	}
}

// shortImage should drop a registry host but leave a bare repo alone —
// "nginx:1.2" has no host to strip.
func TestShortImage(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/fmidev/api:1.2":       "fmidev/api:1.2",
		"registry:5000/team/app:2":     "team/app:2",
		"nginx:1.25":                   "nginx:1.25",
		"library/nginx:1.25":           "library/nginx:1.25",
		"docker.io/library/nginx:1.25": "library/nginx:1.25",
	}
	for in, want := range cases {
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// The first non-True condition is the root cause; the ones after it
// are consequences, and DisruptionTarget is noise on a terminating pod.
func TestPodBlockingCondition(t *testing.T) {
	r := podRow{Conditions: []cluster.PodCondition{
		{Type: "PodScheduled", Status: "True"},
		{Type: "Initialized", Status: "False", Reason: "ContainersNotInitialized"},
		{Type: "Ready", Status: "False", Reason: "ContainersNotReady"},
	}}
	c, ok := podBlockingCondition(r)
	if !ok || c.Reason != "ContainersNotInitialized" {
		t.Errorf("got %+v (ok=%v), want the first failing condition", c, ok)
	}

	healthy := podRow{Conditions: []cluster.PodCondition{{Type: "Ready", Status: "True"}}}
	if _, ok := podBlockingCondition(healthy); ok {
		t.Error("an all-True pod should report no blocking condition")
	}

	terminating := podRow{Conditions: []cluster.PodCondition{
		{Type: "Ready", Status: "True"},
		{Type: "DisruptionTarget", Status: "False", Reason: "EvictionByEvictionAPI"},
	}}
	if _, ok := podBlockingCondition(terminating); ok {
		t.Error("DisruptionTarget should not count as blocking")
	}
}

// The dashboard's log pane must render log text exactly as the
// full-screen viewer does: default foreground. It regressed to grey
// because the pane was spliced into a dim border frame and inherited
// its colour — this pins the symptom, not just the spliceLine fix.
func TestDashboardLogTextMatchesViewer(t *testing.T) {
	withColour(t)

	m := dashModel(160, 40, nil)
	const needle = "GET /health"

	dashPrefix, ok := sgrBefore(m.renderDashboard(30, 160), needle)
	if !ok {
		t.Fatal("log line not found in the dashboard render")
	}
	viewerPrefix, ok := sgrBefore(m.renderLogs(160, 30), needle)
	if !ok {
		t.Fatal("log line not found in the viewer render")
	}

	if dashPrefix != viewerPrefix {
		t.Errorf("log text styled differently:\n dashboard = %q\n viewer    = %q",
			dashPrefix, viewerPrefix)
	}
	if strings.Contains(dashPrefix, "38;5;244") {
		t.Errorf("dashboard log text is dimmed by the frame colour: %q", dashPrefix)
	}
}

// sgrBefore returns the SGR escapes still open immediately before
// needle on the line containing it, tracking resets.
func sgrBefore(render, needle string) (string, bool) {
	for _, line := range strings.Split(render, "\n") {
		i := strings.Index(line, needle)
		if i < 0 {
			continue
		}
		var open []string
		for _, chunk := range strings.SplitAfter(line[:i], "m") {
			j := strings.LastIndex(chunk, "\x1b[")
			if j < 0 || !strings.HasSuffix(chunk, "m") {
				continue
			}
			code := chunk[j:]
			if code == "\x1b[0m" {
				open = open[:0]
				continue
			}
			open = append(open, code)
		}
		return strings.Join(open, ""), true
	}
	return "", false
}
