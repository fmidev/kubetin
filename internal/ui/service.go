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

type serviceRow struct {
	UID          types.UID
	Namespace    string
	Name         string
	Type         string
	ClusterIP    string
	ExternalIPs  []string
	ExternalName string
	Ports        []cluster.ServicePort
	Selector     map[string]string
	CreatedAt    time.Time
	Updated      time.Time
}

// endpointSliceRow is one slice's contribution to a Service's endpoint
// count. Stored per slice rather than summed into serviceRow: a Service
// owns several slices (the controller shards at 100 endpoints, and
// dual-stack clusters get one per family) and they arrive on their own
// clock. Summing at render time keeps the informer that owns the
// Service from clobbering a count the slice informer owns — the same
// field-ownership rule model.Store.ApplyProbe / ApplyMetrics follow.
type endpointSliceRow struct {
	Namespace   string
	ServiceName string
	Ready       int
	Total       int
}

// endpointCount is the aggregate for one Service.
type endpointCount struct {
	Ready int
	Total int
}

func applyServiceEvent(m map[types.UID]serviceRow, ev cluster.ServiceEvent) {
	switch ev.Kind {
	case cluster.SvcDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = serviceRow{
			UID:          ev.UID,
			Namespace:    ev.Namespace,
			Name:         ev.Name,
			Type:         ev.Type,
			ClusterIP:    ev.ClusterIP,
			ExternalIPs:  ev.ExternalIPs,
			ExternalName: ev.ExternalName,
			Ports:        ev.Ports,
			Selector:     ev.Selector,
			CreatedAt:    ev.CreatedAt,
			Updated:      time.Now(),
		}
	}
}

func applyEndpointSliceEvent(m map[types.UID]endpointSliceRow, ev cluster.EndpointSliceEvent) {
	switch ev.Kind {
	case cluster.EndpointSliceDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = endpointSliceRow{
			Namespace:   ev.Namespace,
			ServiceName: ev.ServiceName,
			Ready:       ev.Ready,
			Total:       ev.Total,
		}
	}
}

// endpointKey identifies a Service across the slice map.
func endpointKey(namespace, service string) string { return namespace + "/" + service }

// collectEndpointCounts sums every slice per Service. Built once per
// render, mirroring collectNsCounts.
func collectEndpointCounts(slices map[types.UID]endpointSliceRow) map[string]endpointCount {
	out := make(map[string]endpointCount, len(slices))
	for _, s := range slices {
		k := endpointKey(s.Namespace, s.ServiceName)
		c := out[k]
		c.Ready += s.Ready
		c.Total += s.Total
		out[k] = c
	}
	return out
}

func sortedServiceRows(m map[types.UID]serviceRow) []serviceRow {
	out := make([]serviceRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// formatServicePorts renders spec.ports the way kubectl does:
// "80/TCP", or "80:30821/TCP" once a node port is allocated.
func formatServicePorts(ports []cluster.ServicePort) string {
	if len(ports) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "TCP"
		}
		if p.NodePort > 0 {
			parts = append(parts, fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, proto))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d/%s", p.Port, proto))
	}
	return strings.Join(parts, ",")
}

// serviceExternal renders the external column. The three states read
// differently on purpose: ExternalName services point elsewhere
// entirely, a LoadBalancer with no address yet is *pending* (the
// interesting failure), and everything else simply has none.
func serviceExternal(r serviceRow) (string, bool) {
	if r.Type == "ExternalName" {
		if r.ExternalName != "" {
			return r.ExternalName, false
		}
		return "—", false
	}
	if len(r.ExternalIPs) > 0 {
		return collapseList(r.ExternalIPs), false
	}
	if r.Type == "LoadBalancer" {
		return "<pending>", true
	}
	return "—", false
}

// collapseList renders the first element plus a "+N" tail so a
// multi-valued cell stays one line.
func collapseList(vals []string) string {
	switch len(vals) {
	case 0:
		return "—"
	case 1:
		return vals[0]
	}
	return fmt.Sprintf("%s,+%d", vals[0], len(vals)-1)
}

