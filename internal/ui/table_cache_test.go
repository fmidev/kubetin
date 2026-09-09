package ui

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func updateTableModel(m Model, msg tea.Msg) Model {
	updated, _ := m.Update(FocusedMsg{Focus: m.Focus(), Msg: msg})
	return updated.(Model)
}

func TestTableOrderInvalidation(t *testing.T) {
	for _, tc := range []struct {
		view  View
		event func(types.UID, string) tea.Msg
	}{
		{ViewPods, func(uid types.UID, name string) tea.Msg { return PodEventMsg{Context: "alpha", UID: uid, Name: name} }},
		{ViewNodes, func(uid types.UID, name string) tea.Msg { return NodeEventMsg{Context: "alpha", UID: uid, Name: name} }},
		{ViewDeployments, func(uid types.UID, name string) tea.Msg {
			return DeployEventMsg{Context: "alpha", UID: uid, Name: name}
		}},
		{ViewServices, func(uid types.UID, name string) tea.Msg { return SvcEventMsg{Context: "alpha", UID: uid, Name: name} }},
		{ViewIngresses, func(uid types.UID, name string) tea.Msg { return IngEventMsg{Context: "alpha", UID: uid, Name: name} }},
		{ViewNamespaces, func(uid types.UID, name string) tea.Msg { return NsEventMsg{Context: "alpha", UID: uid, Name: name} }},
	} {
		t.Run(string(rune('0'+tc.view)), func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
			m.view = tc.view
			m = updateTableModel(m, tc.event("first", "a"))
			m = updateTableModel(m, tc.event("second", "b"))
			first := m.visibleUIDs()
			if !slices.Equal(first, []types.UID{"first", "second"}) {
				t.Fatalf("initial order = %v", first)
			}
			m = updateTableModel(m, LogLinesMsg{Session: m.logs.session, Lines: []string{"new log line"}})
			if next := m.visibleUIDs(); &next[0] != &first[0] {
				t.Fatal("unrelated log message rebuilt table ordering")
			}
			m = updateTableModel(m, tc.event("first", "z"))
			if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"second", "first"}) {
				t.Fatalf("same-size resource update left stale order: %v", next)
			}
			m.filterText = "Z"
			if m.tableCount(m.view) != 1 || !slices.Equal(m.visibleUIDs(), []types.UID{"first"}) {
				t.Fatal("filter change left stale count or ordering")
			}
			m = updateTableModel(m, tc.event("first", "other"))
			if m.tableCount(m.view) != 0 || len(m.visibleUIDs()) != 0 {
				t.Fatal("same-size update did not invalidate filtering")
			}
			m.filterText = ""
			m.visibleUIDs()
			m.focusContext("beta")
			m.focusContext("alpha")
			if len(m.visibleUIDs()) != 0 || m.tableCount(m.view) != 0 {
				t.Fatal("focus change retained the prior cluster's rows")
			}
		})
	}
}

func TestMetricsRefreshRowsWithoutResortingNames(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 160, 40
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "a", Name: "a", Namespace: "ns"})
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "b", Name: "b", Namespace: "ns"})
	order := m.visibleUIDs()
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "a", Name: "a", Namespace: "ns", Restarts: 7})
	if next := m.visibleUIDs(); &next[0] != &order[0] {
		t.Fatal("restart count rebuilt name ordering")
	}
	metrics := MetricsSnapshotMsg{Context: "alpha", Pods: []cluster.PodMetric{
		{Name: "a", Namespace: "ns", CPUMilli: 500},
		{Name: "b", Namespace: "ns", CPUMilli: 100},
	}}
	m = updateTableModel(m, metrics)
	if next := m.visibleUIDs(); &next[0] != &order[0] {
		t.Fatal("CPU values rebuilt name ordering")
	}
	if !strings.Contains(m.View(), "500m") {
		t.Fatal("cached ordering hid the updated metric value")
	}
	m.sortKey = SortCPU
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"b", "a"}) {
		t.Fatalf("sort change did not apply CPU ordering: %v", next)
	}
	// Even if the selected sort changes before a render, invalidate the
	// sort actually stored in the cache before it can be selected again.
	m.sortKey = SortName
	metrics.Pods[0].CPUMilli = 50
	m = updateTableModel(m, metrics)
	m.sortKey = SortCPU
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"a", "b"}) {
		t.Fatalf("metric update left stale CPU ordering: %v", next)
	}
	m.sortDesc = true
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"b", "a"}) {
		t.Fatalf("direction change left stale ordering: %v", next)
	}
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "b", Kind: cluster.PodDeleted})
	if m.tableCount(ViewPods) != 1 || !slices.Equal(m.visibleUIDs(), []types.UID{"a"}) {
		t.Fatal("deleted row remained in cached ordering")
	}
}

