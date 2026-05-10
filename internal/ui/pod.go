// Package ui contains the bubbletea Model/Update/View for kubetin.
//
// At M1 the UI is single-cluster: it renders a pod table for one
// context, with a status header and key-hint footer. The cursor is
// anchored to the pod UID so it survives row reorders and updates.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

type podRow struct {
	UID             types.UID
	Namespace       string
	Name            string
	Phase           corev1.PodPhase
	Restarts        int32
	Node            string
	Containers      []string
	ContainerReady  []bool                   // parallel to apiserver ContainerStatuses
	ContainerStates []cluster.ContainerState // parallel to ContainerReady, drives Lens-style colours
	CreatedAt       time.Time
	Updated         time.Time

	// Filled by MetricsSnapshotMsg. Zero values mean "no reading yet".
	CPUMilli   int64
	MemBytes   int64
	HasMetrics bool

	// Filled by NetworkSnapshotMsg. Per-pod rates, sampled from
	// cAdvisor on the kubelet that hosts the pod. Zeroed (and
	// HasNetwork=false) until the first successful scrape after
	// focus-change.
	NetRXBps   int64
	NetTXBps   int64
	HasNetwork bool
}

// SortKey identifies a column to sort by. The order MUST match the
// order of the columns shown in the pod table so `s` cycles in the
// same direction your eyes scan.
type SortKey int

const (
	SortNamespace SortKey = iota
	SortName
	SortStatus
	SortRestarts
	SortAge
	SortCPU
	SortMem
	SortNetRX
	SortNetTX
	SortNode

	sortKeyCount
)

// next cycles through the sort keys in column order.
func (k SortKey) next() SortKey { return (k + 1) % sortKeyCount }

// label returns the short header label for this sort key.
func (k SortKey) label() string {
	switch k {
	case SortNamespace:
		return "ns"
	case SortName:
		return "name"
	case SortStatus:
		return "status"
	case SortRestarts:
		return "restarts"
	case SortAge:
		return "age"
	case SortCPU:
		return "cpu"
	case SortMem:
		return "mem"
	case SortNetRX:
		return "net-rx"
	case SortNetTX:
		return "net-tx"
	case SortNode:
		return "node"
	}
	return "?"
}

// applyPodEvent mutates the pod map in response to one informer event.
func applyPodEvent(m map[types.UID]podRow, ev cluster.PodEvent) {
	switch ev.Kind {
	case cluster.PodDeleted:
		delete(m, ev.UID)
	default:
		m[ev.UID] = podRow{
			UID:             ev.UID,
			Namespace:       ev.Namespace,
			Name:            ev.Name,
			Phase:           ev.Phase,
			Restarts:        ev.Restarts,
			Node:            ev.NodeName,
			Containers:      ev.Containers,
			ContainerReady:  ev.ContainerReady,
			ContainerStates: ev.ContainerStates,
			CreatedAt:       ev.CreatedAt,
			Updated:         time.Now(),
		}
	}
}

// formatCPU renders millicores as "142m" or "1.4" (cores) when ≥1000m.
func formatCPU(m int64) string {
	if m == 0 {
		return "—"
	}
	if m < 1000 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%.1f", float64(m)/1000)
}

// formatMem renders bytes as Mi / Gi (1024 base, k8s convention).
func formatMem(b int64) string {
	if b == 0 {
		return "—"
	}
	const (
		Ki = 1024
		Mi = 1024 * Ki
		Gi = 1024 * Mi
	)
	switch {
	case b >= Gi:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(Gi))
	case b >= Mi:
		return fmt.Sprintf("%dMi", b/Mi)
	case b >= Ki:
		return fmt.Sprintf("%dKi", b/Ki)
	}
	return fmt.Sprintf("%dB", b)
}

