package cluster

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// EndpointSliceEventKind classifies an EndpointSlice cache event.
type EndpointSliceEventKind uint8

const (
	EndpointSliceAdded EndpointSliceEventKind = iota
	EndpointSliceUpdated
	EndpointSliceDeleted
)

// serviceNameLabel links an EndpointSlice back to the Service that owns
// it. Slices without it are custom/unmanaged and have no Service row to
// attach to, so they're dropped.
const serviceNameLabel = "kubernetes.io/service-name"

// EndpointSliceEvent projects one slice down to the two numbers the
// Services table needs.
//
// A Service can own several slices — the controller shards at 100
// endpoints, and dual-stack clusters get one per address family — so
// consumers must sum across slices rather than treat one as the answer.
type EndpointSliceEvent struct {
	Kind      EndpointSliceEventKind
	Context   string
	Namespace string
	Name      string
	UID       types.UID
	// ServiceName is the owning Service, from the service-name label.
	ServiceName string
	// Ready counts endpoints whose Conditions.Ready is true; a nil
	// Ready condition means ready per the API contract.
	Ready int
	Total int
}

// EndpointSliceWatcher mirrors ServiceWatcher.
type EndpointSliceWatcher struct {
	Context       string
	Out           chan EndpointSliceEvent
	DroppedEvents atomic.Uint64
}

func NewEndpointSliceWatcher(ctxName string, cap int) *EndpointSliceWatcher {
	return &EndpointSliceWatcher{
		Context: ctxName,
		Out:     make(chan EndpointSliceEvent, cap),
	}
}

func (w *EndpointSliceWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Discovery().V1().EndpointSlices().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(EndpointSliceAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(EndpointSliceUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(EndpointSliceDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("endpointslicewatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		// Not fatal to the Services view: it renders "—" in the READY
		// column and every other cell still comes off the Service.
		klog.Errorf("endpointslicewatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("endpointslice cache sync timed out (30s)")
	}
	klog.Infof("endpointslicewatch[%s]: synced, %d initial slices", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *EndpointSliceWatcher) emit(kind EndpointSliceEventKind, obj any) {
	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	svc := es.Labels[serviceNameLabel]
	if svc == "" {
		return
	}

	ready := 0
	for _, ep := range es.Endpoints {
		// A nil Ready is "ready" per the EndpointSlice API contract —
		// treating nil as not-ready would report 0/N for any cluster
		// whose controller leaves the condition unset.
		if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
			ready++
		}
	}

	ev := EndpointSliceEvent{
		Kind:        kind,
		Context:     w.Context,
		Namespace:   es.Namespace,
		Name:        es.Name,
		UID:         es.UID,
		ServiceName: svc,
		Ready:       ready,
		Total:       len(es.Endpoints),
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}

// itoaPort renders a port number for the "name:port" backend cells.
// Lives here rather than in ingresswatch.go so both files can use it
// without either importing the other's concerns.
func itoaPort(p int32) string { return strconv.FormatInt(int64(p), 10) }
