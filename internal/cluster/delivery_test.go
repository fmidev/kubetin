package cluster

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"k8s.io/apimachinery/pkg/types"
)

type deliveryTestEvent struct {
	uid     types.UID
	version int
	deleted bool
}

func TestDeliveryBoundsChurnAndPreservesInFlightDeletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newEventDelivery[deliveryTestEvent](1)
		_, stop := d.start(context.Background())
		defer stop()
		d.publish("visible", deliveryTestEvent{uid: "visible"}, false)
		d.publish("in-flight", deliveryTestEvent{uid: "in-flight"}, false)
		synctest.Wait()
		d.publish("in-flight", deliveryTestEvent{uid: "in-flight", deleted: true}, true)
		for i := range 10000 {
			uid := types.UID(fmt.Sprint(i))
			d.publish(uid, deliveryTestEvent{uid: uid}, false)
			d.publish(uid, deliveryTestEvent{uid: uid, deleted: true}, true)
			d.publish("visible", deliveryTestEvent{uid: "visible", version: i + 1}, false)
		}
		synctest.Wait()
		d.mu.Lock()
		pending, known, queued := len(d.pending), len(d.known), d.queue.Len()
		d.mu.Unlock()
		if pending != 2 || known != 1 || queued != 2 {
			t.Fatalf("churn grew delivery state: pending=%d known=%d queued=%d", pending, known, queued)
		}
		want := []deliveryTestEvent{
			{uid: "visible"}, {uid: "in-flight"},
			{uid: "visible", version: 10000}, {uid: "in-flight", deleted: true},
		}
		for i, ev := range want {
			if got := <-d.Out; got != ev {
				t.Fatalf("event %d = %+v, want %+v", i, got, ev)
			}
		}
		synctest.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.pending) != 0 || len(d.known) != 1 || !d.known["visible"] {
			t.Fatalf("delivery did not converge: pending=%v known=%v", d.pending, d.known)
		}
	})
}

func TestDeliveryCancellationWhileConsumerStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newEventDelivery[deliveryTestEvent](0)
		ctx, stop := d.start(context.Background())
		d.publish("blocked", deliveryTestEvent{uid: "blocked"}, false)
		synctest.Wait()
		stop()
		if ctx.Err() != context.Canceled {
			t.Fatal("stop did not cancel the informer context")
		}
		d.publish("late", deliveryTestEvent{uid: "late"}, false)
		if !d.closed || len(d.pending) != 0 || len(d.known) != 0 || d.inFlight != nil || d.queue.Len() != 0 {
			t.Fatal("stopped delivery retained or accepted pending work")
		}
	})
}

func TestDeliveryUnbufferedDeletionAndReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newEventDelivery[deliveryTestEvent](0)
		_, stop := d.start(context.Background())
		defer stop()
		d.publish("old-uid", deliveryTestEvent{uid: "old-uid"}, false)
		synctest.Wait()
		d.publish("old-uid", deliveryTestEvent{uid: "old-uid", deleted: true}, true)
		d.publish("new-uid", deliveryTestEvent{uid: "new-uid", version: 2}, false)
		state := make(map[types.UID]int)
		for range 3 {
			ev := <-d.Out
			if ev.deleted {
				delete(state, ev.uid)
			} else {
				state[ev.uid] = ev.version
			}
		}
		if len(state) != 1 || state["new-uid"] != 2 {
			t.Fatalf("replacement left ghost state: %v", state)
		}
	})
}

func TestWatcherStartupErrorStopsDelivery(t *testing.T) {
	w := NewPodWatcher("missing", 0)
	w.publish("pod", PodEvent{Kind: PodAdded, UID: "pod"}, false)
	if err := w.Run(context.Background(), &Supervisor{}); err == nil {
		t.Fatal("expected an unknown-context startup error")
	}
	if !w.closed || len(w.pending) != 0 || w.inFlight != nil {
		t.Fatal("failed watcher retained delivery work")
	}
}
