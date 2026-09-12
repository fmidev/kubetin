package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func describeTestModel() Model {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
		yaml := req.Ref.Name
		if req.Reveal {
			yaml += ": revealed"
		}
		return DescribeResultMsg{Context: focused, Ref: req.Ref, YAML: yaml, Redacted: !req.Reveal}
	}
	return m
}

func TestDescribeRejectsSupersededResults(t *testing.T) {
	for _, sameRef := range []bool{false, true} {
		for _, closeFirst := range []bool{false, true} {
			m := describeTestModel()
			a := cluster.DescribeRef{Kind: "Pod", Name: "a", Namespace: "default"}
			b := a
			if !sameRef {
				b.Name = "b"
			}
			next, cmd := m.startDescribe(a, false)
			m = next.(Model)
			oldRequest, oldResult := m.describe.request, cmd()
			if closeFirst {
				next, _ = m.handleDescribeKey(key("esc"))
				m = next.(Model)
			}
			next, cmd = m.startDescribe(b, false)
			m = next.(Model)
			current := m.describe.request
			if oldRequest.ctx.Err() == nil || current == oldRequest {
				t.Fatal("reopen did not invalidate the old request")
			}
			next, followup := m.Update(oldResult)
			m = next.(Model)
			if !m.describe.loading || m.describe.result.Ref != b || m.describe.result.YAML != "" || followup != nil {
				t.Fatal("old response replaced the newer loading state")
			}
			result := cmd()
			next, _ = m.Update(result)
			m = next.(Model)
			if m.describe.loading || m.describe.result.YAML != b.Name || current.ctx.Err() == nil {
				t.Fatal("current response was not applied and finished")
			}
			m.describe.scroll = 7
			for _, late := range []tea.Msg{oldResult, result} {
				next, _ = m.Update(late)
				m = next.(Model)
				if m.describe.scroll != 7 || m.describe.result.YAML != b.Name {
					t.Fatal("late or duplicate response overwrote the completed request")
				}
			}
		}
	}
}

func TestDescribeRevealRequestsAreIndependent(t *testing.T) {
	m := describeTestModel()
	ref := cluster.DescribeRef{Kind: "Secret", Name: "credentials", Namespace: "default"}
	m.actionMenu.ref = ref
	next, cmd := m.executeAction(ActDescribe)
	m = next.(Model)
	redacted := cmd()
	next, cmd = m.handleDescribeKey(key("Y"))
	m = next.(Model)
	oldReveal := cmd()
	next, cmd = m.handleDescribeKey(key("Y"))
	m = next.(Model)
	for _, stale := range []tea.Msg{redacted, oldReveal} {
		next, _ = m.Update(stale)
		m = next.(Model)
		if !m.describe.revealed || !m.describe.loading || m.describe.result.YAML != "" {
			t.Fatal("earlier fetch overwrote the active reveal request")
		}
	}
	latest := cmd()
	next, _ = m.Update(latest)
	m = next.(Model)
	if m.describe.loading || !m.describe.revealed || m.describe.result.YAML != "credentials: revealed" {
		t.Fatal("latest reveal did not apply")
	}
	next, _ = m.handleDescribeKey(key("esc"))
	m = next.(Model)
	for _, stale := range []tea.Msg{redacted, oldReveal, latest} {
		next, _ = m.Update(stale)
		m = next.(Model)
		if m.describe.open || m.describe.revealed || m.describe.result.YAML != "" {
			t.Fatal("closed describe retained or restored revealed YAML")
		}
	}
	next, _ = m.startDescribe(ref, false)
	m = next.(Model)
	defer m.describe.request.stop()
	if m.describe.revealed || m.describe.result.YAML != "" {
		t.Fatal("normal reopen inherited reveal state")
	}
}

func TestDescribeCancelsInFlightFetch(t *testing.T) {
	for _, action := range []string{"close", "supersede", "focus", "quit"} {
		t.Run(action, func(t *testing.T) {
			m := describeTestModel()
			started := make(chan context.Context, 1)
			m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
				started <- req.Context
				<-req.Context.Done()
				return DescribeResultMsg{Context: focused, Ref: req.Ref, YAML: "stale secret"}
			}
			ref := cluster.DescribeRef{Kind: "Secret", Name: "a"}
			next, cmd := m.startDescribe(ref, true)
			m = next.(Model)
			defer m.describe.request.stop()
			result := make(chan tea.Msg, 1)
			go func() { result <- cmd() }()
			var ctx context.Context
			select {
			case ctx = <-started:
			case <-time.After(time.Second):
				t.Fatal("fetch did not start")
			}
			switch action {
			case "close":
				next, _ = m.handleDescribeKey(key("esc"))
				m = next.(Model)
			case "quit":
				next, _ = m.handleDescribeKey(tea.KeyMsg{Type: tea.KeyCtrlC})
				m = next.(Model)
			case "supersede":
				next, _ = m.startDescribe(ref, false)
				m = next.(Model)
				defer m.describe.request.stop()
			case "focus":
				m.focusContext("beta")
				m.focusContext("alpha")
			}
			if ctx.Err() == nil {
				t.Fatal("obsolete fetch context is still live")
			}
			select {
			case msg := <-result:
				next, _ = m.Update(msg)
				if next.(Model).describe.result.YAML != "" {
					t.Fatal("cancelled fetch restored its YAML")
				}
			case <-time.After(time.Second):
				t.Fatal("fetch did not stop after cancellation")
			}
		})
	}
}

