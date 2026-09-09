package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func TestDashboardDeletePreservesSelectedUID(t *testing.T) {
	m := dashDeployModel(200, 50, nil)
	want := m.deployOwnedPods(m.deployments["dep-uid"])[1]
	var deleted cluster.DescribeRef
	m.OnDelete = func(_ string, ref cluster.DescribeRef) tea.Msg {
		deleted = ref
		return nil
	}
	moved, _ := m.handleDashboardKey(key("j"))
	opened, _ := moved.(Model).handleDashboardKey(key("enter"))
	confirmed, _ := opened.(Model).executeAction(ActDelete)
	m = confirmed.(Model)
	m.deleteConfirm.typed = m.deleteConfirm.target
	_, cmd := m.handleDeleteConfirmKey(key("enter"))
	cmd()
	if deleted.UID != want.UID || deleted.Name != want.Name || deleted.Namespace != want.Namespace {
		t.Fatalf("delete lost selected replica identity: got %+v, want %+v", deleted, want)
	}
}

func TestExecCompletionUsesOriginatingFocus(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.OnExec = func(focus FocusTarget, _ cluster.DescribeRef, _ string, _ []string) tea.Cmd {
		return func() tea.Msg { return ExecDoneMsg{Focus: focus, Err: errors.New("setup failed")} }
	}
	ref := cluster.DescribeRef{Kind: "Pod", Name: "worker"}
	cmd := m.dispatchExec(ref, "app", nil)
	oldResult := cmd()
	queued := m.dispatchExec(ref, "app", nil)
	m.focusContext("beta")
	m.focusContext("alpha")
	if got := queued(); got != nil {
		t.Fatalf("queued exec survived focus change: %#v", got)
	}
	m.toast = "current toast"
	updated, _ := m.Update(oldResult)
	m = updated.(Model)
	if m.toast != "current toast" {
		t.Fatalf("old exec completion changed toast: %q", m.toast)
	}
	current := m.dispatchExec(ref, "app", nil)
	updated, _ = m.Update(current())
	if !strings.Contains(updated.(Model).toast, "setup failed") {
		t.Fatal("current exec failure was discarded after focus changed")
	}
}

func TestLogStartupLifetime(t *testing.T) {
	for _, transition := range []string{"focus", "close", "replace"} {
		t.Run(transition, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
			started := make(chan LogStartMsg, 1)
			release := make(chan struct{})
			finished := make(chan tea.Msg, 1)
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg {
				started <- req
				<-release
				return LogErrorMsg{Session: req.Session, Err: "old startup failed"}
			}
			ref := cluster.DescribeRef{Kind: "Pod", Name: "worker"}
			cmd := m.beginLogStream(ref, "app")
			go func() { finished <- cmd() }()
			old := <-started
			var next tea.Cmd
			switch transition {
			case "focus":
				m.focusContext("beta")
			case "close":
				closed, _ := m.closeLogs()
				m = closed.(Model)
			case "replace":
				next = m.beginLogStreamTail(ref, "app", logTailAll)
			}
			canceled := old.Context.Err()
			close(release)
			result := <-finished
			if canceled != context.Canceled {
				t.Fatal("in-flight log startup was not synchronously canceled")
			}
			if next != nil {
				next()
				current := <-started
				if current.Context.Err() != nil || current.Session <= old.Session {
					t.Fatal("old startup canceled the replacement session")
				}
				updated, _ := m.Update(result)
				if updated.(Model).logs.err != "" {
					t.Fatal("old startup error contaminated replacement stream")
				}
			}
			m.stopDashboardLogs()
		})
	}
}

func TestLogStartCommandsRunNewestSessionOnly(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	var requests []LogStartMsg
	m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg {
		requests = append(requests, req)
		return nil
	}
	ref := cluster.DescribeRef{Kind: "Pod", Name: "worker"}
	first := m.beginLogStream(ref, "app")
	second := m.beginLogStreamTail(ref, "app", logTailAll)
	second()
	first()
	if len(requests) != 1 || requests[0].Tail != logTailAll || requests[0].Context.Err() != nil {
		t.Fatalf("reversed commands replaced the newest log intent: %+v", requests)
	}
	m.stopDashboardLogs()
	if requests[0].Context.Err() != context.Canceled {
		t.Fatal("closing stream did not cancel its context")
	}
	queued := m.beginLogStream(ref, "app")
	m.stopDashboardLogs()
	queued()
	if len(requests) != 1 {
		t.Fatal("queued log command started after the viewer closed")
	}
}