func TestNamespaceOrderTracksDependentResources(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.view, m.nsSortKey, m.nsSortDesc = ViewNamespaces, NsSortPods, true
	m = updateTableModel(m, NsEventMsg{Context: "alpha", UID: "a", Name: "a"})
	m = updateTableModel(m, NsEventMsg{Context: "alpha", UID: "b", Name: "b"})
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "p", Name: "p", Namespace: "b"})
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"b", "a"}) {
		t.Fatalf("pod counts not reflected in namespace ordering: %v", next)
	}
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "p", Name: "p", Namespace: "a"})
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"a", "b"}) || m.namespaceCounts()["b"].pods != 0 {
		t.Fatal("same-size pod update left stale namespace counts")
	}
	m.nsSortKey = NsSortWarn
	m = updateTableModel(m, EvtEventMsg{Context: "alpha", UID: "e", Namespace: "b", Type: "Warning"})
	if next := m.visibleUIDs(); !slices.Equal(next, []types.UID{"b", "a"}) {
		t.Fatalf("warning counts not reflected in namespace ordering: %v", next)
	}
	m = updateTableModel(m, EvtEventMsg{Context: "alpha", UID: "e", Namespace: "b", Type: "Normal"})
	if m.namespaceCounts()["b"].warnings != 0 {
		t.Fatal("event update left stale warning counts")
	}
}

func TestOpaqueOverlaysDoNotBuildHiddenTableOrder(t *testing.T) {
	for _, overlay := range []string{"logs", "help", "describe", "delete", "scale", "restart", "drain", "drain-confirm", "events", "exec", "rbac", "namespace"} {
		t.Run(overlay, func(t *testing.T) {
			m := tableBenchmarkModel(100, overlay)
			switch overlay {
			case "delete":
				m.deleteConfirm.open = true
			case "scale":
				m.scaleConfirm.open = true
			case "restart":
				m.restartConfirm.open = true
			case "drain":
				m.drainProgress.open = true
			case "drain-confirm":
				m.drainConfirm.open = true
			case "events":
				m.eventsLens.open = true
			case "exec":
				m.exec.pickerOpen = true
			case "rbac":
				m.rbacOpen = true
			case "namespace":
				m.nsPickerOpen = true
			}
			m.filterText = "pod-0000"
			m.View()
			if m.tables.orders[ViewPods].valid {
				t.Fatal("opaque overlay sorted its hidden table")
			}
			if m.tableCount(ViewPods) != 10 {
				t.Fatal("avoiding hidden ordering lost the filtered header count")
			}
		})
	}
	m := tableBenchmarkModel(100, "table")
	m.actionMenu.open = true
	m.View()
	if !m.tables.orders[ViewPods].valid {
		t.Fatal("floating action menu lost its table background")
	}
}

