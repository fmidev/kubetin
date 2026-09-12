package ui

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type tableKey struct {
	filter, namespace string
	sort              int
	desc              bool
	size              int
	dependencies      [3]int
}

type tableOrder struct {
	key        tableKey
	valid      bool
	uids       []types.UID
	countKey   tableKey
	countValid bool
	count      int
}

// Bubble Tea calls Update and View serially. Only derived data is shared
// through this pointer so value-receiver renders can retain their caches.
type tableCache struct {
	dashboardPods dashboardPodOrder
	orders        [ViewFleet]tableOrder
	phasesValid   bool
	phaseSize     int
	phases        [3]int
	nsCounts      map[string]nsCount
	nsCountSizes  [3]int
}

func (m Model) invalidateTables(views ...View) {
	for _, view := range views {
		m.tables.orders[view] = tableOrder{}
	}
}

func (m Model) invalidatePodOrder(before, after podRow) {
	if before.UID != after.UID || before.Namespace != after.Namespace || before.Name != after.Name ||
		before.HasMetrics != after.HasMetrics || before.CPUMilli != after.CPUMilli || before.MemBytes != after.MemBytes {
		m.tables.dashboardPods = dashboardPodOrder{}
	}
	entry := &m.tables.orders[ViewPods]
	if before.UID != after.UID || before.Namespace != after.Namespace || before.Name != after.Name {
		*entry = tableOrder{}
		return
	}
	if entry.valid {
		key := SortKey(entry.key.sort)
		if lessBy(before, after, key) || lessBy(after, before, key) {
			entry.valid = false
			entry.uids = nil
		}
	}
}

func (m Model) tableKey(view View) tableKey {
	key := tableKey{filter: m.filterText, namespace: m.namespace}
	switch view {
	case ViewPods:
		key.size, key.sort, key.desc = len(m.pods), int(m.sortKey), m.sortDesc
	case ViewNodes:
		key.size, key.namespace = len(m.nodes), ""
	case ViewDeployments:
		key.size = len(m.deployments)
	case ViewNamespaces:
		key.size, key.namespace = len(m.namespaces), ""
		key.sort, key.desc = int(m.nsSortKey), m.nsSortDesc
		key.dependencies = [3]int{len(m.pods), len(m.deployments), len(m.events)}
	case ViewServices:
		key.size = len(m.services)
	case ViewIngresses:
		key.size = len(m.ingresses)
	}
	return key
}

func (m Model) tableUIDs(view View) []types.UID {
	entry := &m.tables.orders[view]
	key := m.tableKey(view)
	if !entry.valid || entry.key != key {
		entry.uids = m.buildTableUIDs(view)
		entry.key, entry.valid = key, true
		entry.countKey, entry.countValid, entry.count = key, true, len(entry.uids)
	}
	return entry.uids
}

// Counts in the header must not sort a hidden table when an overlay is open.
func (m Model) tableCount(view View) int {
	entry := &m.tables.orders[view]
	key := m.tableKey(view)
	if key.filter == "" && key.namespace == "" {
		return key.size
	}
	if !entry.countValid || entry.countKey != key {
		needle := strings.ToLower(m.filterText)
		names := func(ns, name string) bool { return matchesNames(ns, name, m.namespace, needle) }
		switch view {
		case ViewPods:
			entry.count = countMatching(m.pods, func(r podRow) bool { return names(r.Namespace, r.Name) })
		case ViewNodes:
			entry.count = countMatching(m.nodes, func(r nodeRow) bool { return matchesNames("", r.Name, "", needle) })
		case ViewDeployments:
			entry.count = countMatching(m.deployments, func(r deploymentRow) bool { return names(r.Namespace, r.Name) })
		case ViewNamespaces:
			entry.count = countMatching(m.namespaces, func(r nsRow) bool { return matchesNames("", r.Name, "", needle) })
		case ViewServices:
			entry.count = countMatching(m.services, func(r serviceRow) bool { return names(r.Namespace, r.Name) })
		case ViewIngresses:
			entry.count = countMatching(m.ingresses, func(r ingressRow) bool {
				return (m.namespace == "" || r.Namespace == m.namespace) && (needle == "" || ingressMatches(r, needle))
			})
		}
		entry.countKey, entry.countValid = key, true
	}
	return entry.count
}

func matchesNames(ns, name, namespace, needle string) bool {
	return (namespace == "" || ns == namespace) &&
		(needle == "" || strings.Contains(strings.ToLower(ns), needle) || strings.Contains(strings.ToLower(name), needle))
}

func countMatching[T any](rows map[types.UID]T, matches func(T) bool) int {
	n := 0
	for _, r := range rows {
		if matches(r) {
			n++
		}
	}
	return n
}

func rowsForUIDs[T any](rows map[types.UID]T, ids []types.UID) []T {
	out := make([]T, len(ids))
	for i, id := range ids {
		out[i] = rows[id]
	}
	return out
}

func (m Model) windowUIDs(view View, maxRows int) []types.UID {
	if maxRows == 1 {
		return nil
	}
	return windowRows(m.tableUIDs(view), m.cursor, maxRows, func(id types.UID) types.UID { return id })
}

func (m Model) podPhaseCounts() (int, int, int) {
	cache := m.tables
	if !cache.phasesValid || cache.phaseSize != len(m.pods) {
		cache.phases = [3]int{}
		for _, p := range m.pods {
			switch p.Phase {
			case corev1.PodRunning:
				cache.phases[0]++
			case corev1.PodPending:
				cache.phases[1]++
			case corev1.PodFailed:
				cache.phases[2]++
			}
		}
		cache.phasesValid, cache.phaseSize = true, len(m.pods)
	}
	return cache.phases[0], cache.phases[1], cache.phases[2]
}

func (m Model) namespaceCounts() map[string]nsCount {
	sizes := [3]int{len(m.pods), len(m.deployments), len(m.events)}
	if m.tables.nsCounts == nil || m.tables.nsCountSizes != sizes {
		m.tables.nsCounts = m.collectNsCounts()
		m.tables.nsCountSizes = sizes
	}
	return m.tables.nsCounts
}
