package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/ui"
)

func receiveDrainEvent(t *testing.T, out <-chan tea.Msg, req ui.DrainRequest) tea.Msg {
	t.Helper()
	select {
	case msg := <-out:
		focus, ok := msg.(ui.FocusedMsg)
		if !ok || focus.Focus != req.Focus {
			t.Fatalf("forwarder lost focus: %#v", msg)
		}
		event, ok := focus.Msg.(ui.DrainMsg)
		if !ok || event.Session != req.Session {
			t.Fatalf("forwarder lost operation identity: %#v", focus.Msg)
		}
		return event.Msg
	case <-time.After(2 * time.Second):
		t.Fatal("drain forwarder did not finish")
		return nil
	}
}

func TestStartDrainForwardsOrderedOperationEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := ui.DrainRequest{Context: ctx, Focus: ui.FocusTarget{Context: "alpha", Generation: 7}, Session: 42, Node: "worker"}
	out := make(chan tea.Msg, 10)
	runContext := make(chan context.Context, 1)
	events := []cluster.DrainProgress{
		{Context: "alpha", Node: "worker", Phase: "blocked", Pod: "default/blocked", Err: "PDB", Total: 2},
		{Context: "alpha", Node: "worker", Phase: "evicted", Pod: "default/done", Done: 1, Total: 2},
		{Context: "alpha", Node: "worker", Phase: "error", Err: "deadline", Done: 1, Total: 2, Remaining: []string{"default/blocked"}},
	}
	run := func(ctx context.Context, clusterName, node string, progress chan<- cluster.DrainProgress) {
		defer close(progress)
		runContext <- ctx
		if clusterName != "alpha" || node != "worker" {
			t.Errorf("wrong drain target: %s/%s", clusterName, node)
		}
		for _, ev := range events {
			progress <- ev
		}
	}
	ack := startDrain(context.Background(), req, run, func(msg tea.Msg) { out <- msg }).(ui.DrainStartMsg)
	defer ack.Cancel()
	if ack.Context != "alpha" || ack.Node != "worker" || ack.Err != "" {
		t.Fatalf("incorrect acknowledgement: %+v", ack)
	}
	// Consume the complete stream before delivering its acknowledgement.
	for _, ev := range events {
		if got := receiveDrainEvent(t, out, req); !reflect.DeepEqual(got, ui.DrainProgressMsg(ev)) {
			t.Fatalf("progress reordered or changed: %#v, want %#v", got, ev)
		}
	}
	got := receiveDrainEvent(t, out, req)
	want := ui.DrainDoneMsg{Context: "alpha", Node: "worker", Done: 1, Total: 2, Err: "deadline", Blocked: []string{"default/blocked (PDB)"}, Remaining: []string{"default/blocked"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal summary = %#v, want %#v", got, want)
	}
	running := <-runContext
	select {
	case <-running.Done():
	case <-time.After(time.Second):
		t.Fatal("completed forwarder retained the operation context")
	}
}

func TestStartDrainCancellation(t *testing.T) {
	for _, source := range []string{"request", "shutdown", "acknowledgement"} {
		t.Run(source, func(t *testing.T) {
			parent, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := ui.DrainRequest{Context: ctx, Focus: ui.FocusTarget{Context: "alpha"}, Session: 3, Node: "worker"}
			started := make(chan struct{})
			out := make(chan tea.Msg, 4)
			run := func(ctx context.Context, clusterName, node string, progress chan<- cluster.DrainProgress) {
				defer close(progress)
				close(started)
				<-ctx.Done()
				progress <- cluster.DrainProgress{Context: clusterName, Node: node, Phase: "error", Err: ctx.Err().Error(), Total: 1, Remaining: []string{"default/pod"}}
			}
			ack := startDrain(parent, req, run, func(msg tea.Msg) { out <- msg }).(ui.DrainStartMsg)
			defer ack.Cancel()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("drain did not start")
			}
			switch source {
			case "request":
				cancel()
			case "shutdown":
				shutdown()
			case "acknowledgement":
				ack.Cancel()
			}
			progress := receiveDrainEvent(t, out, req).(ui.DrainProgressMsg)
			done := receiveDrainEvent(t, out, req).(ui.DrainDoneMsg)
			if progress.Phase != "error" || done.Err != context.Canceled.Error() || done.Total != 1 || !reflect.DeepEqual(done.Remaining, []string{"default/pod"}) {
				t.Fatalf("cancellation lost the terminal summary: %+v", done)
			}
		})
	}
}

func TestStartDrainSkipsAlreadyCancelledWork(t *testing.T) {
	for _, source := range []string{"request", "shutdown"} {
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parent, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			if source == "request" {
				cancel()
			} else {
				shutdown()
			}
			req := ui.DrainRequest{Context: ctx, Focus: ui.FocusTarget{Context: "alpha"}, Node: "worker", Session: 1}
			run := func(context.Context, string, string, chan<- cluster.DrainProgress) {
				t.Error("cancelled drain was dispatched")
			}
			ack := startDrain(parent, req, run, func(tea.Msg) { t.Error("cancelled startup published events") }).(ui.DrainStartMsg)
			if ack.Err == "" || ack.Cancel != nil {
				t.Fatal("cancelled startup did not return an error without an operation")
			}
		})
	}
}
