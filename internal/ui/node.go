package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

type nodeRow struct {
	UID         types.UID
	Name        string
	Ready       bool
	Roles       []string
	KubeletVer  string
	InternalIP  string
	OS          string
	Arch        string
	OSImage     string
	KernelVer   string
	Runtime     string
	Schedulable bool
	CreatedAt   time.Time
	Updated     time.Time

	CPUMilli   int64
	MemBytes   int64
	HasMetrics bool
}

func applyNodeEvent(m map[types.UID]nodeRow, ev cluster.NodeEvent) {
	switch ev.Kind {
	case cluster.NodeDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = nodeRow{
			UID:         ev.UID,
			Name:        ev.Name,
			Ready:       ev.Ready,
			Roles:       ev.Roles,
			KubeletVer:  ev.KubeletVer,
			InternalIP:  ev.InternalIP,
			OS:          ev.OS,
			Arch:        ev.Arch,
			OSImage:     ev.OSImage,
			KernelVer:   ev.KernelVer,
			Runtime:     ev.Runtime,
			Schedulable: ev.Schedulable,
			CreatedAt:   ev.CreatedAt,
			Updated:     time.Now(),
		}
	}
}

func sortedNodeRows(m map[types.UID]nodeRow) []nodeRow {
	out := make([]nodeRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeStatus combines Ready + Schedulable for the STATUS column.
// The cordoned-but-ready state is shown as "Cordoned" rather than
// kubectl's verbose "Ready,SchedulingDisabled" so the column stays
// narrow without losing information.
func nodeStatus(r nodeRow) string {
	switch {
	case !r.Ready:
		return "NotReady"
	case !r.Schedulable:
		return "Cordoned"
	}
	return "Ready"
}

func nodeRolesLabel(r nodeRow) string {
	if len(r.Roles) == 0 {
		return "<none>"
	}
	return strings.Join(r.Roles, ",")
}

// nodeContainerCounts is a per-node tally of container readiness,
// computed once per render and reused across all rows.
type nodeContainerCounts struct {
	ready, notReady int
}

// nodeContainerStates walks the pod cache and groups container ready
// states by spec.nodeName. Pods without a NodeName (Pending/scheduling)
// are skipped — they're not on a node yet so they wouldn't render
// anywhere useful.
func nodeContainerStates(pods map[types.UID]podRow) map[string]nodeContainerCounts {
	out := map[string]nodeContainerCounts{}
	for _, p := range pods {
		if p.Node == "" {
			continue
		}
		c := out[p.Node]
		for _, ready := range p.ContainerReady {
			if ready {
				c.ready++
			} else {
				c.notReady++
			}
		}
		out[p.Node] = c
	}
	return out
}

// containerDots renders one square dot per container — red for not-
// ready, green for ready, with a single blank cell between dots so
// each one reads as a distinct box (Lens-style). Not-ready dots come
// first so the eye lands on them. Visible width = 2N − 1 cells per N
// dots. Overflow surfaces as a dim "+N" suffix.
func containerDots(c nodeContainerCounts, width int, th Theme) string {
	total := c.ready + c.notReady
	if total == 0 {
		return th.Dim.Render("—")
	}
	const dot = "■"

	const overflowReserved = 4
	dotBudget := width
	if 2*total-1 > dotBudget {
		dotBudget = width - overflowReserved
		if dotBudget < 1 {
			dotBudget = 1
		}
	}
	maxDots := (dotBudget + 1) / 2
	if maxDots < 1 {
		maxDots = 1
	}
	overflow := 0
	if total > maxDots {
		overflow = total - maxDots
	}

	// Distribute red/green dots within budget, red first.
	red := c.notReady
	green := c.ready
	if red > maxDots {
		red = maxDots
		green = 0
	} else if red+green > maxDots {
		green = maxDots - red
	}

	var b strings.Builder
	for i := 0; i < red; i++ {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(th.StatusBad.Render(dot))
	}
	for i := 0; i < green; i++ {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(th.StatusOK.Render(dot))
	}
	if overflow > 0 {
		b.WriteString(" " + th.Dim.Render(fmt.Sprintf("+%d", overflow)))
	}
	return b.String()
}

// renderNodeTable mirrors renderTable() for nodes. Nodes have lower
// cardinality than pods (typically 1–50) so we don't bother with the
// windowing logic — just clip to maxRows.
func (m Model) renderNodeTable(maxRows, _ int) string {
	// Apply the same text filter visibleUIDs uses for the cursor —
	// otherwise rows the cursor can't reach still appear.
	all := sortedNodeRows(m.nodes)
	needle := strings.ToLower(m.filterText)
	rows := make([]nodeRow, 0, len(all))
	for _, r := range all {
		if needle != "" && !strings.Contains(strings.ToLower(r.Name), needle) {
			continue
		}
		rows = append(rows, r)
	}

	const (
		colName       = 20
		colSt         = 10
		colRole       = 12
		colCPU        = 7
		colMem        = 9
		colContainers = 24 // up to ~20 dots + overflow marker "+N"
		colAge        = 5
		colVer        = 16
		colIP         = 15
		colOSImage    = 24
		colKernel     = 18
		colRuntime    = 22
	)

	hdr := m.Theme.Header
	header := " " +
		padCol("NODE", colName, hdr) + "  " +
		padCol("STATUS", colSt, hdr) + "  " +
		padCol("ROLES", colRole, hdr) + "  " +
		padColRight("CPU", colCPU, hdr) + "  " +
		padColRight("MEM", colMem, hdr) + "  " +
		padCol("CONTAINERS", colContainers, hdr) + "  " +
		padColRight("AGE", colAge, hdr) + "  " +
		padCol("VERSION", colVer, hdr) + "  " +
		padCol("INTERNAL-IP", colIP, hdr) + "  " +
		padCol("OS-IMAGE", colOSImage, hdr) + "  " +
		padCol("KERNEL-VERSION", colKernel, hdr) + "  " +
		padCol("CONTAINER-RUNTIME", colRuntime, hdr)

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')

	if len(rows) == 0 {
		b.WriteString(m.emptyPlaceholder(m.syncedNodes, "nodes"))
		return b.String()
	}

	// Cursor-centred windowing — same shape as the pod and deploy
	// tables. The original "nodes are 1-50, just clip" shortcut was
	// wrong about its own threshold (default 24-row terminals fit
	// ~18 rows after header/footer/sidebar) so cursor on a row past
	// row 18 of a 25-node cluster was unreachable.
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
	containers := nodeContainerStates(m.pods)
	for _, r := range rows {
		statusStyle := m.Theme.StatusOK
		if !r.Ready {
			statusStyle = m.Theme.StatusBad
		} else if !r.Schedulable {
			statusStyle = m.Theme.StatusWrn
		}
		cpuStr, memStr := "—", "—"
		if r.HasMetrics {
			cpuStr = formatCPU(r.CPUMilli)
			memStr = formatMem(r.MemBytes)
		}
		// Nodes are cluster-scoped — the involvedObject for node
		// events has empty namespace, so use "" here too.
		line := warnGlyph(warnIdx, "Node", "", r.Name, m.Theme) +
			padCol(r.Name, colName, m.Theme.Base) + "  " +
			padCol(nodeStatus(r), colSt, statusStyle) + "  " +
			padCol(nodeRolesLabel(r), colRole, m.Theme.Base) + "  " +
			padColRight(cpuStr, colCPU, m.Theme.Base) + "  " +
			padColRight(memStr, colMem, m.Theme.Base) + "  " +
			padCellANSI(containerDots(containers[r.Name], colContainers, m.Theme), colContainers) + "  " +
			padColRight(formatAge(r.CreatedAt), colAge, m.Theme.Base) + "  " +
			padCol(r.KubeletVer, colVer, m.Theme.Base) + "  " +
			padCol(r.InternalIP, colIP, m.Theme.Base) + "  " +
			padCol(r.OSImage, colOSImage, m.Theme.Base) + "  " +
			padCol(r.KernelVer, colKernel, m.Theme.Base) + "  " +
			padCol(r.Runtime, colRuntime, m.Theme.Base)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
