package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

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
	pw, ph := m.focusedPaneSize(lay, r)
	max := m.dashScrollMax(dashPaneEvents, r, pw, ph)
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
	m.dashboard.focus = dashPaneMain
	var mm tea.Model = m

	want := []dashboardPane{dashPaneEvents, dashPaneLogs, dashPaneMain}
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

func dashDeployModel(w, h int, extra func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = w, h
	m.view = ViewDeployments
	dashDeploySetup(extra)(&m)
	return m
}

// Owned pods come from the label selector, not the name prefix: a
// "payments-api-worker" deployment's pods share the "payments-api-"
// prefix but not the selector, and must not appear in the list.
func TestDeployOwnedPodsUseSelector(t *testing.T) {
	m := dashDeployModel(200, 50, nil)
	d := m.deployments["dep-uid"]

	got := m.deployOwnedPods(d)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got %v", len(got), podNames(got))
	}
	for _, p := range got {
		if p.Name == "payments-api-worker-1234-zzzzz" {
			t.Errorf("prefix-sharing pod from another deployment leaked in: %v", podNames(got))
		}
	}
	// Stable ordering so the cursor doesn't jump between renders.
	if !sortedByName(got) {
		t.Errorf("owned pods not sorted by name: %v", podNames(got))
	}
}

// With no selector projected we fall back to the prefix heuristic
// rather than showing an empty pane. The fallback is deliberately
// imprecise — "payments-api-worker-…" shares the "payments-api-"
// prefix and does get pulled in — which is exactly why the selector
// path above is preferred whenever a selector exists. This test pins
// that trade-off so the fallback isn't mistaken for an exact match.
func TestDeployOwnedPodsPrefixFallback(t *testing.T) {
	m := dashDeployModel(200, 50, func(m *Model) {
		d := m.deployments["dep-uid"]
		d.Selector = nil
		m.deployments["dep-uid"] = d
	})
	got := podNames(m.deployOwnedPods(m.deployments["dep-uid"]))

	// Every real replica is found: the pane is never empty when pods exist.
	for _, want := range []string{
		"payments-api-7f9c8-aaaaa",
		"payments-api-7f9c8-bbbbb",
		"payments-api-7f9c8-x2k4l",
	} {
		if !contains(got, want) {
			t.Errorf("fallback missed replica %q; got %v", want, got)
		}
	}
	// And the known over-match is present, unlike the selector path.
	if !contains(got, "payments-api-worker-1234-zzzzz") {
		t.Log("prefix fallback no longer over-matches; if that was deliberate, " +
			"update this test and the deployOwnedPods comment together")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// The events pane must aggregate the deployment, its ReplicaSets and
// its pods — the pod-level failures are the ones that explain a stuck
// rollout — while excluding anything from a neighbouring workload.
func TestDashDeployEventsAggregates(t *testing.T) {
	m := dashDeployModel(200, 50, func(m *Model) {
		m.events["foreign-evt"] = eventRow{
			UID: "foreign-evt", Namespace: "default", Type: "Warning", Reason: "BackOff",
			InvolvedKind: "Pod", InvolvedName: "payments-api-worker-1234-zzzzz",
			InvolvedNs: "default",
		}
	})
	d := m.deployments["dep-uid"]
	got := m.dashDeployEvents(d, m.deployOwnedPods(d))

	kinds := map[string]int{}
	for _, e := range got {
		kinds[e.InvolvedKind]++
		if e.InvolvedName == "payments-api-worker-1234-zzzzz" {
			t.Error("event from a neighbouring deployment's pod leaked in")
		}
	}
	for _, k := range []string{"Deployment", "ReplicaSet", "Pod"} {
		if kinds[k] == 0 {
			t.Errorf("no %s events in the aggregate (got %v)", k, kinds)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].LastSeen.Before(got[i].LastSeen) {
			t.Fatalf("aggregate not sorted newest-first at index %d", i)
		}
	}
}

// j/k in the PODS pane moves a selection, and i drills into it so Esc
// comes back to the deployment.
func TestDeployPodCursorAndDrillIn(t *testing.T) {
	m := dashDeployModel(200, 50, nil)
	m.dashboard.focus = dashPaneMain
	sub, _ := m.dashSubjectNow()
	n := len(sub.Pods)

	var mm tea.Model = m
	for i := 0; i < n+5; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("j"))
	}
	if got := mm.(Model).dashboard.podCursor; got != n-1 {
		t.Errorf("cursor = %d, want %d (clamped to the last replica)", got, n-1)
	}

	mm, _ = mm.(Model).handleDashboardKey(key("i"))
	got := mm.(Model).dashboard
	if len(got.stack) != 2 {
		t.Fatalf("stack depth = %d, want 2 after drilling in", len(got.stack))
	}
	top, _ := got.target()
	if top.Ref.Kind != "Pod" || top.Ref.Name != sub.Pods[n-1].Name {
		t.Errorf("drilled into %+v, want pod %q", top.Ref, sub.Pods[n-1].Name)
	}

	back, _ := mm.(Model).handleDashboardKey(key("esc"))
	if d := back.(Model).dashboard; len(d.stack) != 1 || !d.open {
		t.Errorf("esc should pop back to the deployment, got depth %d open=%v",
			len(d.stack), d.open)
	}
}

