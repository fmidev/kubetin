package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func lensModel(w, h int, extra func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = w, h
	m.view = ViewPods
	m.pods["p1"] = podRow{UID: "p1", Namespace: "default", Name: "payments-api-7f9c8-x2k4l", Phase: "Running"}
	m.cursor = "p1"
	eventsLens(extra)(&m)
	return m
}

// The lens is a lens, not a place: opening it leaves the underlying
// view and cursor untouched so Esc puts you back exactly where you
// were. The old events *view* reset the cursor on the way in and out.
func TestEventsLensPreservesUnderlyingPosition(t *testing.T) {
	m := lensModel(120, 24, nil)
	m.eventsLens = eventsLensState{}
	m.view = ViewDeployments
	m.cursor = "d7"

	opened, _ := m.handleKey(key("e"))
	o := opened.(Model)
	if !o.eventsLens.open {
		t.Fatal("e did not open the lens")
	}
	if o.view != ViewDeployments || o.cursor != "d7" {
		t.Errorf("opening moved the view/cursor: view=%v cursor=%q", o.view, o.cursor)
	}

	closed, _ := o.handleEventsKey(key("esc"))
	c := closed.(Model)
	if c.eventsLens.open {
		t.Error("esc did not close the lens")
	}
	if c.view != ViewDeployments || c.cursor != "d7" {
		t.Errorf("closing moved the view/cursor: view=%v cursor=%q", c.view, c.cursor)
	}
}

// e scopes to the highlighted row; E opens everything.
func TestEventsLensScopeFromKeys(t *testing.T) {
	m := lensModel(120, 24, nil)
	m.eventsLens = eventsLensState{}

	scoped, _ := m.handleKey(key("e"))
	s := scoped.(Model).eventsLens
	if s.scope == nil {
		t.Fatal("e should scope to the cursor row")
	}
	if s.scope.Kind != "Pod" || s.scope.Name != "payments-api-7f9c8-x2k4l" {
		t.Errorf("scope = %+v, want the highlighted pod", s.scope)
	}

	all, _ := m.handleKey(key("E"))
	if a := all.(Model).eventsLens; !a.open || a.scope != nil {
		t.Errorf("E should open unscoped, got open=%v scope=%+v", a.open, a.scope)
	}
}

// Scope is snapshotted on press, not tracked live — otherwise the list
// churns under you as the cursor moves while you're reading it.
func TestEventsLensScopeIsSnapshot(t *testing.T) {
	m := lensModel(120, 24, nil)
	m.eventsLens = eventsLensState{}
	m.pods["p2"] = podRow{UID: "p2", Namespace: "default", Name: "other-pod", Phase: "Running"}

	opened, _ := m.handleKey(key("e"))
	o := opened.(Model)
	o.cursor = "p2" // cursor moves underneath

	if o.eventsLens.scope.Name != "payments-api-7f9c8-x2k4l" {
		t.Errorf("scope followed the cursor to %q; it should be a snapshot",
			o.eventsLens.scope.Name)
	}
}

// The three scope levels, and the label naming each. The namespace
// level is the one that used to be broken: the events view ignored
// m.namespace entirely, so `n: kube-system` showed the whole cluster.
func TestScopedEventsThreeLevels(t *testing.T) {
	base := func(extra func(*Model)) Model { return lensModel(120, 24, extra) }

	t.Run("object", func(t *testing.T) {
		m := base(func(m *Model) {
			m.eventsLens.scope = &eventScopeRef{
				Kind: "Pod", Namespace: "default", Name: "payments-api-7f9c8-x2k4l",
			}
		})
		got, label := m.scopedEvents()
		for _, e := range got {
			if e.InvolvedNs != "default" {
				t.Errorf("object scope leaked an event from %q", e.InvolvedNs)
			}
		}
		if !strings.Contains(label, "Pod/payments-api-7f9c8-x2k4l") {
			t.Errorf("label = %q, want it to name the object", label)
		}
	})

	t.Run("namespace", func(t *testing.T) {
		m := base(func(m *Model) { m.namespace = "kube-system" })
		got, label := m.scopedEvents()
		if len(got) == 0 {
			t.Fatal("namespace scope matched nothing")
		}
		for _, e := range got {
			if e.InvolvedNs != "kube-system" {
				t.Errorf("namespace scope leaked an event from %q", e.InvolvedNs)
			}
		}
		if label != "namespace kube-system" {
			t.Errorf("label = %q, want %q", label, "namespace kube-system")
		}
	})

	t.Run("cluster", func(t *testing.T) {
		m := base(nil)
		got, label := m.scopedEvents()
		if len(got) != len(m.events) {
			t.Errorf("cluster scope returned %d of %d events", len(got), len(m.events))
		}
		if label != "all namespaces" {
			t.Errorf("label = %q, want %q", label, "all namespaces")
		}
	})
}

