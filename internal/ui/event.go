package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// eventScopeRef narrows ViewEvents to events whose involvedObject
// matches the given Kind/Namespace/Name. Set by the action menu's
// "Events" item so a user can drill from a row into the events for
// that specific resource. Compared by exact match on all three
// fields — substring matching is what the text filter is for.
type eventScopeRef struct {
	Kind      string
	Namespace string
	Name      string
}

// matches returns true if the event row's involvedObject equals the
// scope on all three fields.
func (s eventScopeRef) matches(r eventRow) bool {
	return r.InvolvedKind == s.Kind &&
		r.InvolvedName == s.Name &&
		r.InvolvedNs == s.Namespace
}

// eventRow is a single Event observation. We aggregate at render time
// rather than at apply time, because the informer surfaces UPDATEs to
// individual events as their Count increments.
type eventRow struct {
	UID          types.UID
	Namespace    string
	Reason       string
	Message      string
	Type         string
	Count        int32
	FirstSeen    time.Time
	LastSeen     time.Time
	InvolvedKind string
	InvolvedName string
	InvolvedNs   string
}

// warnRecency is how recent a Warning event must be to mark its
// involved resource as "currently warning". Long enough to surface
// transient flaps (probe failures, image pulls) but short enough that
// fixed problems clear themselves from the row marker.
const warnRecency = 10 * time.Minute

// warnKey is the index key used by recentWarningIndex.
func warnKey(kind, ns, name string) string {
	return kind + "/" + ns + "/" + name
}

// recentWarningIndex returns the set of (kind/ns/name) tuples that
// have at least one Warning event with LastSeen within warnRecency.
// Built once per render so per-row lookups are O(1).
func recentWarningIndex(events map[types.UID]eventRow) map[string]struct{} {
	idx := make(map[string]struct{})
	cutoff := time.Now().Add(-warnRecency)
	for _, e := range events {
		if e.Type != "Warning" {
			continue
		}
		if e.LastSeen.Before(cutoff) {
			continue
		}
		idx[warnKey(e.InvolvedKind, e.InvolvedNs, e.InvolvedName)] = struct{}{}
	}
	return idx
}

func applyEvtEvent(m map[types.UID]eventRow, ev cluster.EventEvent) {
	switch ev.Kind {
	case cluster.EvtDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = eventRow{
			UID:          ev.UID,
			Namespace:    ev.Namespace,
			Reason:       ev.Reason,
			Message:      ev.Message,
			Type:         ev.Type,
			Count:        ev.Count,
			FirstSeen:    ev.FirstSeen,
			LastSeen:     ev.LastSeen,
			InvolvedKind: ev.InvolvedKind,
			InvolvedName: ev.InvolvedName,
			InvolvedNs:   ev.InvolvedNs,
		}
	}
}

// eventGroup is one (Reason + Message) bucket, summed across all
// individual Event objects with that pair.
type eventGroup struct {
	Reason       string
	Message      string
	Type         string
	Count        int32
	LastSeen     time.Time
	InvolvedKind string
	InvolvedName string
	InvolvedNs   string
}