// The log pane streams the newest Running replica; a deployment whose
// replicas are all broken still streams something rather than nothing.
func TestNewestRunningPod(t *testing.T) {
	base := time.Now()
	pods := []podRow{
		{Name: "old-running", Phase: "Running", CreatedAt: base.Add(-2 * time.Hour)},
		{Name: "new-running", Phase: "Running", CreatedAt: base.Add(-1 * time.Hour)},
		{Name: "newest-pending", Phase: "Pending", CreatedAt: base},
	}
	got, ok := newestRunningPod(pods)
	if !ok || got.Name != "new-running" {
		t.Errorf("got %q (ok=%v), want new-running", got.Name, ok)
	}

	allBroken := []podRow{
		{Name: "old-pending", Phase: "Pending", CreatedAt: base.Add(-time.Hour)},
		{Name: "new-pending", Phase: "Pending", CreatedAt: base},
	}
	got, ok = newestRunningPod(allBroken)
	if !ok || got.Name != "new-pending" {
		t.Errorf("all-broken fallback: got %q (ok=%v), want new-pending", got.Name, ok)
	}

	if _, ok := newestRunningPod(nil); ok {
		t.Error("empty list should report no pod")
	}
}

// Progressing=True is both the healthy steady state and the mid-
// rollout state, so only False counts as blocking.
func TestDeployBlockingCondition(t *testing.T) {
	healthy := deploymentRow{Conditions: []cluster.DeployCondition{
		{Type: "Available", Status: "True"},
		{Type: "Progressing", Status: "True", Reason: "NewReplicaSetAvailable"},
	}}
	if c, ok := deployBlockingCondition(healthy); ok {
		t.Errorf("healthy deployment reported blocking condition %+v", c)
	}

	stalled := deploymentRow{Conditions: []cluster.DeployCondition{
		{Type: "Available", Status: "True"},
		{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded"},
	}}
	c, ok := deployBlockingCondition(stalled)
	if !ok || c.Reason != "ProgressDeadlineExceeded" {
		t.Errorf("got %+v (ok=%v), want the stalled Progressing condition", c, ok)
	}

	down := deploymentRow{Conditions: []cluster.DeployCondition{
		{Type: "Available", Status: "False", Reason: "MinimumReplicasUnavailable"},
	}}
	if c, ok := deployBlockingCondition(down); !ok || c.Reason != "MinimumReplicasUnavailable" {
		t.Errorf("got %+v (ok=%v), want MinimumReplicasUnavailable", c, ok)
	}
}

func TestFormatSelectorIsStable(t *testing.T) {
	sel := map[string]string{"tier": "backend", "app": "payments", "env": "prod"}
	want := "app=payments,env=prod,tier=backend"
	for i := 0; i < 10; i++ {
		if got := formatSelector(sel); got != want {
			t.Fatalf("formatSelector = %q, want %q (map order must not leak)", got, want)
		}
	}
	if got := formatSelector(nil); got != "" {
		t.Errorf("formatSelector(nil) = %q, want empty", got)
	}
}

func podNames(pods []podRow) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}
	return out
}

func sortedByName(pods []podRow) bool {
	for i := 1; i < len(pods); i++ {
		if pods[i-1].Name > pods[i].Name {
			return false
		}
	}
	return true
}