// serviceColumns in display order. SERVICE never drops; READY drops
// late because it is the health signal the view exists for. The IP
// columns go first — they're the least actionable.
var serviceColumns = []column{
	{min: 12, max: 18, prio: 3}, // NAMESPACE
	{min: 18, max: 40, prio: 0}, // SERVICE
	{min: 12, max: 12, prio: 4}, // TYPE
	{min: 12, max: 15, prio: 7}, // CLUSTER-IP
	{min: 12, max: 22, prio: 6}, // EXTERNAL-IP
	{min: 12, max: 26, prio: 2}, // PORTS
	{min: 6, max: 7, prio: 1},   // READY
	{min: 4, max: 5, prio: 5},   // AGE
}

func (m Model) renderServiceTable(maxRows, maxWidth int) string {
	rows := m.visibleServiceRows()
	counts := collectEndpointCounts(m.endpointSlices)

	w := fitColumns(serviceColumns, maxWidth-1)
	hdr := m.Theme.Header
	header := " " + joinCells(
		padCol("NAMESPACE", w[0], hdr),
		padCol("SERVICE", w[1], hdr),
		padCol("TYPE", w[2], hdr),
		padCol("CLUSTER-IP", w[3], hdr),
		padCol("EXTERNAL-IP", w[4], hdr),
		padCol("PORTS", w[5], hdr),
		padColRight("READY", w[6], hdr),
		padColRight("AGE", w[7], hdr),
	)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedServices, "services"))
		return b.String()
	}
	rows = windowRows(rows, m.cursor, maxRows, func(r serviceRow) types.UID { return r.UID })

	warnIdx := recentWarningIndex(m.events)
	for _, r := range rows {
		external, pending := serviceExternal(r)
		externalStyle := m.Theme.Base
		if pending {
			externalStyle = m.Theme.StatusWrn
		}

		clusterIP := r.ClusterIP
		if clusterIP == "" || clusterIP == "None" {
			// Headless services genuinely have no cluster IP; say so
			// rather than printing an empty cell.
			clusterIP = "None"
		}

		readyStr, readyStyle := m.serviceReadyCell(r, counts)

		line := warnGlyph(warnIdx, "Service", r.Namespace, r.Name, m.Theme) + joinCells(
			padCol(r.Namespace, w[0], m.Theme.Base),
			padCol(r.Name, w[1], m.Theme.Base),
			padCol(r.Type, w[2], m.Theme.Base),
			padCol(clusterIP, w[3], m.Theme.Dim),
			padCol(external, w[4], externalStyle),
			padCol(formatServicePorts(r.Ports), w[5], m.Theme.Base),
			padColRight(readyStr, w[6], readyStyle),
			padColRight(formatAge(r.CreatedAt), w[7], m.Theme.Base),
		)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// serviceReadyCell renders the endpoint count and picks its colour.
//
// A selector-less Service (ExternalName, or one with hand-managed
// endpoints) has nothing to count, so it shows "—" rather than a
// zero that would read as broken.
func (m Model) serviceReadyCell(r serviceRow, counts map[string]endpointCount) (string, lipgloss.Style) {
	if r.Type == "ExternalName" || len(r.Selector) == 0 {
		return "—", m.Theme.Dim
	}
	c, ok := counts[endpointKey(r.Namespace, r.Name)]
	if !ok {
		// No slice seen yet: either still syncing or the watcher was
		// denied. Either way we don't know, and don't claim zero.
		return "—", m.Theme.Dim
	}
	s := fmt.Sprintf("%d/%d", c.Ready, c.Total)
	switch {
	case c.Total == 0 || c.Ready == 0:
		// The whole reason this column exists: a Service selecting
		// nothing looks perfectly healthy from its own spec.
		return s, m.Theme.StatusBad
	case c.Ready < c.Total:
		return s, m.Theme.StatusWrn
	}
	return s, m.Theme.StatusOK
}

// visibleServiceRows applies the same namespace + text filter
// visibleUIDs uses, so the rendered table and the cursor agree on
// which rows exist.
func (m Model) visibleServiceRows() []serviceRow {
	needle := strings.ToLower(m.filterText)
	all := sortedServiceRows(m.services)
	out := make([]serviceRow, 0, len(all))
	for _, r := range all {
		if m.namespace != "" && r.Namespace != m.namespace {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(r.Name), needle) &&
			!strings.Contains(strings.ToLower(r.Namespace), needle) {
			continue
		}
		out = append(out, r)
	}
	return out
}
