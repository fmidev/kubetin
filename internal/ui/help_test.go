package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/model"
)

func helpModel(w, h int) Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = w, h
	m.helpOpen = true
	return m
}

// The sheet is taller than most terminals, and before this it simply
// truncated: the bottom half was unreachable at any size.
func TestHelpScrollReachesTheEnd(t *testing.T) {
	m := helpModel(90, 20) // one column, definitely overflowing
	w, h := m.helpCanvas()
	lines := len(m.helpBody(w))
	if lines <= helpViewport(h) {
		t.Fatalf("fixture doesn't overflow: %d lines in a %d-row viewport", lines, helpViewport(h))
	}

	// The last group must be invisible at the top and visible at the end.
	if strings.Contains(m.renderHelp(90, h), "quit") {
		t.Error("fixture already shows the last binding; nothing to scroll")
	}
	end, _ := m.handleHelpKey(key("G"))
	if !strings.Contains(end.(Model).renderHelp(90, h), "quit") {
		t.Error("G did not reach the last binding")
	}
}

// Scrolling past either end must saturate rather than run away.
func TestHelpScrollClamps(t *testing.T) {
	m := helpModel(90, 20)
	var mm tea.Model = m
	for i := 0; i < 400; i++ {
		mm, _ = mm.(Model).handleHelpKey(key("j"))
	}
	max := mm.(Model).helpScroll
	if max == 0 {
		t.Fatal("scroll never moved")
	}
	mm, _ = mm.(Model).handleHelpKey(key("j"))
	if got := mm.(Model).helpScroll; got != max {
		t.Errorf("scroll kept growing past the end: %d then %d", max, got)
	}

	mm, _ = mm.(Model).handleHelpKey(key("k"))
	if got := mm.(Model).helpScroll; got != max-1 {
		t.Errorf("one k moved %d rows, want 1", max-got)
	}

	for i := 0; i < 400; i++ {
		mm, _ = mm.(Model).handleHelpKey(key("k"))
	}
	if got := mm.(Model).helpScroll; got != 0 {
		t.Errorf("scroll = %d at the top, want 0", got)
	}
}

// Closing resets the offset, so reopening starts at the top rather
// than wherever you left it.
func TestHelpCloseResetsScroll(t *testing.T) {
	m := helpModel(90, 20)
	scrolled, _ := m.handleHelpKey(key("G"))
	if scrolled.(Model).helpScroll == 0 {
		t.Fatal("G did not scroll")
	}
	closed, _ := scrolled.(Model).handleHelpKey(key("?"))
	c := closed.(Model)
	if c.helpOpen {
		t.Error("? did not close help")
	}
	if c.helpScroll != 0 {
		t.Errorf("helpScroll = %d after close, want 0", c.helpScroll)
	}
}

// Two columns when the terminal can hold them — that's what keeps the
// sheet on one screen at normal sizes, so scrolling is the exception.
func TestHelpUsesTwoColumnsWhenWide(t *testing.T) {
	wide := helpModel(140, 40)
	ww, _ := wide.helpCanvas()
	narrow := helpModel(80, 40)
	nw, _ := narrow.helpCanvas()

	wideLines := len(wide.helpBody(ww))
	narrowLines := len(narrow.helpBody(nw))
	if wideLines >= narrowLines {
		t.Errorf("wide layout is %d lines, narrow is %d — expected the wide one to be shorter",
			wideLines, narrowLines)
	}

	// Two groups on the same row is the observable signature.
	out := wide.renderHelp(140, 36)
	if !strings.Contains(out, "Move") || !strings.Contains(out, "Events lens") {
		t.Errorf("expected both column heads on screen:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Move") && strings.Contains(line, "Events lens") {
			return
		}
	}
	t.Error("no line carries two group titles; layout is not two-column")
}

// Every binding the help claims must be reachable by scrolling — a
// silent truncation is how the old sheet hid half its content.
func TestHelpRendersEveryBinding(t *testing.T) {
	m := helpModel(90, 20)
	w, _ := m.helpCanvas()
	body := strings.Join(m.helpBody(w), "\n")
	for _, g := range helpGroups {
		if !strings.Contains(body, g.Title) {
			t.Errorf("group %q missing from the rendered body", g.Title)
		}
	}
}

// The position indicator should only appear when something is off
// screen; otherwise it advertises scrolling that does nothing.
func TestHelpPositionIndicatorOnlyWhenOverflowing(t *testing.T) {
	if out := helpModel(140, 80).renderHelp(140, 76); strings.Contains(out, "j/k scroll") {
		t.Error("indicator shown on a terminal tall enough for the whole sheet")
	}
	if out := helpModel(90, 20).renderHelp(90, 16); !strings.Contains(out, "j/k scroll") {
		t.Error("indicator missing when content overflows")
	}
}

// `l` gives logs for the highlighted row, the same one-key shape `e`
// gives events and `i` gives the dashboard. Before this, logs were
// only reachable through the action menu.
func TestLogsKeyOpensForPod(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24
	m.view = ViewPods
	m.pods["p1"] = podRow{UID: "p1", Namespace: "default", Name: "api",
		Containers: []string{"app"}}
	m.cursor = "p1"

	var started LogStartMsg
	m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { started = req; return nil }

	out, cmd := m.handleKey(key("l"))
	if cmd != nil {
		cmd()
	}
	o := out.(Model)
	if !o.logs.open {
		t.Fatal("l did not open the log viewer")
	}
	if started.Ref.Name != "api" || started.Container != "app" {
		t.Errorf("streamed %+v, want the highlighted pod's only container", started)
	}
}

// Only pods and deployments have logs. Anything else has to say so
// rather than streaming the cursor's containers against a ref of the
// wrong kind.
func TestLogsKeyRejectsKindsWithoutLogs(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24
	m.view = ViewNodes
	m.nodes["n1"] = nodeRow{UID: "n1", Name: "worker-03"}
	m.cursor = "n1"

	called := false
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { called = true; return nil }

	out, _ := m.handleKey(key("l"))
	o := out.(Model)
	if called || o.logs.open {
		t.Error("l started a log stream for a Node")
	}
	if !strings.Contains(o.toast, "No logs for Node") {
		t.Errorf("toast = %q, want it to name the unsupported kind", o.toast)
	}
}

// No selection is a no-op, not a crash or a stream against an empty ref.
func TestLogsKeyWithoutSelection(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24
	m.view = ViewPods

	called := false
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { called = true; return nil }

	out, _ := m.handleKey(key("l"))
	if called || out.(Model).logs.open {
		t.Error("l opened logs with no row selected")
	}
}