// The stacked column used to be a fixed height regardless of the
// terminal: at 60 rows it drew 32 and left 24 blank under a log pane
// frozen at 10 lines. Logs now absorb the slack.
func TestStackedLogsFillTallWindow(t *testing.T) {
	for _, h := range []int{32, 40, 50, 60, 100} {
		m := dashModel(70, h, func(m *Model) {
			m.logs.lines = nil
			for i := 0; i < 300; i++ {
				m.logs.lines = append(m.logs.lines, "line")
			}
		})
		_, canvas := m.dashCanvasSize()
		sub, ok := m.dashSubjectNow()
		if !ok {
			t.Fatal("no subject")
		}
		if got := lipgloss.Height(m.stackedBody(sub, 70, canvas)); got != canvas {
			t.Errorf("h=%d: stacked body is %d rows for a %d-row canvas (%+d)",
				h, got, canvas, got-canvas)
		}
	}
}

// A taller window has to mean more log lines, not more blank space.
func TestStackedLogsGrowWithHeight(t *testing.T) {
	count := func(h int) int {
		m := dashModel(70, h, func(m *Model) {
			m.logs.lines = nil
			for i := 0; i < 300; i++ {
				m.logs.lines = append(m.logs.lines, fmt.Sprintf("logline-%03d", i))
			}
		})
		return strings.Count(m.View(), "logline-")
	}
	short, tall := count(34), count(60)
	if tall <= short {
		t.Errorf("60-row window shows %d log lines, 34-row shows %d — taller must show more",
			tall, short)
	}
}

// Below the point where the column can fit, the canvas scrolls — the
// behaviour short terminals always had. It must not panic or clip the
// panes above logs.
func TestStackedShortWindowStillScrolls(t *testing.T) {
	for _, h := range []int{12, 16, 20, 26} {
		m := dashModel(70, h, nil)
		_, canvas := m.dashCanvasSize()
		sub, ok := m.dashSubjectNow()
		if !ok {
			t.Fatal("no subject")
		}
		body := m.stackedBody(sub, 70, canvas)
		if lipgloss.Height(body) < canvas {
			t.Errorf("h=%d: body %d rows is under the %d-row canvas; it should fill or overflow",
				h, lipgloss.Height(body), canvas)
		}
		if got := lipgloss.Height(m.renderDashboard(canvas, 70)); got != canvas {
			t.Errorf("h=%d: rendered %d rows, want %d", h, got, canvas)
		}
	}
}

// The log pane never drops below its floor, however cramped the rest.
// stackedLogRows returns how many *content* rows the logs box shows —
// its interior, excluding the two border rows. Counting the borders as
// content is how the first version of this test passed with the floor
// regressed from 5 to 3.
func stackedLogRows(t *testing.T, body string) int {
	t.Helper()
	idx := strings.Index(body, "LOGS")
	if idx < 0 {
		t.Fatal("no logs pane in the stacked body")
	}
	// Logs is the last box, so the slice runs: the top border row
	// carrying the LOGS label, the interior, then the bottom border.
	rows := strings.Split(body[idx:], "\n")
	if len(rows) < 2 {
		t.Fatalf("logs box is only %d rows; it has no interior", len(rows))
	}
	if last := rows[len(rows)-1]; !strings.Contains(last, "└") {
		t.Fatalf("logs box has no bottom border; last row = %q", truncForErr(last))
	}
	return len(rows) - 2
}

