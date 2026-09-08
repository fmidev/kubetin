package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type watcherObservation struct {
	uid     types.UID
	context string
	value   string
	deleted bool
}

func checkWatcherBurst[T any](t *testing.T, d *eventDelivery[T], emit func(int, metav1.ObjectMeta), observe func(T) watcherObservation) {
	t.Helper()
	capacity := cap(d.Out)
	metadata := func(i, revision int) metav1.ObjectMeta {
		return metav1.ObjectMeta{UID: types.UID(fmt.Sprint(i)), Name: fmt.Sprintf("revision-%d", revision), Namespace: "default", Labels: map[string]string{serviceNameLabel: "api"}}
	}
	// Fill the real production-sized channel without a consumer.
	for i := range capacity + 25 {
		emit(0, metadata(i, 1))
	}
	for i := range capacity + 25 {
		emit(1, metadata(i, 2))
	}
	// One visible UID needs a reliable tombstone; an unseen UID can vanish.
	emit(2, metadata(0, 2))
	emit(2, metadata(capacity+24, 2))
	emit(0, metadata(-1, 3))

	if d.CoalescedEvents.Load() == 0 {
		t.Fatal("burst did not exercise coalescing")
	}
	_, stop := d.start(context.Background())
	defer stop()
	state := make(map[types.UID]string)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case raw := <-d.Out:
			ev := observe(raw)
			if ev.context != "alpha" {
				t.Fatalf("lost origin context: %+v", ev)
			}
			if ev.deleted {
				delete(state, ev.uid)
			} else {
				state[ev.uid] = ev.value
			}
			if ev.uid == "-1" {
				if len(state) != capacity+24 {
					t.Fatalf("wrong final resource count: got %d, want %d", len(state), capacity+24)
				}
				for i := 1; i < capacity+24; i++ {
					if got := state[types.UID(fmt.Sprint(i))]; got != "revision-2" {
						t.Fatalf("UID %d missing or stale: %q", i, got)
					}
				}
				if _, ok := state["0"]; ok {
					t.Fatal("deleted UID 0 survived as a ghost")
				}
				if _, ok := state[types.UID(fmt.Sprint(capacity+24))]; ok {
					t.Fatal("unseen deleted UID survived as a ghost")
				}
				return
			}
		case <-deadline.C:
			t.Fatalf("burst never converged; received %d resources", len(state))
		}
	}
}

func TestAllWatchersConvergeAfterOverflow(t *testing.T) {
	t.Run("pods", func(t *testing.T) {
		w := NewPodWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(PodEventKind(kind), &corev1.Pod{ObjectMeta: meta})
		}, func(ev PodEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("nodes", func(t *testing.T) {
		w := NewNodeWatcher("alpha", 64)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(NodeEventKind(kind), &corev1.Node{ObjectMeta: meta})
		}, func(ev NodeEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("deployments", func(t *testing.T) {
		w := NewDeployWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(DeployEventKind(kind), &appsv1.Deployment{ObjectMeta: meta})
		}, func(ev DeployEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("services", func(t *testing.T) {
		w := NewServiceWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(SvcEventKind(kind), &corev1.Service{ObjectMeta: meta})
		}, func(ev ServiceEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("ingresses", func(t *testing.T) {
		w := NewIngressWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(IngEventKind(kind), &networkingv1.Ingress{ObjectMeta: meta})
		}, func(ev IngressEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("events", func(t *testing.T) {
		w := NewEventWatcher("alpha", 512)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(EvtKind(kind), &corev1.Event{ObjectMeta: meta, Message: meta.Name})
		}, func(ev EventEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Message, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("namespaces", func(t *testing.T) {
		w := NewNamespaceWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(NsEventKind(kind), &corev1.Namespace{ObjectMeta: meta})
		}, func(ev NamespaceEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("projects", func(t *testing.T) {
		w := NewProjectWatcher("alpha", 256)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(NsEventKind(kind), &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"uid": string(meta.UID), "name": meta.Name}}})
		}, func(ev NamespaceEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
	t.Run("endpoint-slices", func(t *testing.T) {
		w := NewEndpointSliceWatcher("alpha", 512)
		checkWatcherBurst(t, w.eventDelivery, func(kind int, meta metav1.ObjectMeta) {
			w.emit(EndpointSliceEventKind(kind), &discoveryv1.EndpointSlice{ObjectMeta: meta})
		}, func(ev EndpointSliceEvent) watcherObservation {
			return watcherObservation{uid: ev.UID, context: ev.Context, value: ev.Name, deleted: int(ev.Kind) == 2}
		})
	})
}
