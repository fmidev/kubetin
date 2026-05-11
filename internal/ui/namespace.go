package ui

import (
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// nsRow is the UI projection of a Namespace. The watcher copies
// labels in defensively so we can sort/render without poking the
// shared informer cache.
type nsRow struct {
	UID       types.UID
	Name      string
	Phase     corev1.NamespacePhase
	CreatedAt time.Time
	Labels    map[string]string
}

func applyNsEvent(m map[types.UID]nsRow, ev cluster.NamespaceEvent) {
	switch ev.Kind {
	case cluster.NsDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = nsRow{
			UID:       ev.UID,
			Name:      ev.Name,
			Phase:     ev.Phase,
			CreatedAt: ev.CreatedAt,
			Labels:    ev.Labels,
		}
	}
}

func sortedNsRows(m map[types.UID]nsRow) []nsRow {
	out := make([]nsRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// labelSummary renders a one-line approximation of a namespace's
// labels, capped to a column width. Format: `k=v · k=v · k=v` with
// the rest replaced by an "…+N" suffix when truncated.
//
// We sort keys for stable output (map iteration order would shuffle
// the column across renders, the same kind of UX glitch the events
// view had).
func labelSummary(labels map[string]string, width int) string {
	if len(labels) == 0 || width <= 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	joined := strings.Join(parts, " · ")
	if len(joined) <= width {
		return joined
	}
	// Truncate, leaving room for the "…+N" suffix that says how many
	// labels weren't shown.
	for shown := len(parts) - 1; shown >= 1; shown-- {
		cand := strings.Join(parts[:shown], " · ")
		suffix := " …+" + itoa(len(parts)-shown)
		if len(cand)+len(suffix) <= width {
			return cand + suffix
		}
	}
	// Even one label is too long — truncate the single string.
	return truncate(joined, width)
}

// itoa is a tiny local helper so labelSummary doesn't drag in
// strconv just to format a small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// renderNamespacesView is the Namespace table — mirrors the
// deployment table layout, with STATUS color-coded:
//
//	Active       → green
//	Terminating  → yellow (red when stuck for > ~10 min would be a
//	                       v2 polish; v1 keeps it simple)
func (m Model) renderNamespacesView(maxRows, _ int) string {
	all := sortedNsRows(m.namespaces)
	needle := strings.ToLower(m.filterText)
	rows := make([]nsRow, 0, len(all))
	for _, r := range all {
		if needle != "" && !strings.Contains(strings.ToLower(r.Name), needle) {
			continue
		}
		rows = append(rows, r)
	}

	const (
		colName   = 36
		colStatus = 14
		colAge    = 5
		colLabels = 60
	)

	hdr := m.Theme.Header
	header := " " +
		padCol("NAME", colName, hdr) + "  " +
		padCol("STATUS", colStatus, hdr) + "  " +
		padColRight("AGE", colAge, hdr) + "  " +
		padCol("LABELS", colLabels, hdr)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedNamespaces, "namespaces"))
		return b.String()
	}

	// Cursor-centred windowing, same shape as the deployment table.
	if maxRows > 0 && len(rows) > maxRows-1 {
		idx := -1
		for i, r := range rows {
			if r.UID == m.cursor {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = 0
		}
		half := (maxRows - 1) / 2
		start := idx - half
		if start < 0 {
			start = 0
		}
		end := start + (maxRows - 1)
		if end > len(rows) {
			end = len(rows)
			start = end - (maxRows - 1)
			if start < 0 {
				start = 0
			}
		}
		rows = rows[start:end]
	}

	for _, r := range rows {
		statusStyle := m.Theme.StatusOK
		if r.Phase == corev1.NamespaceTerminating {
			statusStyle = m.Theme.StatusWrn
		}

		// One leading space replaces the warnGlyph column that the
		// pod / deploy / node tables use — namespaces don't have a
		// useful "has-recent-warning" signal of their own.
		line := " " +
			padCol(r.Name, colName, m.Theme.Base) + "  " +
			padCol(string(r.Phase), colStatus, statusStyle) + "  " +
			padColRight(formatAge(r.CreatedAt), colAge, m.Theme.Base) + "  " +
			padCol(labelSummary(r.Labels, colLabels), colLabels, m.Theme.Dim)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
