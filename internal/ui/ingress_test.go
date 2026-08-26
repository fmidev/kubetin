package ui

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// Multi-valued cells collapse to "first,+N" so a row stays one line —
// an unbounded host list would wrap and break the canvas contract.
func TestCollapseList(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "—"},
		{[]string{}, "—"},
		{[]string{"a.example.com"}, "a.example.com"},
		{[]string{"a.example.com", "b.example.com"}, "a.example.com,+1"},
		{[]string{"a", "b", "c", "d"}, "a,+3"},
	}
	for _, tc := range cases {
		if got := collapseList(tc.in); got != tc.want {
			t.Errorf("collapseList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatBackends(t *testing.T) {
	cases := []struct {
		name string
		in   []cluster.IngressBackend
		want string
	}{
		{"none", nil, "—"},
		{"one", []cluster.IngressBackend{{Service: "api", Port: "80"}}, "api:80"},
		{"named port", []cluster.IngressBackend{{Service: "api", Port: "http"}}, "api:http"},
		{"portless", []cluster.IngressBackend{{Service: "api"}}, "api"},
		{"several collapse", []cluster.IngressBackend{
			{Service: "api", Port: "80"}, {Service: "ui", Port: "80"},
		}, "api:80,+1"},
	}
	for _, tc := range cases {
		if got := formatBackends(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Ports are derived from whether any rule is TLS-covered, because the
// Ingress object carries no port field of its own.
func TestIngressPorts(t *testing.T) {
	if got := ingressPorts(ingressRow{}); got != "80" {
		t.Errorf("plain ingress ports = %q, want 80", got)
	}
	if got := ingressPorts(ingressRow{TLSHosts: 2}); got != "80,443" {
		t.Errorf("TLS ingress ports = %q, want 80,443", got)
	}
}

// An Ingress with no published address hasn't been admitted by a
// controller, which is the usual reason one exists but answers nothing.
// It has to read differently from an ingress that simply has no class.
func TestIngressPendingAddressRendered(t *testing.T) {
	m := netModel(160, 20, ViewIngresses, nil)
	out := m.View()

	if !strings.Contains(out, "<pending>") {
		t.Errorf("expected the un-admitted ingress to show <pending>:\n%s", out)
	}
	if !strings.Contains(out, "34.88.10.4") {
		t.Errorf("expected the admitted ingress to show its address:\n%s", out)
	}
}

// Hosts are searchable: "which ingress serves pay.example.com" is the
// question this view gets asked, and the host isn't in the name.
func TestIngressFilterMatchesHosts(t *testing.T) {
	m := netModel(160, 20, ViewIngresses, func(m *Model) {
		m.filterText = "pay.example.com"
	})

	rows := m.visibleIngressRows()
	if len(rows) != 1 || rows[0].Name != "payments" {
		t.Fatalf("host filter matched %d rows (%v), want just payments", len(rows), rows)
	}
	if len(m.visibleUIDs()) != len(rows) {
		t.Error("cursor and renderer disagree on the filtered set")
	}
}

func TestIngressFilterMatchesName(t *testing.T) {
	m := netModel(160, 20, ViewIngresses, func(m *Model) { m.filterText = "orders" })
	if rows := m.visibleIngressRows(); len(rows) != 1 || rows[0].Name != "orders" {
		t.Errorf("name filter matched %d rows, want just orders", len(rows))
	}
}

func TestApplyIngressEventDelete(t *testing.T) {
	m := map[types.UID]ingressRow{}
	applyIngressEvent(m, cluster.IngressEvent{
		Kind: cluster.IngAdded, UID: "a", Namespace: "default", Name: "web",
	})
	applyIngressEvent(m, cluster.IngressEvent{Kind: cluster.IngDeleted, UID: "a"})
	if len(m) != 0 {
		t.Errorf("deleted ingress still present: %v", m)
	}
}

// The two new views must reach every row via j/k. windowRows centres on
// the cursor rather than head-truncating, which is what makes rows past
// the fold reachable at all.
func TestWindowRowsKeepsCursorVisible(t *testing.T) {
	type row struct {
		id types.UID
	}
	rows := make([]row, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, row{id: types.UID(string(rune('a' + i)))})
	}
	uid := func(r row) types.UID { return r.id }

	// Cursor near the end must still appear in the window.
	got := windowRows(rows, "t", 6, uid) // 'a'+19
	if len(got) != 5 {
		t.Fatalf("window = %d rows, want 5 (maxRows-1)", len(got))
	}
	found := false
	for _, r := range got {
		if r.id == "t" {
			found = true
		}
	}
	if !found {
		t.Errorf("cursor row fell outside the window: %v", got)
	}

	// Fewer rows than the window: everything, unchanged.
	short := rows[:3]
	if got := windowRows(short, "a", 10, uid); len(got) != 3 {
		t.Errorf("short list = %d rows, want all 3", len(got))
	}
	// Degenerate height must not panic or slice out of range.
	if got := windowRows(rows, "a", 1, uid); len(got) != len(rows) {
		t.Errorf("maxRows=1 returned %d rows; expected the unwindowed slice", len(got))
	}
}
