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
		// Three resource-count sub-columns, headed PODS / DEP / WRN.
		// PODS gets one extra cell because pod counts hit four digits
		// on big clusters; DEP and WRN stay at 3 because anything
		// over 999 deployments in one namespace is its own problem.
		colPods   = 4
		colDeps   = 3
		colWarn   = 3
		colLabels = 60
	)

	// Single-pass count of pods / deploys / warning events per
	// namespace. Avoids O(N · (P+D+E)) inside the row loop.
	counts := m.collectNsCounts()

	hdr := m.Theme.Header
	header := " " +
		padCol("NAME", colName, hdr) + "  " +
		padCol("STATUS", colStatus, hdr) + "  " +
		padColRight("AGE", colAge, hdr) + "  " +
		padColRight("PODS", colPods, hdr) + "  " +
		padColRight("DEP", colDeps, hdr) + "  " +
		padColRight("WRN", colWarn, hdr) + "  " +
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

		c := counts[r.Name]
		// WRN cell is the whole point of this column — colour-code it
		// so a problem namespace pops on scan. Dim when zero so the
		// eye is drawn only to non-empty cells; red when populated.
		warnStyle := m.Theme.StatusDim
		if c.warnings > 0 {
			warnStyle = m.Theme.StatusBad
		}

		// One leading space replaces the warnGlyph column that the
		// pod / deploy / node tables use — namespaces don't have a
		// useful "has-recent-warning" signal of their own.
		line := " " +
			padCol(r.Name, colName, m.Theme.Base) + "  " +
			padCol(string(r.Phase), colStatus, statusStyle) + "  " +
			padColRight(formatAge(r.CreatedAt), colAge, m.Theme.Base) + "  " +
			padColRight(itoa(c.pods), colPods, m.Theme.Base) + "  " +
			padColRight(itoa(c.deploys), colDeps, m.Theme.Base) + "  " +
			padColRight(itoa(c.warnings), colWarn, warnStyle) + "  " +
			padCol(labelSummary(r.Labels, colLabels), colLabels, m.Theme.Dim)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// nsCount holds the per-namespace counts for one render pass.
// Built by collectNsCounts in a single walk over the in-memory
// caches; consumed by renderNamespacesView one row at a time.
type nsCount struct {
	pods     int
	deploys  int
	warnings int
}

// collectNsCounts walks pods / deployments / events once and groups
// counts by namespace name. O(P+D+E), not O(N·(P+D+E)) — important
// for big clusters where naive per-row counting would scan thousands
// of pods for each of dozens of namespaces.
//
// WRN counts ONLY Type=="Warning" events. Normal-class events are
// noise and would dominate the cell; the whole point of the column
// is to surface "where the problems are."
//
// Counts reflect only what kubetin has cached on the focused
// cluster. The watchers are usually synced within a couple of
// seconds; before then counts will show 0, which is the same
// trade-off every other view already makes.
func (m Model) collectNsCounts() map[string]nsCount {
	out := make(map[string]nsCount)
	for _, p := range m.pods {
		c := out[p.Namespace]
		c.pods++
		out[p.Namespace] = c
	}
	for _, d := range m.deployments {
		c := out[d.Namespace]
		c.deploys++
		out[d.Namespace] = c
	}
	for _, e := range m.events {
		if e.Type != "Warning" {
			continue
		}
		c := out[e.Namespace]
		c.warnings++
		out[e.Namespace] = c
	}
	return out
}