// The pane never drops below its floor however cramped the panes above
// it. The floor is what stops a busy events list squeezing logs down to
// nothing on a short terminal.
func TestStackedLogsRespectFloor(t *testing.T) {
	m := dashModel(70, 14, func(m *Model) {
		// Force the panes above logs to be as tall as they can be.
		now := time.Now()
		for i := 0; i < 40; i++ {
			uid := types.UID("evt" + string(rune('a'+i%26)) + string(rune('A'+i/26)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Warning",
				Reason: "R" + string(rune('a'+i%26)), Message: "m", Count: 1,
				LastSeen: now, InvolvedKind: "Pod", InvolvedName: "dash-pod", InvolvedNs: "default",
			}
		}
		// Unique lines so the structural count can be cross-checked
		// against what is actually on screen.
		m.logs.lines = nil
		for i := 0; i < 200; i++ {
			m.logs.lines = append(m.logs.lines, fmt.Sprintf("floorline-%03d", i))
		}
	})
	_, canvas := m.dashCanvasSize()
	sub, _ := m.dashSubjectNow()
	body := m.stackedBody(sub, 70, canvas)

	if got := stackedLogRows(t, body); got < dashStackLogMin {
		t.Errorf("logs pane interior is %d rows, below the %d floor", got, dashStackLogMin)
	}

	// And independently: that many log lines are genuinely rendered,
	// not just that many rows of padding.
	if got := strings.Count(body, "floorline-"); got < dashStackLogMin {
		t.Errorf("only %d log lines visible, want at least %d", got, dashStackLogMin)
	}
}

// A pane renderer must not panic on a degenerate height. The stacked
// layout's floor keeps this from arising today, but the renderer
// shouldn't depend on its caller for that: `make([]string, 0, h)` with
// a negative h takes down the whole TUI.
func TestRenderDashLogsSurvivesDegenerateHeight(t *testing.T) {
	m := dashModel(70, 30, nil)
	for _, h := range []int{-10, -1, 0, 1} {
		for _, w := range []int{-5, 0, 1, 40} {
			out := m.renderDashLogs(w, h) // must not panic
			if got := lipgloss.Height(out); got < 1 {
				t.Errorf("w=%d h=%d: rendered %d rows, want at least 1", w, h, got)
			}
		}
	}
}

// openedDash goes through the real entry point, so focus and canvas
// are whatever a user actually gets — dashSetup alone leaves focus at
// the zero value, which is how the first version of these tests
// managed to exercise the wrong pane.
func openedDash(t *testing.T, w, h int, extra func(*Model)) Model {
	t.Helper()
	m := dashModel(w, h, extra)
	m.dashboard = dashboardState{}
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
	out, _ := m.openDashboard(cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: "default", Name: "dash-pod",
	}, types.UID("dash-uid"))
	o := out.(Model)
	// After opening, not before: beginLogStreamTail resets the buffer,
	// so lines seeded earlier are thrown away and the pane has nothing
	// to scroll.
	o.logs.lines = nil
	for i := 0; i < 300; i++ {
		o.logs.lines = append(o.logs.lines, fmt.Sprintf("logline-%03d", i))
	}
	return o
}

// j/k must scroll the log pane in *both* layouts. Stacked mode routed
// them to the canvas offset instead, so once the column started
// filling the canvas exactly there was nothing left to move and the
// keys did nothing at all.
func TestDashLogsScrollInBothLayouts(t *testing.T) {
	for _, dim := range [][2]int{{160, 40}, {70, 40}, {70, 30}} {
		w, h := dim[0], dim[1]
		m := openedDash(t, w, h, nil)
		if m.dashboard.focus != dashPaneLogs {
			t.Fatalf("%dx%d: dashboard opened focused on pane %d, want logs", w, h, m.dashboard.focus)
		}

		var mm tea.Model = m
		for i := 0; i < 5; i++ {
			mm, _ = mm.(Model).handleDashboardKey(key("k"))
		}
		a := mm.(Model)
		if a.logs.scroll != 5 {
			t.Errorf("%dx%d: five k moved the log offset to %d, want 5", w, h, a.logs.scroll)
		}
		if a.logs.follow {
			t.Errorf("%dx%d: scrolling back should pause follow", w, h)
		}

		// And it has to be visible, not just recorded in state.
		_, canvas := a.dashCanvasSize()
		before := m.renderDashboard(canvas, w)
		after := a.renderDashboard(canvas, w)
		if before == after {
			t.Errorf("%dx%d: the rendered dashboard is unchanged after scrolling", w, h)
		}
	}
}

// The events pane never scrolled in the stacked layout either: it was
// rendered at natural height and clipped from the top, so its own
// offset was ignored.
func TestDashEventsScrollWhenStacked(t *testing.T) {
	// The pane caps at dashStackEventsMax rows, so it needs more
	// groups than that before there is anything to scroll.
	m := openedDash(t, 70, 40, func(m *Model) {
		now := time.Now()
		for i := 0; i < 30; i++ {
			uid := types.UID("scroll-evt-" + string(rune('a'+i%26)) + string(rune('A'+i/26)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Warning",
				Reason: "Reason" + string(rune('a'+i%26)), Message: "m", Count: 1,
				LastSeen:     now.Add(-time.Duration(i) * time.Minute),
				InvolvedKind: "Pod", InvolvedName: "dash-pod", InvolvedNs: "default",
			}
		}
	})
	m.dashboard.focus = dashPaneEvents

	lay, sub, _ := m.dashLayoutNow()
	if lay.wide {
		t.Fatal("fixture should be the stacked layout")
	}
	pw, ph := m.focusedPaneSize(lay, sub)
	if max := m.dashScrollMax(dashPaneEvents, sub, pw, ph); max <= 0 {
		t.Fatalf("fixture should overflow the events pane; max = %d", max)
	}

	before := m.renderDashboard(30, 70)
	var mm tea.Model = m
	for i := 0; i < 3; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("j"))
	}
	a := mm.(Model)
	if a.dashboard.scroll[dashPaneEvents] != 3 {
		t.Errorf("events offset = %d, want 3", a.dashboard.scroll[dashPaneEvents])
	}
	if a.renderDashboard(30, 70) == before {
		t.Error("events pane looks identical after scrolling")
	}
}

