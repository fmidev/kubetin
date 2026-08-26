package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func netModel(w, h int, view View, extra func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = w, h
	m.view = view
	netFixture(extra)(&m)
	return m
}

// A Service owns several EndpointSlices — the controller shards past
// 100 endpoints, and dual-stack clusters get one per address family —
// so the count has to be a sum, not whichever slice arrived last.
func TestCollectEndpointCountsSumsSlices(t *testing.T) {
	slices := map[types.UID]endpointSliceRow{
		"a": {Namespace: "default", ServiceName: "api", Ready: 2, Total: 3},
		"b": {Namespace: "default", ServiceName: "api", Ready: 1, Total: 1},
		// Same service name in a different namespace must not merge.
		"c": {Namespace: "staging", ServiceName: "api", Ready: 5, Total: 5},
	}
	got := collectEndpointCounts(slices)

	if c := got[endpointKey("default", "api")]; c.Ready != 3 || c.Total != 4 {
		t.Errorf("default/api = %+v, want {3 4}", c)
	}
	if c := got[endpointKey("staging", "api")]; c.Ready != 5 || c.Total != 5 {
		t.Errorf("staging/api = %+v, want {5 5} — namespaces must not merge", c)
	}
}

// Deleting a slice must remove its contribution. Storing per slice
// rather than per service is what makes this fall out for free.
func TestEndpointSliceDeleteDropsContribution(t *testing.T) {
	m := map[types.UID]endpointSliceRow{}
	applyEndpointSliceEvent(m, cluster.EndpointSliceEvent{
		Kind: cluster.EndpointSliceAdded, UID: "a", Namespace: "default",
		ServiceName: "api", Ready: 2, Total: 2,
	})
	applyEndpointSliceEvent(m, cluster.EndpointSliceEvent{
		Kind: cluster.EndpointSliceAdded, UID: "b", Namespace: "default",
		ServiceName: "api", Ready: 3, Total: 3,
	})
	if c := collectEndpointCounts(m)[endpointKey("default", "api")]; c.Ready != 5 {
		t.Fatalf("before delete: ready = %d, want 5", c.Ready)
	}

	applyEndpointSliceEvent(m, cluster.EndpointSliceEvent{
		Kind: cluster.EndpointSliceDeleted, UID: "b", Namespace: "default", ServiceName: "api",
	})
	if c := collectEndpointCounts(m)[endpointKey("default", "api")]; c.Ready != 2 || c.Total != 2 {
		t.Errorf("after delete: %+v, want {2 2}", c)
	}
}

// The READY cell is the reason the view exists, so its four states have
// to be distinguishable — especially "selects nothing", which looks
// perfectly healthy from the Service spec alone.
func TestServiceReadyCell(t *testing.T) {
	m := netModel(120, 20, ViewServices, nil)
	counts := collectEndpointCounts(m.endpointSlices)

	cases := []struct {
		name  string
		uid   types.UID
		want  string
		style string
	}{
		{"all ready", "svc-lb", "2/2", "ok"},
		{"partially ready across two slices", "svc-api", "3/4", "warn"},
		{"selects nothing", "svc-orphan", "0/0", "bad"},
		{"no selector at all", "svc-headless", "—", "dim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, style := m.serviceReadyCell(m.services[tc.uid], counts)
			if got != tc.want {
				t.Errorf("cell = %q, want %q", got, tc.want)
			}
			want := map[string]lipgloss.Style{
				"ok": m.Theme.StatusOK, "warn": m.Theme.StatusWrn,
				"bad": m.Theme.StatusBad, "dim": m.Theme.Dim,
			}[tc.style]
			if style.Render("x") != want.Render("x") {
				t.Errorf("style for %q is not the %s style", tc.want, tc.style)
			}
		})
	}
}

// A Service we've never seen a slice for is unknown, not empty. Claiming
// 0/0 while the informer is still syncing — or was denied by RBAC —
// would report a healthy Service as broken.
func TestServiceReadyUnknownWithoutSlices(t *testing.T) {
	m := netModel(120, 20, ViewServices, func(m *Model) {
		m.endpointSlices = map[types.UID]endpointSliceRow{}
	})
	got, _ := m.serviceReadyCell(m.services["svc-api"], collectEndpointCounts(m.endpointSlices))
	if got != "—" {
		t.Errorf("cell = %q, want %q for a Service with no slices seen yet", got, "—")
	}
}