func TestResourceBatchAppliesInOrderAndRejectsStaleFocus(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	batch := ResourceBatchMsg{
		PodEventMsg{Context: "alpha", UID: "old", Name: "old"},
		PodEventMsg{Context: "alpha", UID: "old", Kind: cluster.PodDeleted},
		PodEventMsg{Context: "alpha", UID: "new", Name: "new"},
	}
	old := m.Focus()
	m = updateTableModel(m, batch)
	if !slices.Equal(m.visibleUIDs(), []types.UID{"new"}) {
		t.Fatal("batch lost event ordering")
	}
	m.focusContext("beta")
	m.focusContext("alpha")
	updated, _ := m.Update(FocusedMsg{Focus: old, Msg: batch})
	m = updated.(Model)
	if len(m.pods) != 0 {
		t.Fatal("stale batch changed the current visit")
	}
	updated, _ = m.Update(batch)
	if len(updated.(Model).pods) != 0 {
		t.Fatal("untagged batch changed a later focus generation")
	}
	m = updateTableModel(m, batch)
	if !reflect.DeepEqual(m.visibleUIDs(), []types.UID{"new"}) {
		t.Fatal("current batch was rejected after focus changed")
	}
}

func TestTableCountsMatchNamespaceAndHostFilters(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.pods["p"] = podRow{UID: "p", Name: "api", Namespace: "team"}
	m.deployments["d"] = deploymentRow{UID: "d", Name: "api", Namespace: "team"}
	m.nodes["n"] = nodeRow{UID: "n", Name: "worker"}
	m.namespaces["ns"] = nsRow{UID: "ns", Name: "team"}
	m.services["s"] = serviceRow{UID: "s", Name: "api", Namespace: "team"}
	m.ingresses["i"] = ingressRow{UID: "i", Name: "api", Namespace: "team", Hosts: []string{"pay.example.com"}}
	for view := ViewPods; view < ViewFleet; view++ {
		m.view = view
		for _, namespace := range []string{"", "team", "other"} {
			m.namespace = namespace
			for _, filter := range []string{"", "API", "team", "PAY.EXAMPLE.COM", "missing"} {
				m.filterText = filter
				count := m.tableCount(view)
				if got := len(m.visibleUIDs()); got != count {
					t.Fatalf("view=%d ns=%q filter=%q: header count %d != rows %d", view, namespace, filter, count, got)
				}
			}
		}
	}
}

func TestNetworkOrderAndPodPhaseInvalidation(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "a", Name: "a", Phase: "Running"})
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "b", Name: "b", Phase: "Pending"})
	if run, pending, failed := m.podPhaseCounts(); run != 1 || pending != 1 || failed != 0 {
		t.Fatal("initial phase counts are incorrect")
	}
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "b", Name: "b", Phase: "Failed"})
	if run, pending, failed := m.podPhaseCounts(); run != 1 || pending != 0 || failed != 1 {
		t.Fatal("same-size pod update left stale header phase counts")
	}
	m.sortKey = SortNetRX
	m.visibleUIDs()
	m = updateTableModel(m, NetworkSnapshotMsg{Context: "alpha", OK: true, Pods: []cluster.PodNetwork{
		{Name: "a", RXBytesPerSec: 200}, {Name: "b", RXBytesPerSec: 100},
	}})
	order := m.visibleUIDs()
	if !slices.Equal(order, []types.UID{"b", "a"}) {
		t.Fatalf("network update left stale order: %v", order)
	}
	m = updateTableModel(m, NetworkSnapshotMsg{Context: "alpha", OK: false})
	if next := m.visibleUIDs(); &next[0] != &order[0] {
		t.Fatal("failed network sample unnecessarily invalidated order")
	}
	m = updateTableModel(m, PodEventMsg{Context: "alpha", UID: "a", Kind: cluster.PodDeleted})
	if run, pending, failed := m.podPhaseCounts(); run != 0 || pending != 0 || failed != 1 {
		t.Fatal("pod deletion left stale header phase counts")
	}
}

func TestHeaderOnlyTableDoesNotMaterializeRows(t *testing.T) {
	m := tableBenchmarkModel(100, "table")
	rendered := m.renderTable(1, 160)
	if strings.Contains(rendered, "pod-00000") || m.tables.orders[ViewPods].valid {
		t.Fatal("a header-only table built rows that cannot fit")
	}
}
