package cluster

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

var replicaSetSyncTimeout = 30 * time.Second

// ReplicaSetEventKind classifies a ReplicaSet cache event.
type ReplicaSetEventKind uint8

const (
	ReplicaSetAdded ReplicaSetEventKind = iota
	ReplicaSetUpdated
	ReplicaSetDeleted
	ReplicaSetSynced
)

// ReplicaSetEvent retains the controller identity needed to scope rollout events.
type ReplicaSetEvent struct {
	Kind          ReplicaSetEventKind
	Context       string
	Namespace     string
	Name          string
	UID           types.UID
	DeploymentUID types.UID
}

// ReplicaSetWatcher mirrors PodWatcher / NodeWatcher.
type ReplicaSetWatcher struct {
	Context string
	*eventDelivery[ReplicaSetEvent]
}

func NewReplicaSetWatcher(ctxName string, cap int) *ReplicaSetWatcher {
	return &ReplicaSetWatcher{
		Context:       ctxName,
		eventDelivery: newEventDelivery[ReplicaSetEvent](cap),
	}
}

func (w *ReplicaSetWatcher) Run(ctx context.Context, sup *Supervisor) error {
	ctx, stop := w.start(ctx)
	defer stop()
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	scope := sup.ResolveScope(ctx, w.Context, clientset)
	factory := newScopedFactory(clientset, scope)
	informer := factory.Apps().V1().ReplicaSets().Informer()

	handler, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(ReplicaSetAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(ReplicaSetUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(ReplicaSetDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("replicasetwatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, replicaSetSyncTimeout)
	synced := cache.WaitForCacheSync(syncCtx.Done(), handler.HasSynced)
	syncCancel()
	if !synced {
		if ctx.Err() != nil {
			return nil
		}
		klog.Warningf("replicasetwatch[%s]: initial cache sync delayed; retrying", w.Context)
		// Let the reflector's backoff recover without canceling its watch context.
		if !cache.WaitForCacheSync(ctx.Done(), handler.HasSynced) {
			return nil
		}
	}
	klog.Infof("replicasetwatch[%s]: synced, %d initial replicasets", w.Context, len(informer.GetStore().List()))

	// The empty UID is reserved for this ordered cache-completion marker.
	w.publish("", ReplicaSetEvent{Kind: ReplicaSetSynced, Context: w.Context}, false)

	<-ctx.Done()
	return nil
}

func (w *ReplicaSetWatcher) emit(kind ReplicaSetEventKind, obj any) {
	d, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		return
	}

	var ownerUID types.UID
	if owner := metav1.GetControllerOf(d); owner != nil && owner.Kind == "Deployment" && owner.APIVersion == "apps/v1" {
		ownerUID = owner.UID
	}
	ev := ReplicaSetEvent{Kind: kind, Context: w.Context, Namespace: d.Namespace, Name: d.Name, UID: d.UID, DeploymentUID: ownerUID}
	w.publish(ev.UID, ev, kind == ReplicaSetDeleted)
}
