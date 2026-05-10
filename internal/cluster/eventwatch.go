package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// EvtKind classifies a Kubernetes Event cache event.
type EvtKind uint8

const (
	EvtAdded EvtKind = iota
	EvtUpdated
	EvtDeleted
)

// EventEvent is the projection of a *corev1.Event used by the UI.
// (Yes the doubled "Event" is awkward — it's "an event observation
// of an Event resource".)
type EventEvent struct {
	Kind         EvtKind
	Context      string
	UID          types.UID
	Namespace    string
	Reason       string
	Message      string
	Type         string // Normal | Warning
	Count        int32
	FirstSeen    time.Time
	LastSeen     time.Time
	InvolvedKind string
	InvolvedName string
	InvolvedNs   string
}

// EventWatcher mirrors the other watchers.
type EventWatcher struct {
	Context       string
	Out           chan EventEvent
	DroppedEvents atomic.Uint64
}

func NewEventWatcher(ctxName string, cap int) *EventWatcher {
	return &EventWatcher{
		Context: ctxName,
		Out:     make(chan EventEvent, cap),
	}
}

func (w *EventWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Core().V1().Events().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(EvtAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(EvtUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(EvtDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("eventwatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("eventwatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("event cache sync timed out (30s)")
	}
	klog.Infof("eventwatch[%s]: synced, %d initial events", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *EventWatcher) emit(kind EvtKind, obj any) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	last := e.LastTimestamp.Time
	if last.IsZero() {
		last = e.EventTime.Time
	}
	if last.IsZero() {
		last = e.CreationTimestamp.Time
	}
	first := e.FirstTimestamp.Time
	if first.IsZero() {
		first = last
	}
	count := e.Count
	if count == 0 {
		count = 1
	}
	ev := EventEvent{
		Kind:         kind,
		Context:      w.Context,
		UID:          e.UID,
		Namespace:    e.Namespace,
		Reason:       e.Reason,
		Message:      e.Message,
		Type:         e.Type,
		Count:        count,
		FirstSeen:    first,
		LastSeen:     last,
		InvolvedKind: e.InvolvedObject.Kind,
		InvolvedName: e.InvolvedObject.Name,
		InvolvedNs:   e.InvolvedObject.Namespace,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
