package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

type deploymentRow struct {
	UID       types.UID
	Namespace string
	Name      string
	Replicas  int32
	Ready     int32
	UpToDate  int32
	Available int32
	CreatedAt time.Time
	Updated   time.Time
}

func applyDeployEvent(m map[types.UID]deploymentRow, ev cluster.DeployEvent) {
	switch ev.Kind {
	case cluster.DeployDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = deploymentRow{
			UID:       ev.UID,
			Namespace: ev.Namespace,
			Name:      ev.Name,
			Replicas:  ev.Replicas,
			Ready:     ev.Ready,
			UpToDate:  ev.UpToDate,
			Available: ev.Available,
			CreatedAt: ev.CreatedAt,
			Updated:   time.Now(),
		}
	}
}

func sortedDeployRows(m map[types.UID]deploymentRow) []deploymentRow {
	out := make([]deploymentRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// renderDeployTable mirrors the pod / node tables.
func (m Model) renderDeployTable(maxRows, _ int) string {
	// Apply the same namespace + text filter the cursor logic in
	// visibleUIDs uses, otherwise the rendered table includes rows the
	// cursor can't reach and `n: <ns>` looks broken.
	all := sortedDeployRows(m.deployments)
	needle := strings.ToLower(m.filterText)
	rows := make([]deploymentRow, 0, len(all))
	for _, r := range all {
		if m.namespace != "" && r.Namespace != m.namespace {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(r.Name), needle) &&
			!strings.Contains(strings.ToLower(r.Namespace), needle) {
			continue
		}
		rows = append(rows, r)
	}

	const (
		colNs    = 18
		colName  = 36
		colReady = 8
		colUpd   = 10
		colAvail = 10
		colAge   = 5
	)

	hdr := m.Theme.Header
	header := " " +
		padCol("NAMESPACE", colNs, hdr) + "  " +
		padCol("DEPLOYMENT", colName, hdr) + "  " +
		padColRight("READY", colReady, hdr) + "  " +
		padColRight("UP-TO-DATE", colUpd, hdr) + "  " +
		padColRight("AVAILABLE", colAvail, hdr) + "  " +
		padColRight("AGE", colAge, hdr)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedDeploys, "deployments"))
		return b.String()
	}

	// Cursor-centred windowing — match the pod table at app.go:1300+.
	// Naive head-truncation hides any deployment past row maxRows-1
	// from the cursor, which made bottom rows unreachable on clusters
	// with more deployments than fit on screen.
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

	warnIdx := recentWarningIndex(m.events)
	for _, r := range rows {
		readyStr := fmt.Sprintf("%d/%d", r.Ready, r.Replicas)
		// Color the READY cell amber if not all replicas are ready.
		readyStyle := m.Theme.StatusOK
		if r.Ready < r.Replicas {
			readyStyle = m.Theme.StatusWrn
		}
		if r.Ready == 0 && r.Replicas > 0 {
			readyStyle = m.Theme.StatusBad
		}

		line := warnGlyph(warnIdx, "Deployment", r.Namespace, r.Name, m.Theme) +
			padCol(r.Namespace, colNs, m.Theme.Base) + "  " +
			padCol(r.Name, colName, m.Theme.Base) + "  " +
			padColRight(readyStr, colReady, readyStyle) + "  " +
			padColRight(fmt.Sprintf("%d", r.UpToDate), colUpd, m.Theme.Base) + "  " +
			padColRight(fmt.Sprintf("%d", r.Available), colAvail, m.Theme.Base) + "  " +
			padColRight(formatAge(r.CreatedAt), colAge, m.Theme.Base)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
