package cluster

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/types"
)

// eventDelivery preserves eventual state, not every intermediate update.
// Pending updates coalesce by UID. A deletion is retained if its UID has
// reached Out or is in flight; otherwise it cancels an unseen addition.
// Memory is bounded by live resources, consumer-visible UIDs, and Out's
// capacity, rather than the number of events during a consumer stall.
type eventDelivery[T any] struct {
	Out             chan T
	CoalescedEvents atomic.Uint64

	mu       sync.Mutex
	queue    list.List
	pending  map[types.UID]*list.Element
	known    map[types.UID]bool
	inFlight *list.Element
	wake     chan struct{}
	closed   bool
}

type pendingEvent[T any] struct {
	uid     types.UID
	event   T
	deleted bool
	version uint64
}

func newEventDelivery[T any](capacity int) *eventDelivery[T] {
	return &eventDelivery[T]{
		Out: make(chan T, capacity), pending: make(map[types.UID]*list.Element),
		known: make(map[types.UID]bool), wake: make(chan struct{}, 1),
	}
}

// start binds delivery and informer lifetimes to the same context, including
// early Run failures. A watcher is started once. Out stays open, as before;
// consumers stop on their watch context rather than channel closure.
func (d *eventDelivery[T]) start(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx)
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func (d *eventDelivery[T]) publish(uid types.UID, event T, deleted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	// Keep the normal buffered path synchronous, without overtaking queued
	// or in-flight events when delivery has fallen behind.
	if d.queue.Len() == 0 {
		select {
		case d.Out <- event:
			d.record(uid, deleted)
			return
		default:
		}
	}
	elem := d.pending[uid]
	if deleted && !d.known[uid] && (elem == nil || elem != d.inFlight) {
		if elem != nil {
			d.remove(elem)
		}
		d.CoalescedEvents.Add(1)
		return
	}
	if elem == nil {
		elem = d.queue.PushBack(pendingEvent[T]{uid: uid, event: event, deleted: deleted})
		d.pending[uid] = elem
	} else {
		old := elem.Value.(pendingEvent[T])
		elem.Value = pendingEvent[T]{uid: uid, event: event, deleted: deleted, version: old.version + 1}
		d.CoalescedEvents.Add(1)
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *eventDelivery[T]) record(uid types.UID, deleted bool) {
	if deleted {
		delete(d.known, uid)
	} else {
		d.known[uid] = true
	}
}

func (d *eventDelivery[T]) remove(elem *list.Element) {
	delete(d.pending, elem.Value.(pendingEvent[T]).uid)
	d.queue.Remove(elem)
}

func (d *eventDelivery[T]) run(ctx context.Context) {
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.closed = true
		d.pending = nil
		d.known = nil
		d.inFlight = nil
		d.queue.Init()
	}()
	for ctx.Err() == nil {
		d.mu.Lock()
		elem := d.queue.Front()
		if elem == nil {
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			}
			continue
		}
		ev := elem.Value.(pendingEvent[T])
		d.inFlight = elem
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case d.Out <- ev.event:
		}
		d.mu.Lock()
		d.inFlight = nil
		d.record(ev.uid, ev.deleted)
		latest := elem.Value.(pendingEvent[T])
		if latest.version == ev.version || latest.deleted && !d.known[ev.uid] {
			d.remove(elem)
		} else {
			// A hot UID must not starve other resources waiting for delivery.
			d.queue.MoveToBack(elem)
		}
		d.mu.Unlock()
	}
}