// On a window too short for the whole column, the focused pane has to
// be brought into view — otherwise Tab moves a highlight below the
// fold and j/k drives something invisible.
func TestDashFocusedPaneRevealedWhenStackedOverflows(t *testing.T) {
	m := openedDash(t, 70, 20, nil)
	_, canvas := m.dashCanvasSize()

	sub, _ := m.dashSubjectNow()
	if lipgloss.Height(m.stackedBody(sub, 70, canvas)) <= canvas {
		t.Fatal("fixture should overflow the canvas")
	}
	if !strings.Contains(m.renderDashboard(canvas, 70), "LOGS") {
		t.Error("opened focused on logs, but the logs pane is off screen")
	}

	// Tab round the panes; each must be on screen once focused.
	var mm tea.Model = m
	for _, want := range []string{"CONTAINERS", "EVENTS", "LOGS"} {
		mm, _ = mm.(Model).handleDashboardKey(key("tab"))
		a := mm.(Model)
		if !strings.Contains(a.renderDashboard(canvas, 70), want) {
			t.Errorf("after Tab to %s, that pane is not visible", want)
		}
	}
}

// A window tall enough for everything shows every pane at once, and
// tabbing round must not scroll anything away.
func TestDashNoCanvasScrollWhenColumnFits(t *testing.T) {
	m := openedDash(t, 70, 44, nil)
	_, canvas := m.dashCanvasSize()

	var mm tea.Model = m
	for i := 0; i <= int(dashPaneCount); i++ {
		out := mm.(Model).renderDashboard(canvas, 70)
		for _, pane := range []string{"CONTAINERS", "EVENTS", "LOGS"} {
			if !strings.Contains(out, pane) {
				t.Fatalf("tab %d: %s scrolled off a window that fits the whole column", i, pane)
			}
		}
		mm, _ = mm.(Model).handleDashboardKey(key("tab"))
	}
}

// Resizing is its own path into the bug this PR fixes: the layout is
// sized against the canvas, so dragging a terminal narrower can move
// the focused pane below the fold. WindowSizeMsg only updated the
// dimensions, so j/k went back to driving a pane the user can't see.
func TestDashResizeRevealsFocusedPane(t *testing.T) {
	cases := []struct {
		name       string
		from, to   [2]int
		wantHidden bool // whether the pane would be off screen unrevealed
	}{
		{"wide to overflowing stacked", [2]int{160, 40}, [2]int{70, 20}, true},
		{"tall stacked to short stacked", [2]int{70, 44}, [2]int{70, 20}, true},
		{"short stacked to tall stacked", [2]int{70, 20}, [2]int{70, 44}, false},
		{"stacked to wide", [2]int{70, 20}, [2]int{200, 50}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := openedDash(t, tc.from[0], tc.from[1], nil)
			out, _ := m.Update(tea.WindowSizeMsg{Width: tc.to[0], Height: tc.to[1]})
			r := out.(Model)

			_, canvas := r.dashCanvasSize()
			if !strings.Contains(r.renderDashboard(canvas, tc.to[0]), "LOGS") {
				t.Error("focused pane off screen after resize")
			}

			// The whole render must still be exactly the canvas.
			if got := lipgloss.Height(r.renderDashboard(canvas, tc.to[0])); got != canvas {
				t.Errorf("rendered %d rows, want %d", got, canvas)
			}
		})
	}
}

