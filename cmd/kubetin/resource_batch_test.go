package main

import (
	"context"
	"testing"
	"testing/synctest"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/ui"
)

func TestResourceForwardingBatchesWithoutLosingOrder(t *testing.T) {
	const count = 2*resourceBatchMax + 3
	out := make(chan int, count)
	for i := 0; i < count; i++ {
		out <- i
	}
	close(out)
	sender := make(focusTestSender, 3)
	focus := ui.FocusTarget{Context: "alpha", Generation: 42}
	forwardResourceEvents(context.Background(), out, sender, focus, func(i int) tea.Msg { return i })
	if len(sender) != 3 {
		t.Fatalf("got %d messages, want 3 bounded batches", len(sender))
	}
	next := 0
	for len(sender) > 0 {
		msg := (<-sender).(ui.FocusedMsg)
		batch := msg.Msg.(ui.ResourceBatchMsg)
		if msg.Focus != focus || len(batch) > resourceBatchMax {
			t.Fatal("batch lost its focus or exceeded the bound")
		}
		for _, value := range batch {
			if value != next {
				t.Fatalf("got %v, want event %d", value, next)
			}
			next++
		}
	}
	if next != count {
		t.Fatalf("delivered %d of %d events", next, count)
	}
}

func TestQuietResourceForwarderDoesNotWaitForFullBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := make(chan int, 1)
		sender := make(focusTestSender, 1)
		done := make(chan struct{})
		go func() {
			forwardResourceEvents(ctx, out, sender, ui.FocusTarget{}, func(i int) tea.Msg { return i })
			close(done)
		}()
		out <- 7
		synctest.Wait()
		select {
		case msg := <-sender:
			batch := msg.(ui.FocusedMsg).Msg.(ui.ResourceBatchMsg)
			if len(batch) != 1 || batch[0] != 7 {
				t.Fatalf("quiet event changed: %v", batch)
			}
		default:
			t.Fatal("quiet event was held waiting for more events")
		}
		cancel()
		<-done
	})
}
