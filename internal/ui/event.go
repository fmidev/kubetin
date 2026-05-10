package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

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
	// Sort: severity desc (Warning first), then count desc, then
	// lastSeen desc.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "Warning") != (out[j].Type == "Warning") {
			return out[i].Type == "Warning"
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// renderEventsView renders the aggregated event list. Each group is
// rendered as a 2-3 line "card":
//
//	● Reason                                          ×count
//	  Message (one or two lines, truncated)
//	  Pod/foo · namespace · 14:32:01
func (m Model) renderEventsView(maxRows, maxWidth int) string {
	groups := groupEvents(m.events)

	if maxWidth < 40 {
		maxWidth = 40
	}

	var b strings.Builder
	header := m.Theme.Header.Render(fmt.Sprintf(" EVENTS (%d groups, %d total events)",
		len(groups), totalEventCount(groups)))
	b.WriteString(header)
	b.WriteString("\n\n")
	rendered := 2

	if len(groups) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedEvents, "events"))
		return b.String()
	}

	for _, g := range groups {
		if rendered >= maxRows {
			break
		}
		dotStyle := m.Theme.StatusOK
		if g.Type == "Warning" {
			dotStyle = m.Theme.StatusWrn
		}
		dot := dotStyle.Render("●")

		// Line 1: dot + reason (bold), count badge right-aligned.
		reason := m.Theme.Header.Render(g.Reason)
		countBadge := m.Theme.Dim.Render(fmt.Sprintf("×%d", g.Count))
		left := " " + dot + " " + reason
		pad := maxWidth - lipgloss.Width(left) - lipgloss.Width(countBadge) - 1
		if pad < 1 {
			pad = 1
		}
		b.WriteString(left + strings.Repeat(" ", pad) + countBadge)
		b.WriteByte('\n')
		rendered++
		if rendered >= maxRows {
			break
		}

		// Line 2: message (truncated to width).
		msg := strings.ReplaceAll(g.Message, "\n", " ")
		msgLine := "   " + truncate(msg, maxWidth-4)
		b.WriteString(m.Theme.Base.Render(msgLine))
		b.WriteByte('\n')
		rendered++
		if rendered >= maxRows {
			break
		}

		// Line 3: involved object · namespace · last seen.
		involved := ""
		if g.InvolvedKind != "" {
			involved = g.InvolvedKind + "/" + g.InvolvedName
		}
		ns := g.InvolvedNs
		seen := formatAge(g.LastSeen)
		meta := strings.TrimSpace(strings.Join([]string{involved, ns, seen + " ago"}, " · "))
		b.WriteString(m.Theme.Dim.Render("   " + truncate(meta, maxWidth-4)))
		b.WriteString("\n\n")
		rendered += 2
	}
	return b.String()
}

func totalEventCount(groups []eventGroup) int32 {
	var n int32
	for _, g := range groups {
		n += g.Count
	}
	return n
}
