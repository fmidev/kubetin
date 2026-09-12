package ui

import (
	"slices"

	"k8s.io/apimachinery/pkg/types"
)

type dashboardPodOrder struct {
	valid     bool
	namespace string
	size      int
	cpu, mem  []types.UID
}

func (m Model) dashboardPodOrder() ([]types.UID, []types.UID) {
	order := &m.tables.dashboardPods
	namespace := m.dashboardNamespace()
	if order.valid && order.namespace == namespace && order.size == len(m.pods) {
		return order.cpu, order.mem
	}
	cpu := make([]types.UID, 0, len(m.pods))
	for uid, p := range m.pods {
		if p.HasMetrics && m.inScope(p.Namespace) {
			cpu = append(cpu, uid)
		}
	}
	mem := slices.Clone(cpu)
	compare := func(a, b types.UID, memory bool) int {
		pa, pb := m.pods[a], m.pods[b]
		va, vb := pa.CPUMilli, pb.CPUMilli
		if memory {
			va, vb = pa.MemBytes, pb.MemBytes
		}
		if va > vb {
			return -1
		}
		if va < vb {
			return 1
		}
		if a == b {
			return 0
		}
		if dashboardNameLess(pa.Namespace, pa.Name, a, pb.Namespace, pb.Name, b) {
			return -1
		}
		return 1
	}
	slices.SortFunc(cpu, func(a, b types.UID) int { return compare(a, b, false) })
	slices.SortFunc(mem, func(a, b types.UID) int { return compare(a, b, true) })
	*order = dashboardPodOrder{valid: true, namespace: namespace, size: len(m.pods), cpu: cpu, mem: mem}
	return cpu, mem
}
