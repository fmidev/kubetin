package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/ui"
)

func TestCoordinatorKeepsNewestFocusIntent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started []ui.FocusTarget
		var contexts []context.Context
		c := &watchCoordinator{
			parent: context.Background(), reqCh: make(chan struct{}, 1),
			stopCh: make(chan struct{}), doneCh: make(chan struct{}),
			spawn: func(ctx context.Context, f ui.FocusTarget) {
				started = append(started, f)
				contexts = append(contexts, ctx)
			},
		}
		go c.loop()
		c.switchTo(ui.FocusTarget{Context: "alpha"})
		time.Sleep(2 * debounceWindow)
		synctest.Wait()
		// Reverse completion of alpha -> beta -> alpha commands.
		c.switchTo(ui.FocusTarget{Context: "alpha", Generation: 2})
		c.switchTo(ui.FocusTarget{Context: "beta", Generation: 1})
		time.Sleep(2 * debounceWindow)
		synctest.Wait()
		if len(started) != 2 || started[1] != (ui.FocusTarget{Context: "alpha", Generation: 2}) || contexts[0].Err() != context.Canceled {
			t.Fatalf("reverse completion restored stale focus: %+v", started)
		}
		// Fill the wake-up channel before the coordinator can consume it.
		for generation := uint64(3); generation <= 100; generation++ {
			c.switchTo(ui.FocusTarget{Context: "beta", Generation: generation})
		}
		c.switchTo(ui.FocusTarget{Context: "alpha", Generation: 50})
		time.Sleep(2 * debounceWindow)
		synctest.Wait()
		if len(started) != 3 || started[2] != (ui.FocusTarget{Context: "beta", Generation: 100}) || contexts[1].Err() != context.Canceled {
			t.Fatalf("overflow lost newest request: %+v", started)
		}
		c.stop()
		if contexts[2].Err() != context.Canceled {
			t.Fatal("shutdown left current watchers running")
		}
	})
}

type focusTestSender chan tea.Msg

func TestCoordinatorSerializesAcceptanceWithApplication(t *testing.T) {
	applying := make(chan struct{})
	release := make(chan struct{})
	accepted := make(chan struct{})
	started := make(chan ui.FocusTarget, 2)
	c := &watchCoordinator{
		parent: context.Background(), reqCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
		spawn: func(_ context.Context, f ui.FocusTarget) {
			if f.Generation == 1 {
				close(applying)
				<-release
			}
			started <- f
		},
	}
	go c.loop()
	defer c.stop()
	c.switchTo(ui.FocusTarget{Context: "alpha", Generation: 1})
	<-applying
	go func() {
		c.switchTo(ui.FocusTarget{Context: "beta", Generation: 2})
		close(accepted)
	}()
	select {
	case <-accepted:
		t.Error("accepted a newer request while older context application was still in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-accepted
	for generation := uint64(1); generation <= 2; generation++ {
		select {
		case focus := <-started:
			if focus.Generation != generation {
				t.Fatalf("applied focus %+v, want generation %d", focus, generation)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("did not apply request after serialization")
		}
	}
}

func (s focusTestSender) Send(msg tea.Msg) { s <- msg }

func TestWatchForwardersCarryFocusGeneration(t *testing.T) {
	focus := ui.FocusTarget{Context: "alpha", Generation: 42}
	cases := []struct {
		name string
		run  func(context.Context, watchMessageSender)
	}{
		{"pods", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewPodWatcher("alpha", 1)
			w.Out <- cluster.PodEvent{Context: "alpha"}
			forwardPodEvents(ctx, w, sender, focus)
		}},
		{"nodes", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewNodeWatcher("alpha", 1)
			w.Out <- cluster.NodeEvent{Context: "alpha"}
			forwardNodeEvents(ctx, w, sender, focus)
		}},
		{"deployments", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewDeployWatcher("alpha", 1)
			w.Out <- cluster.DeployEvent{Context: "alpha"}
			forwardDeployEvents(ctx, w, sender, focus)
		}},
		{"events", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewEventWatcher("alpha", 1)
			w.Out <- cluster.EventEvent{Context: "alpha"}
			forwardEvtEvents(ctx, w, sender, focus)
		}},
		{"namespaces/projects", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewNamespaceWatcher("alpha", 1)
			w.Out <- cluster.NamespaceEvent{Context: "alpha"}
			forwardNsEvents(ctx, w.Out, sender, focus)
		}},
		{"services", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewServiceWatcher("alpha", 1)
			w.Out <- cluster.ServiceEvent{Context: "alpha"}
			forwardSvcEvents(ctx, w, sender, focus)
		}},
		{"ingresses", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewIngressWatcher("alpha", 1)
			w.Out <- cluster.IngressEvent{Context: "alpha"}
			forwardIngEvents(ctx, w, sender, focus)
		}},
		{"endpoint-slices", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewEndpointSliceWatcher("alpha", 1)
			w.Out <- cluster.EndpointSliceEvent{Context: "alpha"}
			forwardEndpointSliceEvents(ctx, w, sender, focus)
		}},
		{"metrics", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewFocusedMetricsPoller("alpha", 1)
			w.Out <- cluster.MetricsSnapshot{Context: "alpha"}
			forwardMetrics(ctx, w, sender, focus)
		}},
		{"network", func(ctx context.Context, sender watchMessageSender) {
			w := cluster.NewNetworkPoller("alpha", 1)
			w.Out <- cluster.NetworkSnapshot{Context: "alpha"}
			forwardNetwork(ctx, w, sender, focus)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sender := make(focusTestSender, 1)
				go tc.run(ctx, sender)
				msg := <-sender
				if wrapped, ok := msg.(ui.FocusedMsg); !ok || wrapped.Focus != focus || wrapped.Msg == nil {
					t.Fatalf("forwarder lost generation: %#v", msg)
				}
			})
		})
	}
}