func groupEvents(m map[types.UID]eventRow) []eventGroup {
	byKey := make(map[string]*eventGroup)
	for _, r := range m {
		key := r.Reason + "\x1f" + r.Message
		g, ok := byKey[key]
		if !ok {
			g = &eventGroup{
				Reason:       r.Reason,
				Message:      r.Message,
				Type:         r.Type,
				LastSeen:     r.LastSeen,
				InvolvedKind: r.InvolvedKind,
				InvolvedName: r.InvolvedName,
				InvolvedNs:   r.InvolvedNs,
			}
			byKey[key] = g
		}
		g.Count += r.Count
		if r.LastSeen.After(g.LastSeen) {
			g.LastSeen = r.LastSeen
			// Use the most recently observed source object as the
			// "representative" — likely what the user wants to look at.
			g.InvolvedKind = r.InvolvedKind
			g.InvolvedName = r.InvolvedName
			g.InvolvedNs = r.InvolvedNs
		}
		// Warning beats Normal at the group level.
		if r.Type == "Warning" {
			g.Type = "Warning"
		}
	}

	out := make([]eventGroup, 0, len(byKey))
	for _, g := range byKey {
		out = append(out, *g)
	}
	// Sort: most recent first, alphabetical Reason as tie-breaker.
	//
	// The prior sort used (Type=Warning desc, Count desc, LastSeen
	// desc) and called sort.Slice. Two problems with that:
	//
	//  - Count is a moving target. Every time the event watcher
	//    surfaces a fresh occurrence, the group's Count bumps and
	//    the row can jump several positions. From the user's seat
	//    that read as "order changes for no apparent reason."
	//
	//  - sort.Slice is not stable. With three mutable keys and lots
	//    of ties (events from one rollout hitting the same second),
	//    two consecutive renders could produce different orderings
	//    of the same set.
	//
	// Sorting by LastSeen-only matches the log-viewer mental model
	// (newest at top, older drifts down). Severity is still visible
	// from the coloured ● dot at the start of every card, so taking
	// Type out of the sort key doesn't hide anything — it just stops
	// it from competing with the time dimension. SliceStable + a
	// Reason tie-breaker gives a fully deterministic order even when
	// timestamps coincide.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// renderEventsView renders the aggregated event list. Each group is
// rendered as a 2-3 line "card":
//
//	● Reason                                          ×count
//	  Message (one or two lines, truncated)
//	  Pod/foo · namespace · 14:32:01
//
// eventsLensState is the events overlay.
//
// Events are never a place you navigate to — they are always *about*
// something. So this works the way the log viewer does: pop it open
// over whatever you were looking at, read, Esc straight back to the
// same row. It used to be a view on the number row, which meant a
// round trip and a cursor reset to read three lines.
type eventsLensState struct {
	open bool
	// scope nil means "no single object" — the namespace picker then
	// decides between one namespace and the whole cluster.
	scope  *eventScopeRef
	scroll int
}

// scopedEvents returns the events to show plus a label naming that
// scope. Three levels, all derived from state that already exists:
//
//	object     `e` on a row
//	namespace  no object scope, namespace picker set
//	cluster    no object scope, ns: all
func (m Model) scopedEvents() (map[types.UID]eventRow, string) {
	if s := m.eventsLens.scope; s != nil {
		out := make(map[types.UID]eventRow, 16)
		for uid, r := range m.events {
			if s.matches(r) {
				out[uid] = r
			}
		}
		label := s.Kind + "/" + s.Name
		if s.Namespace != "" {
			label += " · " + s.Namespace
		}
		return out, label
	}
	if m.namespace != "" {
		// The namespace picker applies here like it does to every
		// other namespaced view. It did not before: the events view
		// ignored m.namespace outright, so `n: kube-system` showed the
		// whole cluster's events while the header said otherwise.
		out := make(map[types.UID]eventRow, 64)
		for uid, r := range m.events {
			if r.InvolvedNs == m.namespace {
				out[uid] = r
			}
		}
		return out, "namespace " + m.namespace
	}
	return m.events, "all namespaces"
}

// eventGroupLines renders each group as its three-line block plus a
// blank separator, returned as lines so the caller can window them.
func (m Model) eventGroupLines(groups []eventGroup, width int) []string {
	lines := make([]string, 0, len(groups)*4)
	for _, g := range groups {
		dotStyle := m.Theme.StatusOK
		if g.Type == "Warning" {
			dotStyle = m.Theme.StatusWrn
		}

		reason := m.Theme.Header.Render(g.Reason)
		countBadge := m.Theme.Dim.Render(fmt.Sprintf("×%d", g.Count))
		left := " " + dotStyle.Render("●") + " " + reason
		// -1 keeps the badge off the right border.
		pad := width - lipgloss.Width(left) - lipgloss.Width(countBadge) - 1
		if pad < 1 {
			pad = 1
		}
		lines = append(lines, left+strings.Repeat(" ", pad)+countBadge)

		lines = append(lines, m.Theme.Base.Render("   "+truncate(oneLine(g.Message), width-4)))

		involved := ""
		if g.InvolvedKind != "" {
			involved = g.InvolvedKind + "/" + g.InvolvedName
		}
		meta := strings.TrimSpace(strings.Join(
			[]string{involved, g.InvolvedNs, formatAge(g.LastSeen) + " ago"}, " · "))
		lines = append(lines, m.Theme.Dim.Render("   "+truncate(meta, width-4)))
		lines = append(lines, "")
	}
	return lines
}

// renderEventsLens draws the overlay, shaped like the log viewer: a
// bordered box over the full body region, scrollable, Esc to close.
func (m Model) renderEventsLens(canvasWidth, canvasHeight int) string {
	w, h := canvasWidth, canvasHeight
	if w < eventsLensMinWidth {
		w = eventsLensMinWidth
	}
	if h < 6 {
		h = 6
	}
	// Width(w-2) plus the two border columns renders at w, so the
	// content area is exactly w-2 — the separator has to span that or
	// it stops short of the right border.
	innerW := w - 2
	if innerW < 1 {
		innerW = 1
	}

	events, scopeLabel := m.scopedEvents()
	groups := groupEvents(events)

	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" events ") +
		m.Theme.Dim.Render(truncate(scopeLabel, innerW-9)) + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", innerW)) + "\n")

	bodyHeight := h - 4 // title, separator, footer, border slack
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	lines := m.eventGroupLines(groups, innerW)
	if len(lines) == 0 {
		lines = []string{m.emptyEventsLine()}
	}
	body, _ := scrollWindow(strings.Join(lines, "\n"), m.eventsLens.scroll, bodyHeight)
	b.WriteString(body)
	for i := lipgloss.Height(body); i < bodyHeight; i++ {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	hint := " · j/k scroll · Esc close"
	if m.eventsLens.scope != nil {
		// Widening is only meaningful when something is narrowing it.
		hint = " · j/k scroll · E all events · Esc close"
	}
	b.WriteString(m.Theme.Footer.Render(fmt.Sprintf(
		" %s · %s%s",
		plural(len(groups), "group"), plural(int(totalEventCount(groups)), "event"), hint)))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Width(w - 2).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

// emptyEventsLine distinguishes "this object has none" from "the
// cluster has none" — the difference matters when you pressed `e`
// expecting an explanation and got nothing.
func (m Model) emptyEventsLine() string {
	if s := m.eventsLens.scope; s != nil {
		return m.Theme.Dim.Render("  no events for this " + strings.ToLower(s.Kind) + " yet")
	}
	if m.namespace != "" {
		return m.Theme.Dim.Render("  no events in namespace " + m.namespace)
	}
	return m.emptyPlaceholder(m.syncedEvents, "events")
}

const eventsLensMinWidth = 50

func totalEventCount(groups []eventGroup) int32 {
	var n int32
	for _, g := range groups {
		n += g.Count
	}
	return n
}

// openEventsForCursor opens the lens scoped to the highlighted row.
// Falls back to the unscoped lens when there's no selection, rather
// than doing nothing and looking broken.
func (m Model) openEventsForCursor() (tea.Model, tea.Cmd) {
	ref, ok := m.refForCursor()
	if !ok {
		return m.openEventsAll()
	}
	return m.openEventsFor(ref)
}

// openEventsFor scopes the lens to one object.
func (m Model) openEventsFor(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	m.eventsLens = eventsLensState{
		open: true,
		scope: &eventScopeRef{
			Kind:      ref.Kind,
			Namespace: ref.Namespace,
			Name:      ref.Name,
		},
	}
	return m, nil
}

// openEventsAll drops the object scope. What's left is the namespace
// picker's doing: `n: foo` narrows to that namespace, `ns: all` shows
// the cluster.
func (m Model) openEventsAll() (tea.Model, tea.Cmd) {
	m.eventsLens = eventsLensState{open: true}
	return m, nil
}

func (m Model) handleEventsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "esc", "q", "e":
		// `e` closes as well as opens: pressing it twice is the
		// fastest way to glance at events and get back.
		m.eventsLens = eventsLensState{}
	case "E":
		// Widen to everything without leaving the lens.
		m.eventsLens.scope = nil
		m.eventsLens.scroll = 0
	case "j", "down":
		m.eventsLens.scroll = m.clampEventsScroll(m.eventsLens.scroll + 1)
	case "k", "up":
		if m.eventsLens.scroll > 0 {
			m.eventsLens.scroll--
		}
	case "g", "home":
		m.eventsLens.scroll = 0
	case "G", "end":
		m.eventsLens.scroll = m.clampEventsScroll(1 << 30)
	}
	return m, nil
}

// clampEventsScroll bounds the offset against the rendered line count,
// so j past the end saturates instead of running away and needing as
// many k presses to come back.
func (m Model) clampEventsScroll(want int) int {
	if want < 0 {
		return 0
	}
	events, _ := m.scopedEvents()
	lines := len(m.eventGroupLines(groupEvents(events), 80))
	// Mirrors renderEventsLens: title, separator, footer, border slack.
	body := m.height - lipgloss.Height(m.renderHeader()) - lipgloss.Height(m.renderFooter()) - 4
	if body < 1 {
		body = 1
	}
	if max := lines - body; want > max {
		if max < 0 {
			return 0
		}
		return max
	}
	return want
}

// plural renders "1 group" / "2 groups" — the counts sit in a footer
// the user reads, not a log line.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
