package ui

import (
	"maps"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func TestFocusClearsActionableStateBeforeCommandRuns(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.pods["old"] = podRow{UID: "old", Name: "database", Namespace: "default"}
	m.cursor = "old"
	m.actionMenu.open, m.deleteConfirm.open, m.scaleConfirm.open = true, true, true
	m.restartConfirm.open, m.drainConfirm.open, m.describe.open = true, true, true
	m.eventsLens.open, m.dashboard.open, m.logs.open, m.exec.pickerOpen = true, true, true, true
	m.namespace = "old-namespace"
	m.syncedPods, m.clusterNetOK = true, true
	m.clusterNetRX = 123
	logsStopped, drainStopped := false, false
	m.logs.cancel = func() { logsStopped = true }
	m.drainProgress.cancel = func() { drainStopped = true }
	called := false
	m.OnFocusChange = func(FocusTarget) { called = true }
	cmd := m.focusContext("beta")
	if _, ok := m.refForCursor(); ok || m.cursor != "" || len(m.pods) != 0 || m.WatchedContext != "beta" {
		t.Fatal("old resource remains actionable under the new context")
	}
	if called || cmd == nil || !logsStopped || !drainStopped || m.namespace != "" || m.syncedPods || m.clusterNetOK || m.clusterNetRX != 0 {
		t.Fatal("focus reset was deferred or left old context state behind")
	}
	if m.actionMenu.open || m.deleteConfirm.open || m.scaleConfirm.open || m.restartConfirm.open || m.drainConfirm.open || m.describe.open ||
		m.eventsLens.open || m.dashboard.open || m.logs.open || m.exec.pickerOpen {
		t.Fatal("old cluster dialog survived focus change")
	}
}

func TestFocusCommandsCarryIntentOrderAndNeverClearRows(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	var requests []FocusTarget
	m.OnFocusChange = func(f FocusTarget) { requests = append(requests, f) }
	first := m.focusContext("beta")
	second := m.focusContext("alpha")
	current := m.Focus()
	updated, _ := m.Update(FocusedMsg{Focus: current, Msg: PodEventMsg{Context: "alpha", UID: "current", Name: "database"}})
	m = updated.(Model)
	// Bubble Tea may run these commands in reverse order. Neither result
	// can asynchronously clear rows already received for the newest focus.
	for _, cmd := range []tea.Cmd{second, first} {
		msg := cmd()
		if msg != nil {
			t.Fatalf("focus command returned an asynchronous state mutation: %T", msg)
		}
		updated, _ = m.Update(msg)
		m = updated.(Model)
	}
	if len(requests) != 2 || requests[0] != current || requests[1].Generation >= current.Generation || len(m.pods) != 1 {
		t.Fatalf("lost intent order or cleared fresh rows: requests=%+v pods=%v", requests, m.pods)
	}
}

func TestFocusRejectsPreviousVisitMessages(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	old := m.Focus()
	m.focusContext("beta")
	m.focusContext("alpha")
	m.pods["current"] = podRow{UID: "current", Name: "database", Namespace: "default", CPUMilli: 10, HasMetrics: true}
	m.nodes["current"] = nodeRow{UID: "current", Name: "worker", CPUMilli: 20, HasMetrics: true}
	m.deleteConfirm.open, m.scaleConfirm.open, m.restartConfirm.open = true, true, true
	m.describe.loading, m.drainConfirm.open = true, true
	m.toast = "current toast"
	before := m
	before.pods, before.nodes = maps.Clone(m.pods), maps.Clone(m.nodes)
	before.deployments, before.events = maps.Clone(m.deployments), maps.Clone(m.events)
	before.namespaces, before.services = maps.Clone(m.namespaces), maps.Clone(m.services)
	before.ingresses, before.endpointSlices = maps.Clone(m.ingresses), maps.Clone(m.endpointSlices)
	before.permissions = maps.Clone(m.permissions)
	for _, msg := range []tea.Msg{
		PodEventMsg{Context: "alpha", UID: "ghost"},
		PodEventMsg{Context: "alpha", UID: "current", Kind: cluster.PodDeleted},
		NodeEventMsg{Context: "alpha", UID: "ghost"},
		DeployEventMsg{Context: "alpha", UID: "ghost"},
		EvtEventMsg{Context: "alpha", UID: "ghost"},
		NsEventMsg{Context: "alpha", UID: "ghost"},
		SvcEventMsg{Context: "alpha", UID: "ghost"},
		IngEventMsg{Context: "alpha", UID: "ghost"},
		EndpointSliceEventMsg{Context: "alpha", UID: "ghost"},
		MetricsSnapshotMsg{Context: "alpha"},
		NetworkSnapshotMsg{Context: "alpha", OK: true},
		DescribeResultMsg{Context: "alpha", YAML: "old yaml"},
		PermissionResultMsg{Context: "alpha", Key: "old", Allowed: true},
		DeleteResultMsg{Context: "alpha", OK: true},
		ScaleResultMsg{Context: "alpha", OK: true},
		RolloutResultMsg{Context: "alpha", OK: true},
		NodeOpResultMsg{Context: "alpha", OK: true},
		DrainStartMsg{Context: "alpha", Node: "old"},
		DrainProgressMsg{Context: "alpha", Phase: "blocked", Pod: "old"},
		DrainDoneMsg{Context: "alpha", Total: 1, Remaining: []string{"old"}},
	} {
		for _, stale := range []tea.Msg{FocusedMsg{Focus: old, Msg: msg}, msg} {
			updated, _ := m.Update(stale)
			if !reflect.DeepEqual(before, updated.(Model)) {
				t.Fatalf("stale %T changed current focus state", msg)
			}
		}
	}
	updated, _ := m.Update(FocusedMsg{Focus: m.Focus(), Msg: PodEventMsg{Context: "alpha", UID: "fresh"}})
	if _, ok := updated.(Model).pods["fresh"]; !ok {
		t.Fatal("current generation was rejected")
	}
}

func TestFocusCancelsUndispatchedActionsAndRejectsOldResults(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.pods["selected-uid"] = podRow{UID: "selected-uid", Name: "database", Namespace: "default"}
	m.cursor = "selected-uid"
	ref, _ := m.refForCursor()
	if ref.UID != "selected-uid" {
		t.Fatal("action reference lost the selected UID")
	}
	called := false
	m.OnDelete = func(ctx string, got cluster.DescribeRef) tea.Msg {
		called = true
		if ctx != "alpha" || got != ref {
			t.Errorf("action redirected: context=%s ref=%+v", ctx, got)
		}
		return DeleteResultMsg{Context: ctx, Ref: got, OK: true}
	}
	opened, _ := m.openDeleteConfirm(ref)
	m = opened.(Model)
	m.deleteConfirm.typed = m.deleteConfirm.target
	updated, delayed := m.handleDeleteConfirmKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	// A completed request still belongs to alpha's original visit.
	oldResult := delayed()
	if !called {
		t.Fatal("confirmed action did not run")
	}
	m.focusContext("beta")
	m.focusContext("alpha")
	m.deleteConfirm.open = true
	m.toast = "new dialog"
	updated, _ = m.Update(oldResult)
	m = updated.(Model)
	if !m.deleteConfirm.open || m.toast != "new dialog" {
		t.Fatal("old action result changed a new visit")
	}
	called = false
	if delayed() != nil || called {
		t.Fatal("obsolete queued action ran after focus changed")
	}
}

func TestFocusCancelsLateDrainStart(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	old := m.Focus()
	m.focusContext("beta")
	cancelled := false
	updated, _ := m.Update(FocusedMsg{Focus: old, Msg: DrainStartMsg{
		Context: "alpha", Node: "worker", Cancel: func() { cancelled = true },
	}})
	if !cancelled || updated.(Model).drainProgress.open {
		t.Fatal("late drain acknowledgement leaked an operation or opened a stale dialog")
	}
}