// podContainerDots draws one square dot per container with a single
// blank cell between them so each container reads as a distinct box,
// the way Lens renders the column. Order is preserved (apiserver
// ordering) so position-to-container mapping is stable. Overflow
// gets a dim "+N" suffix.
//
// Cell budget: each dot consumes 2 cells (dot + trailing gap) except
// the last dot which is just 1, so visible width = 2N − 1.
func podContainerDots(r podRow, width int, th Theme) string {
	if len(r.ContainerStates) == 0 {
		return th.Dim.Render("—")
	}
	const dot = "■"

	// Reserve " +NN" worth of room for the overflow marker before
	// computing how many dots we can fit.
	const overflowReserved = 4
	dotBudget := width
	overflow := 0
	if 2*len(r.ContainerStates)-1 > dotBudget {
		dotBudget = width - overflowReserved
		if dotBudget < 1 {
			dotBudget = 1
		}
	}
	maxDots := (dotBudget + 1) / 2 // 2N-1 ≤ budget → N ≤ (budget+1)/2
	if maxDots < 1 {
		maxDots = 1
	}
	if len(r.ContainerStates) > maxDots {
		overflow = len(r.ContainerStates) - maxDots
	}

	visible := r.ContainerStates
	if len(visible) > maxDots {
		visible = visible[:maxDots]
	}

	var b strings.Builder
	for i, st := range visible {
		if i > 0 {
			b.WriteByte(' ')
		}
		var style lipgloss.Style
		switch st {
		case cluster.ContainerReady:
			style = th.StatusOK
		case cluster.ContainerWaiting:
			style = th.StatusWrn
		case cluster.ContainerError:
			style = th.StatusBad
		case cluster.ContainerTerminated:
			style = th.StatusDim
		default:
			style = th.Base
		}
		b.WriteString(style.Render(dot))
	}
	if overflow > 0 {
		b.WriteString(" " + th.Dim.Render(fmt.Sprintf("+%d", overflow)))
	}
	return b.String()
}

// formatRate renders bytes/sec compactly. Decimal (1000-base) is the
// usual convention for network throughput — keeping this consistent
// with how iftop/nload/`kubectl top` users read traffic numbers.
//
// "—" is reserved for "no reading yet"; an actual zero rate prints
// as "0 B/s" so the user can tell a quiet pod from a not-yet-scraped one.
func formatRate(bps int64) string {
	if bps < 0 {
		bps = 0
	}
	const (
		k = 1000
		M = 1000 * k
		G = 1000 * M
	)
	switch {
	case bps >= G:
		return fmt.Sprintf("%.1fGB/s", float64(bps)/float64(G))
	case bps >= M:
		return fmt.Sprintf("%.1fMB/s", float64(bps)/float64(M))
	case bps >= k:
		return fmt.Sprintf("%.1fkB/s", float64(bps)/float64(k))
	}
	return fmt.Sprintf("%dB/s", bps)
}

// formatAge produces an htop-style compact age string.
//
//	"12s", "4m", "3h", "14d", "9w"
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// sortedRows returns rows ordered by key, reversed if desc. lessBy
// always produces a strict total order (UID as final tiebreaker), so
// rows never shuffle just because the informer fired an UPDATE.
func sortedRows(m map[types.UID]podRow, key SortKey, desc bool) []podRow {
	out := make([]podRow, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return lessBy(out[j], out[i], key)
		}
		return lessBy(out[i], out[j], key)
	})
	return out
}

// lessBy compares two pods by the active sort key, then falls
// through a deterministic tiebreaker chain: namespace → name → UID.
// UID is unique cluster-wide, so the ordering is total — no ties.
func lessBy(a, b podRow, k SortKey) bool {
	switch k {
	case SortNamespace:
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
	case SortName:
		if a.Name != b.Name {
			return a.Name < b.Name
		}
	case SortStatus:
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
	case SortRestarts:
		if a.Restarts != b.Restarts {
			return a.Restarts < b.Restarts
		}
	case SortCPU:
		if a.CPUMilli != b.CPUMilli {
			return a.CPUMilli < b.CPUMilli
		}
	case SortMem:
		if a.MemBytes != b.MemBytes {
			return a.MemBytes < b.MemBytes
		}
	case SortNetRX:
		if a.NetRXBps != b.NetRXBps {
			return a.NetRXBps < b.NetRXBps
		}
	case SortNetTX:
		if a.NetTXBps != b.NetTXBps {
			return a.NetTXBps < b.NetTXBps
		}
	case SortAge:
		// Older pods first ascending: smaller CreatedAt = "less".
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
	case SortNode:
		if a.Node != b.Node {
			return a.Node < b.Node
		}
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.UID < b.UID
}

// rowIndex returns the index of uid in rows, or -1.
func rowIndex(rows []podRow, uid types.UID) int {
	for i, r := range rows {
		if r.UID == uid {
			return i
		}
	}
	return -1
}