func TestFormatServicePorts(t *testing.T) {
	cases := []struct {
		name  string
		ports []cluster.ServicePort
		want  string
	}{
		{"none", nil, "—"},
		{"cluster port", []cluster.ServicePort{{Port: 80, Protocol: "TCP"}}, "80/TCP"},
		{"node port", []cluster.ServicePort{{Port: 80, NodePort: 30821, Protocol: "TCP"}}, "80:30821/TCP"},
		{"multiple", []cluster.ServicePort{
			{Port: 53, Protocol: "UDP"}, {Port: 53, Protocol: "TCP"},
		}, "53/UDP,53/TCP"},
		{"protocol defaults to TCP", []cluster.ServicePort{{Port: 80}}, "80/TCP"},
	}
	for _, tc := range cases {
		if got := formatServicePorts(tc.ports); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The external column distinguishes "points elsewhere", "waiting on a
// controller" and "not applicable" — collapsing them would hide the
// one that means something is wrong.
func TestServiceExternal(t *testing.T) {
	cases := []struct {
		name        string
		row         serviceRow
		want        string
		wantPending bool
	}{
		{"clusterip has none", serviceRow{Type: "ClusterIP"}, "—", false},
		{"loadbalancer pending", serviceRow{Type: "LoadBalancer"}, "<pending>", true},
		{"loadbalancer assigned", serviceRow{
			Type: "LoadBalancer", ExternalIPs: []string{"34.88.10.4"},
		}, "34.88.10.4", false},
		{"several addresses collapse", serviceRow{
			Type: "LoadBalancer", ExternalIPs: []string{"a", "b", "c"},
		}, "a,+2", false},
		{"externalname", serviceRow{
			Type: "ExternalName", ExternalName: "db.example.com",
		}, "db.example.com", false},
	}
	for _, tc := range cases {
		got, pending := serviceExternal(tc.row)
		if got != tc.want || pending != tc.wantPending {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, got, pending, tc.want, tc.wantPending)
		}
	}
}

// The cursor walks visibleUIDs while the renderer walks
// visibleServiceRows; if they disagree, j/k can park on a row that
// isn't drawn.
func TestServiceFilterAgreesWithCursor(t *testing.T) {
	m := netModel(120, 20, ViewServices, func(m *Model) {
		m.namespace = "default"
		m.filterText = "payments"
	})

	uids := m.visibleUIDs()
	rows := m.visibleServiceRows()
	if len(uids) != len(rows) {
		t.Fatalf("visibleUIDs = %d rows, visibleServiceRows = %d", len(uids), len(rows))
	}
	if len(rows) != 2 {
		t.Fatalf("expected the two default/payments-* services, got %d", len(rows))
	}
	for i, r := range rows {
		if uids[i] != r.UID {
			t.Errorf("row %d: cursor sees %q, table draws %q", i, uids[i], r.UID)
		}
	}

	out := m.View()
	if strings.Contains(out, "orders-api") {
		t.Error("filtered-out service still rendered")
	}
	if !strings.Contains(out, "payments-api") {
		t.Error("matching service missing from the render")
	}
}

// Deleting a Service removes the row; unrelated rows survive.
func TestApplyServiceEventDelete(t *testing.T) {
	m := map[types.UID]serviceRow{}
	applyServiceEvent(m, cluster.ServiceEvent{
		Kind: cluster.SvcAdded, UID: "a", Namespace: "default", Name: "api",
	})
	applyServiceEvent(m, cluster.ServiceEvent{
		Kind: cluster.SvcAdded, UID: "b", Namespace: "default", Name: "web",
	})
	applyServiceEvent(m, cluster.ServiceEvent{Kind: cluster.SvcDeleted, UID: "a"})

	if _, ok := m["a"]; ok {
		t.Error("deleted service still present")
	}
	if _, ok := m["b"]; !ok {
		t.Error("unrelated service was removed")
	}
}
