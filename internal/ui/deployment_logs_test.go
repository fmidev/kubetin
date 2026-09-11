package ui

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func TestDeploymentLogEntryPointsAgree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expression bool
		broken     bool
		picker     bool
	}{
		{name: "shared name prefix"},
		{name: "expression-only selector", expression: true},
		{name: "all replicas failed", broken: true},
		{name: "multiple containers", picker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { return req }
			sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
			if tc.expression {
				sel = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"api"}},
				}}
			}
			applyDeployEvent(m.deployments, cluster.DeployEvent{
				UID: "d", Name: "api", Namespace: "default", Selector: sel,
			})
			base := time.Now().Add(-time.Hour)
			m.pods = map[types.UID]podRow{
				"old":      {UID: "old", Name: "api-old-1", Namespace: "default", Phase: "Running", Labels: map[string]string{"app": "api"}, CreatedAt: base},
				"new":      {UID: "new", Name: "api-new-1", Namespace: "default", Phase: "Running", Labels: map[string]string{"app": "api"}, CreatedAt: base.Add(time.Minute), Containers: []string{"app"}},
				"pending":  {UID: "pending", Name: "api-pending-1", Namespace: "default", Phase: "Pending", Labels: map[string]string{"app": "api"}, CreatedAt: base.Add(2 * time.Minute)},
				"worker":   {UID: "worker", Name: "api-worker-new-1", Namespace: "default", Phase: "Running", Labels: map[string]string{"app": "api-worker"}, CreatedAt: base.Add(3 * time.Minute)},
				"other-ns": {UID: "other-ns", Name: "api-other-1", Namespace: "other", Phase: "Running", Labels: map[string]string{"app": "api"}, CreatedAt: base.Add(4 * time.Minute)},
			}
			if tc.broken {
				delete(m.pods, "pending")
				for _, uid := range []types.UID{"old", "new"} {
					p := m.pods[uid]
					p.Phase = "Failed"
					m.pods[uid] = p
				}
			}
			if tc.picker {
				p := m.pods["new"]
				p.Containers = []string{"app", "sidecar"}
				m.pods["new"] = p
			}
			ref := cluster.DescribeRef{Kind: "Deployment", Name: "api", Namespace: "default"}
			m.prepareLogTarget(dashboardTarget{Ref: ref, UID: "d"})
			if m.dashboard.logRef.Name != "api-new-1" {
				t.Fatalf("dashboard chose %+v", m.dashboard.logRef)
			}
			// The action target, rather than a possibly unrelated table cursor,
			// must determine which deployment supplies the selector.
			m.cursor = "other-deployment"
			next, cmd := m.openLogsForCursor(ref)
			got := next.(Model)
			if got.logs.ref != m.dashboard.logRef {
				t.Fatalf("shortcut chose %+v, dashboard chose %+v", got.logs.ref, m.dashboard.logRef)
			}
			if tc.picker {
				if !got.logs.pickerOpen || !reflect.DeepEqual(got.logs.containers, []string{"app", "sidecar"}) || cmd != nil {
					t.Fatal("shortcut did not preserve the selected pod's container picker")
				}
			} else {
				if cmd == nil || got.logs.container != "app" || !got.logs.streaming {
					t.Fatal("shortcut did not start the selected container's logs")
				}
				defer got.logs.cancel()
			}
		})
	}
}

func TestDeploymentLogsRejectUnknownMembership(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  *metav1.LabelSelector
	}{
		{name: "missing deployment"},
		{name: "nil selector"},
		{name: "empty selector", sel: &metav1.LabelSelector{}},
		{name: "invalid selector", sel: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Invalid"}}}},
		{name: "no match", sel: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { return req }
			if tc.name != "missing deployment" {
				applyDeployEvent(m.deployments, cluster.DeployEvent{UID: "d", Name: "api", Namespace: "default", Selector: tc.sel})
			}
			m.pods["p"] = podRow{UID: "p", Name: "api-worker-1", Namespace: "default", Phase: "Running"}
			ref := cluster.DescribeRef{Kind: "Deployment", Namespace: "default", Name: "api"}
			m.prepareLogTarget(dashboardTarget{Ref: ref, UID: "d"})
			if m.dashboard.logRef.Name != "" {
				t.Fatalf("dashboard selected unknown membership: %+v", m.dashboard.logRef)
			}
			next, _ := m.openLogsForCursor(ref)
			got := next.(Model)
			if got.logs.open || got.logs.streaming || got.logs.pickerOpen || !strings.Contains(got.toast, "No matching pod") {
				t.Fatal("shortcut should report no matching pod without opening logs")
			}
		})
	}
}

func TestDeploymentPodsFullSelector(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	d := deploymentRow{Namespace: "default", Selector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "backend", "empty": ""},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"api", "api-v2"}},
			{Key: "track", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"canary"}},
			{Key: "zone", Operator: metav1.LabelSelectorOpExists},
			{Key: "debug", Operator: metav1.LabelSelectorOpDoesNotExist},
		},
	}}
	matching := map[string]string{"app": "api", "tier": "backend", "empty": "", "zone": "west"}
	for _, tc := range []struct {
		name, key, value string
		remove           bool
	}{
		{name: "match"},
		{name: "wrong-app", key: "app", value: "worker"},
		{name: "wrong-tier", key: "tier", value: "frontend"},
		{name: "missing-empty", key: "empty", remove: true},
		{name: "canary", key: "track", value: "canary"},
		{name: "missing-zone", key: "zone", remove: true},
		{name: "debug", key: "debug", value: ""},
	} {
		labels := maps.Clone(matching)
		if tc.key != "" {
			if tc.remove {
				delete(labels, tc.key)
			} else {
				labels[tc.key] = tc.value
			}
		}
		uid := types.UID(tc.name)
		m.pods[uid] = podRow{UID: uid, Name: tc.name, Namespace: "default", Labels: labels}
	}
	if got := podNames(m.deploymentPods(d)); !reflect.DeepEqual(got, []string{"match"}) {
		t.Fatalf("full selector matched %v, want only match", got)
	}
	const want = "app in (api,api-v2),!debug,empty=,tier=backend,track notin (canary),zone"
	if got := formatSelector(d.Selector); got != want {
		t.Fatalf("selector banner = %q, want %q", got, want)
	}
}