func TestDescribeSkipsClosedQueuedRequest(t *testing.T) {
	m := describeTestModel()
	called := false
	m.OnDescribe = func(DescribeRequestMsg, string) tea.Msg { called = true; return nil }
	m.pods["a"] = podRow{UID: "a", Name: "a", Namespace: "default"}
	m.cursor = "a"
	next, cmd := m.openDescribe(false)
	m = next.(Model)
	m.closeDescribe()
	next, _ = m.Update(cmd())
	if called || next.(Model).describe.open {
		t.Fatal("closed queued describe request ran")
	}
}

func TestMutationModalRequestLifecycle(t *testing.T) {
	for _, op := range []string{"delete", "scale", "restart"} {
		t.Run(op, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			calls := 0
			m.OnDelete = func(ctx string, ref cluster.DescribeRef) tea.Msg {
				calls++
				return DeleteResultMsg{Context: ctx, Ref: ref, OK: true}
			}
			m.OnScale = func(ctx string, ref cluster.DescribeRef, replicas int32) tea.Msg {
				calls++
				return ScaleResultMsg{Context: ctx, Ref: ref, Replicas: replicas, OK: true}
			}
			m.OnRolloutRestart = func(ctx string, ref cluster.DescribeRef) tea.Msg {
				calls++
				return RolloutResultMsg{Context: ctx, Ref: ref, OK: true}
			}
			ref := cluster.DescribeRef{Kind: "Deployment", Namespace: "default", Name: "api"}
			open := func() {
				var next tea.Model
				switch op {
				case "delete":
					next, _ = m.openDeleteConfirm(ref)
				case "scale":
					next, _ = m.openScaleConfirm(ref)
				case "restart":
					next, _ = m.openRestartConfirm(ref)
				}
				m = next.(Model)
				m.deleteConfirm.typed = m.deleteConfirm.target
				m.scaleConfirm.typed = "2"
			}
			press := func(k tea.KeyMsg) tea.Cmd {
				var next tea.Model
				var cmd tea.Cmd
				switch op {
				case "delete":
					next, cmd = m.handleDeleteConfirmKey(k)
				case "scale":
					next, cmd = m.handleScaleConfirmKey(k)
				case "restart":
					next, cmd = m.handleRestartConfirmKey(k)
				}
				m = next.(Model)
				return cmd
			}
			pending := func() bool {
				switch op {
				case "delete":
					return m.deleteConfirm.open && m.deleteConfirm.pending
				case "scale":
					return m.scaleConfirm.open && m.scaleConfirm.pending
				default:
					return m.restartConfirm.open && m.restartConfirm.pending
				}
			}
			open()
			oldResult := press(key("enter"))()
			press(key("esc"))
			m.toast = "unchanged"
			next, cmd := m.Update(oldResult)
			m = next.(Model)
			if m.toast != "unchanged" || cmd != nil {
				t.Fatal("closed operation result changed the toast")
			}
			open() // Same ref: comparing resource names alone is insufficient.
			currentCmd := press(key("enter"))
			next, _ = m.Update(oldResult)
			m = next.(Model)
			if !pending() || m.toast != "unchanged" {
				t.Fatal("old operation closed the newer pending modal")
			}
			currentResult := currentCmd()
			next, _ = m.Update(currentResult)
			m = next.(Model)
			if pending() || !strings.HasPrefix(m.toast, "✓") || calls != 2 {
				t.Fatal("current operation result did not complete normally")
			}
			m.toast = "later notification"
			next, _ = m.Update(currentResult)
			m = next.(Model)
			if m.toast != "later notification" {
				t.Fatal("duplicate operation result replaced a later toast")
			}
			open()
			queued := press(key("enter"))
			press(key("esc"))
			queued()
			if calls != 2 {
				t.Fatal("dismissed queued mutation was executed")
			}
		})
	}
}
