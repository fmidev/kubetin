package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func queueTestDrain(t *testing.T, m Model, node string) (Model, DrainRequest, tea.Cmd) {
	t.Helper()
	if m.OnDrainStart == nil {
		m.OnDrainStart = func(req DrainRequest) tea.Msg {
			return DrainStartMsg{Context: req.Focus.Context, Node: req.Node}
		}
	}
	next, _ := m.openDrainConfirm(cluster.DescribeRef{Kind: "Node", Name: node})
	m = next.(Model)
	next, cmd := m.handleDrainConfirmKey(key("enter"))
	m = next.(Model)
	if cmd == nil || m.drainProgress.request == nil || m.drainProgress.session == 0 {
		t.Fatal("identity and cancellation were not allocated before dispatch")
	}
	req := DrainRequest{Context: m.drainProgress.request.ctx, Focus: m.Focus(), Session: m.drainProgress.session, Node: node}
	t.Cleanup(m.drainProgress.request.stop)
	return m, req, cmd
}

func drainTestMessage(req DrainRequest, msg tea.Msg) tea.Msg {
	return FocusedMsg{Focus: req.Focus, Msg: DrainMsg{Session: req.Session, Msg: msg}}
}

func TestDrainRejectsSupersededOperations(t *testing.T) {
	for _, newNode := range []string{"old-node", "new-node"} {
		t.Run(newNode, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			cancelled := 0
			m.OnDrainStart = func(req DrainRequest) tea.Msg {
				return DrainStartMsg{Context: req.Focus.Context, Node: req.Node, Cancel: func() { cancelled++ }}
			}
			m, old, cmd := queueTestDrain(t, m, "old-node")
			start := cmd()
			next, _ := m.handleDrainConfirmKey(key("esc"))
			m = next.(Model)
			if old.Context.Err() == nil {
				t.Fatal("closing pending startup did not cancel its context")
			}
			next, _ = m.openDrainConfirm(cluster.DescribeRef{Name: newNode})
			m = next.(Model)
			stale := []tea.Msg{
				start,
				drainTestMessage(old, DrainProgressMsg{Context: "alpha", Node: old.Node, Phase: "blocked", Pod: "old-pod", Total: 9}),
				drainTestMessage(old, DrainDoneMsg{Context: "alpha", Node: old.Node, Total: 9, Remaining: []string{"old-pod"}}),
			}
			for _, msg := range stale {
				next, _ = m.Update(msg)
				m = next.(Model)
				if !m.drainConfirm.open || m.drainConfirm.node != newNode || m.drainProgress.open || m.toast != "" {
					t.Fatal("obsolete drain replaced a newer confirmation")
				}
			}
			if cancelled != 1 {
				t.Fatal("late startup handle was not cancelled")
			}
			m, current, _ := queueTestDrain(t, m, newNode)
			if old.Session == current.Session {
				t.Fatal("repeated drain reused its operation identity")
			}
			next, _ = m.Update(drainTestMessage(current, DrainProgressMsg{Context: "alpha", Node: newNode, Phase: "waiting", Done: 1, Total: 2}))
			m = next.(Model)
			for _, msg := range stale {
				next, _ = m.Update(msg)
				m = next.(Model)
				if !m.drainProgress.open || m.drainProgress.node != newNode || m.drainProgress.phase != "waiting" || m.drainProgress.done != 1 || len(m.drainProgress.blocked) != 0 {
					t.Fatal("obsolete drain changed newer progress")
				}
			}
		})
	}
}

func TestDrainProgressBeforeAcknowledgement(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	var cancelled int
	m.OnDrainStart = func(req DrainRequest) tea.Msg {
		return DrainStartMsg{Context: req.Focus.Context, Node: req.Node, Cancel: func() { cancelled++ }}
	}
	m, req, cmd := queueTestDrain(t, m, "worker")
	ack := cmd()
	next, _ := m.Update(drainTestMessage(req, DrainProgressMsg{Context: "alpha", Node: req.Node, Phase: "blocked", Pod: "default/pod", Err: "PDB", Done: 1, Total: 3}))
	m = next.(Model)
	for i := 0; i < 2; i++ {
		next, _ = m.Update(ack)
		m = next.(Model)
		if m.drainConfirm.open || !m.drainProgress.open || m.drainProgress.phase != "blocked" || m.drainProgress.done != 1 || m.drainProgress.total != 3 || len(m.drainProgress.blocked) != 1 || cancelled != 0 {
			t.Fatal("late or duplicate acknowledgement reset progress or cancelled the active drain")
		}
	}
	next, _ = m.handleDrainProgressKey(key("esc"))
	m = next.(Model)
	if req.Context.Err() == nil || cancelled != 1 {
		t.Fatal("progress cancellation did not stop both startup and returned handles")
	}
	next, _ = m.Update(drainTestMessage(req, DrainDoneMsg{Context: "alpha", Node: req.Node, Done: 1, Total: 3, Err: "cancelled", Remaining: []string{"default/pod", "default/other"}}))
	m = next.(Model)
	if !m.drainProgress.finished || !m.drainProgress.open || len(m.drainProgress.remaining) != 2 {
		t.Fatal("cancellation lost the terminal summary")
	}
}