// Growing back into a window that fits must show the top of the column
// again, not leave it scrolled where the smaller layout had it.
func TestDashResizeShowsColumnTopWhenItFits(t *testing.T) {
	m := openedDash(t, 70, 20, nil)
	var mm tea.Model = m
	for i := 0; i < 3; i++ {
		mm, _ = mm.(Model).handleDashboardKey(key("tab"))
	}
	small := mm.(Model)
	_, smallCanvas := small.dashCanvasSize()
	if strings.Contains(small.renderDashboard(smallCanvas, 70), "CONTAINERS") {
		t.Fatal("fixture should have scrolled the top pane away")
	}

	out, _ := small.Update(tea.WindowSizeMsg{Width: 70, Height: 60})
	r := out.(Model)
	_, canvas := r.dashCanvasSize()
	if !strings.Contains(r.renderDashboard(canvas, 70), "CONTAINERS") {
		t.Error("growing the window left the top of the column scrolled away")
	}
}

// A resize while the dashboard is closed must not touch its state.
func TestResizeIgnoresClosedDashboard(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.dashboard = dashboardState{}
	out, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	if r := out.(Model); r.dashboard.open {
		t.Errorf("resize disturbed a closed dashboard: %+v", r.dashboard)
	}
}

// Pane heights come from live cluster data, so the column reflows
// while the dashboard is open: a burst of events grows that pane from
// one row to eight and pushes a focused logs pane below the fold. With
// the offset remembered rather than derived, nothing recomputed it —
// no key was pressed and no resize happened — and j/k then scrolled a
// pane the user could not see, with no way back except cycling focus.
func TestDashFocusedPaneStaysVisibleAsPanesGrow(t *testing.T) {
	m := openedDash(t, 70, 22, func(m *Model) {
		// Start with a single event so the events pane is one row.
		m.events = map[types.UID]eventRow{"seed": {
			UID: "seed", Namespace: "default", Type: "Normal", Reason: "Started",
			Message: "started", Count: 1, LastSeen: time.Now(),
			InvolvedKind: "Pod", InvolvedName: "dash-pod", InvolvedNs: "default",
		}}
	})
	_, canvas := m.dashCanvasSize()
	if !strings.Contains(m.renderDashboard(canvas, 70), "LOGS") {
		t.Fatal("fixture should start with the logs pane visible")
	}

	// Events stream in, growing the pane above logs.
	var mm tea.Model = m
	for i := 1; i <= 12; i++ {
		mm, _ = mm.(Model).Update(EvtEventMsg(cluster.EventEvent{
			Kind: cluster.EvtAdded, Context: "alpha",
			UID:       types.UID(fmt.Sprintf("grow-%02d", i)),
			Namespace: "default", Type: "Warning",
			Reason: fmt.Sprintf("Reason%02d", i), Message: "m", Count: 1,
			LastSeen: time.Now(), InvolvedKind: "Pod",
			InvolvedName: "dash-pod", InvolvedNs: "default",
		}))
	}
	a := mm.(Model)

	if !strings.Contains(a.renderDashboard(canvas, 70), "LOGS") {
		t.Error("focused logs pane pushed off screen by incoming events")
	}
	// And it must still be the pane j/k drives.
	if a.dashboard.focus != dashPaneLogs {
		t.Errorf("focus drifted to pane %d", a.dashboard.focus)
	}
}

// The same reflow through the pod informer rather than events.
func TestDashFocusedPaneStaysVisibleAsContainersGrow(t *testing.T) {
	m := openedDash(t, 70, 22, func(m *Model) {
		r := m.pods["dash-uid"]
		r.ContainerInfo = r.ContainerInfo[:1]
		r.InitContainerInfo = nil
		m.pods["dash-uid"] = r
	})
	_, canvas := m.dashCanvasSize()
	if !strings.Contains(m.renderDashboard(canvas, 70), "LOGS") {
		t.Fatal("fixture should start with the logs pane visible")
	}

	many := make([]cluster.ContainerInfo, 0, 8)
	for i := 0; i < 8; i++ {
		many = append(many, cluster.ContainerInfo{
			Name: fmt.Sprintf("c%d", i), Image: "img:1", Ready: true,
			State: cluster.ContainerReady,
		})
	}
	out, _ := m.Update(PodEventMsg(cluster.PodEvent{
		Kind: cluster.PodUpdated, Context: "alpha", UID: "dash-uid",
		Namespace: "default", Name: "dash-pod", Phase: "Running",
		ContainerInfo: many,
	}))
	if !strings.Contains(out.(Model).renderDashboard(canvas, 70), "LOGS") {
		t.Error("focused logs pane pushed off screen by a pod update")
	}
}
