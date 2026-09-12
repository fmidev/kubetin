package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

	// Dashboard detail. Not read by the table renderer.
	Unavailable    int32
	StrategyType   string
	MaxSurge       string
	MaxUnavailable string
	Selector       *metav1.LabelSelector
	Conditions     []cluster.DeployCondition
}

func applyDeployEvent(m map[types.UID]deploymentRow, ev cluster.DeployEvent) {
	switch ev.Kind {
	case cluster.DeployDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = deploymentRow{
			UID:            ev.UID,
			Namespace:      ev.Namespace,
			Name:           ev.Name,
			Replicas:       ev.Replicas,
			Ready:          ev.Ready,
			UpToDate:       ev.UpToDate,
			Available:      ev.Available,
			CreatedAt:      ev.CreatedAt,
			Updated:        time.Now(),
			Unavailable:    ev.Unavailable,
			StrategyType:   ev.StrategyType,
			MaxSurge:       ev.MaxSurge,
			MaxUnavailable: ev.MaxUnavailable,
			Selector:       ev.Selector,
			Conditions:     ev.Conditions,
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

// deploymentPods groups pods by the deployment's selector, as log lookup
// does in kubectl. It does not establish the ReplicaSet ownership chain.
func (m Model) deploymentPods(d deploymentRow) []podRow {
	if d.Selector == nil {
		return nil
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Selector)
	if err != nil || sel.Empty() {
		return nil
	}
	// Sort identities before copying rows, avoiding repeated growth of a large row slice.
	ids := make([]types.UID, 0, 8)
	for uid, p := range m.pods {
		if p.Namespace == d.Namespace && sel.Matches(labels.Set(p.Labels)) {
			ids = append(ids, uid)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := m.pods[ids[i]], m.pods[ids[j]]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return ids[i] < ids[j]
	})
	out := make([]podRow, len(ids))
	for i, uid := range ids {
		out[i] = m.pods[uid]
	}

	return out
}

// deployColumns in display order; UP-TO-DATE and AVAILABLE drop
// first (READY already carries the health signal), DEPLOYMENT never
// drops and absorbs spare width.
var deployColumns = []column{
	{min: 12, max: 18, prio: 2}, // NAMESPACE
	{min: 20, max: 48, prio: 0}, // DEPLOYMENT
	{min: 8, max: 8, prio: 1},   // READY
	{min: 10, max: 10, prio: 5}, // UP-TO-DATE
	{min: 10, max: 10, prio: 4}, // AVAILABLE
	{min: 5, max: 5, prio: 3},   // AGE
}

// renderDeployTable mirrors the pod / node tables.
func (m Model) renderDeployTable(maxRows, maxWidth int) string {
	rows := rowsForUIDs(m.deployments, m.windowUIDs(ViewDeployments, maxRows))

	w := fitColumns(deployColumns, maxWidth-1)

	hdr := m.Theme.Header
	header := " " + joinCells(
		padCol("NAMESPACE", w[0], hdr),
		padCol("DEPLOYMENT", w[1], hdr),
		padColRight("READY", w[2], hdr),
		padColRight("UP-TO-DATE", w[3], hdr),
		padColRight("AVAILABLE", w[4], hdr),
		padColRight("AGE", w[5], hdr),
	)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if m.tableCount(ViewDeployments) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedDeploys, "deployments"))
		return b.String()
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

		line := warnGlyph(warnIdx, "Deployment", r.Namespace, r.Name, m.Theme) + joinCells(
			padCol(r.Namespace, w[0], m.Theme.Base),
			padCol(r.Name, w[1], m.Theme.Base),
			padColRight(readyStr, w[2], readyStyle),
			padColRight(fmt.Sprintf("%d", r.UpToDate), w[3], m.Theme.Base),
			padColRight(fmt.Sprintf("%d", r.Available), w[4], m.Theme.Base),
			padColRight(formatAge(r.CreatedAt), w[5], m.Theme.Base),
		)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