func TestDrainCompletionBeforeAcknowledgement(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		m := New("alpha", model.NewStore(), []string{"alpha"})
		cancelled := false
		m.OnDrainStart = func(req DrainRequest) tea.Msg {
			return DrainStartMsg{Context: req.Focus.Context, Node: req.Node, Cancel: func() { cancelled = true }}
		}
		m, req, cmd := queueTestDrain(t, m, "worker")
		ack := cmd()
		done := DrainDoneMsg{Context: "alpha", Node: req.Node, Done: 2, Total: 2}
		if incomplete {
			done.Done, done.Err, done.Remaining = 1, "cancelled", []string{"default/pod"}
		}
		next, _ := m.Update(drainTestMessage(req, done))
		m = next.(Model)
		toast := m.toast
		if m.drainConfirm.open || !m.drainProgress.finished || m.drainProgress.open != incomplete || req.Context.Err() == nil {
			t.Fatal("early completion did not finalize its operation")
		}
		for _, late := range []tea.Msg{ack, drainTestMessage(req, done), drainTestMessage(req, DrainProgressMsg{Context: "alpha", Node: req.Node, Phase: "evicting", Total: 9})} {
			next, _ = m.Update(late)
			m = next.(Model)
			if m.drainProgress.open != incomplete || !m.drainProgress.finished || m.drainProgress.done != done.Done || m.toast != toast {
				t.Fatal("late acknowledgement/progress/completion changed the terminal state")
			}
		}
		if !cancelled {
			t.Fatal("terminal operation's late handle was not released")
		}
	}
}

func TestDrainCancellationBeforeAcknowledgement(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	var cancelled bool
	m.OnDrainStart = func(req DrainRequest) tea.Msg {
		return DrainStartMsg{Context: "alpha", Node: req.Node, Cancel: func() { cancelled = true }}
	}
	m, req, cmd := queueTestDrain(t, m, "worker")
	ack := cmd()
	next, _ := m.Update(drainTestMessage(req, DrainProgressMsg{Context: "alpha", Node: req.Node, Phase: "waiting", Total: 1}))
	m = next.(Model)
	next, _ = m.handleDrainProgressKey(key("esc"))
	m = next.(Model)
	if req.Context.Err() == nil {
		t.Fatal("early progress has no cancellation handle")
	}
	next, _ = m.Update(ack)
	m = next.(Model)
	if !cancelled || m.drainProgress.phase != "waiting" {
		t.Fatal("late acknowledgement did not respect cancellation")
	}
}

func TestDrainSkipsObsoleteQueuedStartup(t *testing.T) {
	for _, action := range []string{"escape", "supersede", "focus", "quit"} {
		t.Run(action, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
			called := false
			m.OnDrainStart = func(DrainRequest) tea.Msg { called = true; return nil }
			m, req, cmd := queueTestDrain(t, m, "worker")
			switch action {
			case "escape":
				next, _ := m.handleDrainConfirmKey(key("esc"))
				m = next.(Model)
			case "supersede":
				next, _ := m.openDrainConfirm(cluster.DescribeRef{Name: "worker"})
				m = next.(Model)
			case "focus":
				m.focusContext("beta")
				m.focusContext("alpha")
			case "quit":
				next, quit := m.handleDrainConfirmKey(tea.KeyMsg{Type: tea.KeyCtrlC})
				m = next.(Model)
				if quit == nil {
					t.Fatal("pending drain swallowed quit")
				}
			}
			cmd()
			if called || req.Context.Err() == nil {
				t.Fatal("obsolete queued drain was executed or kept its context")
			}
			cancelled := false
			next, _ := m.Update(drainTestMessage(req, DrainStartMsg{Context: "alpha", Node: "worker", Cancel: func() { cancelled = true }}))
			if !cancelled || next.(Model).drainProgress.open {
				t.Fatal("obsolete acknowledgement reopened progress or leaked its handle")
			}
		})
	}
}

func TestDrainActiveQuitAndFocusCancel(t *testing.T) {
	for _, action := range []string{"quit", "focus"} {
		t.Run(action, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
			cancelled := false
			m.OnDrainStart = func(req DrainRequest) tea.Msg {
				return DrainStartMsg{Context: "alpha", Node: req.Node, Cancel: func() { cancelled = true }}
			}
			m, req, cmd := queueTestDrain(t, m, "worker")
			next, _ := m.Update(cmd())
			m = next.(Model)
			if action == "quit" {
				next, quit := m.handleDrainProgressKey(tea.KeyMsg{Type: tea.KeyCtrlC})
				m = next.(Model)
				if quit == nil {
					t.Fatal("active drain swallowed quit")
				}
			} else {
				m.focusContext("beta")
			}
			if !cancelled || req.Context.Err() == nil || m.drainProgress.open {
				t.Fatal("leaving an active drain retained its operation")
			}
		})
	}
}
