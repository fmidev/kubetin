package ui

import (
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

type ingressRow struct {
	UID       types.UID
	Namespace string
	Name      string
	Class     string
	Hosts     []string
	Backends  []cluster.IngressBackend
	Address   string
	TLSHosts  int
	CreatedAt time.Time
	Updated   time.Time
}

func applyIngressEvent(m map[types.UID]ingressRow, ev cluster.IngressEvent) {
	switch ev.Kind {
	case cluster.IngDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = ingressRow{
			UID:       ev.UID,
			Namespace: ev.Namespace,
			Name:      ev.Name,
			Class:     ev.Class,
			Hosts:     ev.Hosts,
			Backends:  ev.Backends,
			Address:   ev.Address,
			TLSHosts:  ev.TLSHosts,
			CreatedAt: ev.CreatedAt,
			Updated:   time.Now(),
		}
	}
}

func sortedIngressRows(m map[types.UID]ingressRow) []ingressRow {
	out := make([]ingressRow, 0, len(m))
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

// formatBackends renders "svc:port" per backend, collapsed to the first
// plus "+N". An Ingress fanning twenty paths onto one service already
// deduplicated upstream, so a "+N" here means genuinely distinct
// services.
func formatBackends(bs []cluster.IngressBackend) string {
	if len(bs) == 0 {
		return "—"
	}
	strs := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Port == "" {
			strs = append(strs, b.Service)
			continue
		}
		strs = append(strs, b.Service+":"+b.Port)
	}
	return collapseList(strs)
}

// ingressPorts is derived, not read off the object: an Ingress always
// serves 80, and 443 as well once any rule is covered by a TLS entry.
func ingressPorts(r ingressRow) string {
	if r.TLSHosts > 0 {
		return "80,443"
	}
	return "80"
}

// ingressColumns in display order. INGRESS never drops, then HOSTS and
// BACKENDS — what the ingress actually routes. ADDRESS sheds first,
// then TLS, then PORTS.
var ingressColumns = []column{
	{min: 12, max: 18, prio: 4}, // NAMESPACE
	{min: 16, max: 32, prio: 0}, // INGRESS
	{min: 7, max: 12, prio: 5},  // CLASS
	{min: 18, max: 40, prio: 1}, // HOSTS
	{min: 14, max: 32, prio: 2}, // BACKENDS
	{min: 12, max: 18, prio: 9}, // ADDRESS
	{min: 3, max: 3, prio: 8},   // TLS
	{min: 6, max: 8, prio: 7},   // PORTS
	{min: 4, max: 5, prio: 6},   // AGE
}

func (m Model) renderIngressTable(maxRows, maxWidth int) string {
	rows := m.visibleIngressRows()

	w := fitColumns(ingressColumns, maxWidth-1)
	hdr := m.Theme.Header
	header := " " + joinCells(
		padCol("NAMESPACE", w[0], hdr),
		padCol("INGRESS", w[1], hdr),
		padCol("CLASS", w[2], hdr),
		padCol("HOSTS", w[3], hdr),
		padCol("BACKENDS", w[4], hdr),
		padCol("ADDRESS", w[5], hdr),
		padCol("TLS", w[6], hdr),
		padCol("PORTS", w[7], hdr),
		padColRight("AGE", w[8], hdr),
	)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedIngresses, "ingresses"))
		return b.String()
	}
	rows = windowRows(rows, m.cursor, maxRows, func(r ingressRow) types.UID { return r.UID })

	warnIdx := recentWarningIndex(m.events)
	for _, r := range rows {
		class := r.Class
		if class == "" {
			class = "—"
		}

		// No address means the controller hasn't admitted it — the
		// usual reason an Ingress exists but answers nothing.
		address, addressStyle := r.Address, m.Theme.Base
		if address == "" {
			address, addressStyle = "<pending>", m.Theme.StatusWrn
		}

		tls, tlsStyle := "—", m.Theme.Dim
		if r.TLSHosts > 0 {
			tls, tlsStyle = "✓", m.Theme.StatusOK
		}

		line := warnGlyph(warnIdx, "Ingress", r.Namespace, r.Name, m.Theme) + joinCells(
			padCol(r.Namespace, w[0], m.Theme.Base),
			padCol(r.Name, w[1], m.Theme.Base),
			padCol(class, w[2], m.Theme.Base),
			padCol(collapseList(r.Hosts), w[3], m.Theme.Base),
			padCol(formatBackends(r.Backends), w[4], m.Theme.Base),
			padCol(address, w[5], addressStyle),
			padCol(tls, w[6], tlsStyle),
			padCol(ingressPorts(r), w[7], m.Theme.Dim),
			padColRight(formatAge(r.CreatedAt), w[8], m.Theme.Base),
		)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// visibleIngressRows applies the same namespace + text filter
// visibleUIDs uses. Hosts are searchable too: "which ingress serves
// pay.example.com" is the question this view gets asked.
func (m Model) visibleIngressRows() []ingressRow {
	needle := strings.ToLower(m.filterText)
	all := sortedIngressRows(m.ingresses)
	out := make([]ingressRow, 0, len(all))
	for _, r := range all {
		if m.namespace != "" && r.Namespace != m.namespace {
			continue
		}
		if needle != "" && !ingressMatches(r, needle) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func ingressMatches(r ingressRow, needle string) bool {
	if strings.Contains(strings.ToLower(r.Name), needle) ||
		strings.Contains(strings.ToLower(r.Namespace), needle) {
		return true
	}
	for _, h := range r.Hosts {
		if strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}