// E inside the lens widens without closing it.
func TestEventsLensWidenInPlace(t *testing.T) {
	m := lensModel(120, 24, func(m *Model) {
		m.eventsLens.scope = &eventScopeRef{Kind: "Pod", Namespace: "default", Name: "payments-api-7f9c8-x2k4l"}
		m.eventsLens.scroll = 4
	})

	out, _ := m.handleEventsKey(key("E"))
	o := out.(Model)
	if !o.eventsLens.open {
		t.Error("E should widen in place, not close")
	}
	if o.eventsLens.scope != nil {
		t.Error("E should drop the object scope")
	}
	if o.eventsLens.scroll != 0 {
		t.Errorf("scroll = %d, want reset to 0 after widening", o.eventsLens.scroll)
	}
}

// e closes as well as opens, so glancing at events is press-press.
func TestEventsLensToggleClosesWithE(t *testing.T) {
	m := lensModel(120, 24, nil)
	out, _ := m.handleEventsKey(key("e"))
	if out.(Model).eventsLens.open {
		t.Error("e inside the lens should close it")
	}
}

// Scrolling past the end must saturate.
func TestEventsLensScrollClamps(t *testing.T) {
	m := lensModel(120, 20, func(m *Model) {
		now := time.Now()
		for i := 0; i < 40; i++ {
			uid := types.UID("bulk-" + string(rune('A'+i%26)) + string(rune('a'+i/26)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Normal",
				Reason: "Reason" + string(rune('A'+i%26)), Message: "message",
				Count: 1, LastSeen: now.Add(-time.Duration(i) * time.Second),
				InvolvedKind: "Pod", InvolvedName: "p", InvolvedNs: "default",
			}
		}
	})

	var mm tea.Model = m
	for i := 0; i < 500; i++ {
		mm, _ = mm.(Model).handleEventsKey(key("j"))
	}
	saturated := mm.(Model).eventsLens.scroll
	if saturated == 0 {
		t.Fatal("fixture should overflow the lens")
	}

	mm, _ = mm.(Model).handleEventsKey(key("j"))
	if got := mm.(Model).eventsLens.scroll; got != saturated {
		t.Errorf("scroll kept growing past the end: %d then %d", saturated, got)
	}

	mm, _ = mm.(Model).handleEventsKey(key("k"))
	if got := mm.(Model).eventsLens.scroll; got != saturated-1 {
		t.Errorf("one k moved %d rows, want exactly 1", saturated-got)
	}

	for i := 0; i < 500; i++ {
		mm, _ = mm.(Model).handleEventsKey(key("k"))
	}
	if got := mm.(Model).eventsLens.scroll; got != 0 {
		t.Errorf("scroll = %d after scrolling to the top, want 0", got)
	}
}

// Switching clusters closes the lens: its target doesn't exist there.
func TestEventsLensClosedOnClusterSwitch(t *testing.T) {
	m := lensModel(120, 24, nil)
	out, _ := m.Update(PodsClearedMsg{})
	if out.(Model).eventsLens.open {
		t.Error("lens survived a cluster switch")
	}
}

// The action menu's Events item opens the lens rather than navigating.
func TestActionMenuEventsOpensLens(t *testing.T) {
	m := lensModel(120, 24, nil)
	m.eventsLens = eventsLensState{}
	m.actionMenu.open = true
	m.actionMenu.ref = cluster.DescribeRef{
		Kind: "Deployment", Namespace: "default", Name: "payments",
	}

	out, _ := m.executeAction(ActEvents)
	o := out.(Model)
	if !o.eventsLens.open || o.eventsLens.scope == nil {
		t.Fatal("Events action should open the lens scoped to the ref")
	}
	if o.eventsLens.scope.Kind != "Deployment" || o.eventsLens.scope.Name != "payments" {
		t.Errorf("scope = %+v, want the menu's ref", o.eventsLens.scope)
	}
	if o.view != ViewPods {
		t.Errorf("view changed to %v; the lens should not navigate", o.view)
	}
	if o.actionMenu.open {
		t.Error("action menu stayed open behind the lens")
	}
}

// Empty states have to distinguish "this object has none" from "the
// cluster has none" — you pressed e expecting an explanation.
func TestEventsLensEmptyStates(t *testing.T) {
	scoped := lensModel(120, 24, func(m *Model) {
		m.events = map[types.UID]eventRow{}
		m.eventsLens.scope = &eventScopeRef{Kind: "Pod", Namespace: "default", Name: "gone"}
	})
	if out := scoped.renderEventsLens(120, 20); !strings.Contains(out, "no events for this pod") {
		t.Errorf("scoped empty state missing:\n%s", out)
	}

	ns := lensModel(120, 24, func(m *Model) {
		m.events = map[types.UID]eventRow{}
		m.namespace = "kube-system"
	})
	if out := ns.renderEventsLens(120, 20); !strings.Contains(out, "no events in namespace kube-system") {
		t.Errorf("namespace empty state missing:\n%s", out)
	}
}

// The box must occupy exactly the canvas it was given. At one row too
// many, View's clampCanvas silently eats the bottom border — the
// layout test still passes, because it asserts dimensions rather than
// an intact frame.
func TestEventsLensBoxFitsItsCanvas(t *testing.T) {
	for _, dim := range [][2]int{{110, 12}, {80, 8}, {200, 40}, {50, 6}} {
		w, h := dim[0], dim[1]
		m := lensModel(w, h+4, nil)
		out := m.renderEventsLens(w, h)

		if got := lipgloss.Height(out); got != h {
			t.Errorf("%dx%d: rendered %d rows, want %d", w, h, got, h)
		}
		rows := strings.Split(out, "\n")
		if !strings.Contains(rows[len(rows)-1], "└") {
			t.Errorf("%dx%d: bottom border missing — last row = %q",
				w, h, strings.TrimSpace(rows[len(rows)-1]))
		}
		if !strings.Contains(rows[0], "┌") {
			t.Errorf("%dx%d: top border missing", w, h)
		}
	}
}

// A reason from a custom controller can be arbitrarily long. Unbounded
// it wraps inside the box, which breaks both the four-lines-per-group
// scroll arithmetic and the box height.
func TestEventsLensLongReasonDoesNotWrap(t *testing.T) {
	m := lensModel(eventsLensMinWidth, 20, func(m *Model) {
		m.events = map[types.UID]eventRow{"x": {
			UID: "x", Namespace: "default", Type: "Warning",
			Reason:  strings.Repeat("VeryLongCustomReason", 6),
			Message: strings.Repeat("m", 300),
			Count:   3, LastSeen: time.Now(),
			InvolvedKind: "Pod", InvolvedName: strings.Repeat("p", 80), InvolvedNs: "default",
		}}
	})

	const pane = eventsLensMinWidth - 2
	lines := m.eventGroupLines(groupEvents(m.events), pane)
	if len(lines) != 4 {
		t.Fatalf("one group produced %d lines, want exactly 4", len(lines))
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got > pane {
			t.Errorf("line %d is %d cells wide in a %d-cell pane; it will wrap", i, got, pane)
		}
	}

	// And end to end: the box still fits and keeps its border.
	out := m.renderEventsLens(eventsLensMinWidth, 12)
	if got := lipgloss.Height(out); got != 12 {
		t.Errorf("box height = %d, want 12", got)
	}
	rows := strings.Split(out, "\n")
	if !strings.Contains(rows[len(rows)-1], "└") {
		t.Error("long reason pushed the bottom border off the canvas")
	}
}

// The footer carries counts plus hints and overflows a narrow box just
// as readily as the reason does.
func TestEventsLensFooterDoesNotWrap(t *testing.T) {
	m := lensModel(eventsLensMinWidth, 20, func(m *Model) {
		now := time.Now()
		for i := 0; i < 40; i++ {
			uid := types.UID("f" + string(rune('a'+i%26)) + string(rune('A'+i/26)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Normal",
				Reason: "R" + string(rune('a'+i%26)), Message: "m", Count: 99,
				LastSeen: now, InvolvedKind: "Pod", InvolvedName: "p", InvolvedNs: "default",
			}
		}
		// Scoped, so the footer also carries the "E all events" hint.
		m.eventsLens.scope = &eventScopeRef{Kind: "Pod", Namespace: "default", Name: "p"}
	})

	out := m.renderEventsLens(eventsLensMinWidth, 14)
	if got := lipgloss.Height(out); got != 14 {
		t.Errorf("box height = %d, want 14 — a wrapped footer adds a row", got)
	}
	for i, line := range strings.Split(out, "\n") {
		if got := lipgloss.Width(line); got > eventsLensMinWidth {
			t.Errorf("row %d is %d cells wide, want <= %d", i, got, eventsLensMinWidth)
		}
	}
}

// Scroll limits are computed from the same chrome budget the renderer
// spends; if they drift, G either stops short or scrolls into blank.
func TestEventsLensScrollLimitMatchesRender(t *testing.T) {
	m := lensModel(120, 24, func(m *Model) {
		now := time.Now()
		for i := 0; i < 30; i++ {
			uid := types.UID("s" + string(rune('a'+i%26)) + string(rune('A'+i/26)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Normal",
				Reason: "Reason" + string(rune('a'+i%26)), Message: "m", Count: 1,
				LastSeen: now, InvolvedKind: "Pod", InvolvedName: "p", InvolvedNs: "default",
			}
		}
	})

	end, _ := m.handleEventsKey(key("G"))
	e := end.(Model)

	events, _ := e.scopedEvents()
	total := len(e.eventGroupLines(groupEvents(events), 118))
	canvas := e.height - lipgloss.Height(e.renderHeader()) - lipgloss.Height(e.renderFooter())
	want := total - (canvas - eventsLensChrome)
	if want < 0 {
		want = 0
	}
	if e.eventsLens.scroll != want {
		t.Errorf("G scrolled to %d, want %d (last full screen of %d lines)",
			e.eventsLens.scroll, want, total)
	}
}
