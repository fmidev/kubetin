package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// DeployEventKind classifies a Deployment cache event.
type DeployEventKind uint8

const (
	DeployAdded DeployEventKind = iota
	DeployUpdated
	DeployDeleted
)

// DeployEvent is the thin projection used by the UI.
type DeployEvent struct {
	Kind         DeployEventKind
	Context      string
	Namespace    string
	Name         string
	UID          types.UID
	Replicas     int32
	Ready        int32
	UpToDate     int32
	Available    int32
	Unavailable  int32
	CreatedAt    time.Time
	StrategyType string // RollingUpdate | Recreate
}

// DeployWatcher mirrors PodWatcher / NodeWatcher.
type DeployWatcher struct {
	Context       string
	Out           chan DeployEvent
	DroppedEvents atomic.Uint64
}

func NewDeployWatcher(ctxName string, cap int) *DeployWatcher {
	return &DeployWatcher{
		Context: ctxName,
		Out:     make(chan DeployEvent, cap),
	}
}

func (w *DeployWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Apps().V1().Deployments().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(DeployAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(DeployUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(DeployDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("deploywatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("deploywatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("deployment cache sync timed out (30s)")
	}
	klog.Infof("deploywatch[%s]: synced, %d initial deployments", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *DeployWatcher) emit(kind DeployEventKind, obj any) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	desired := int32(0)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	ev := DeployEvent{
		Kind:         kind,
		Context:      w.Context,
		Namespace:    d.Namespace,
		Name:         d.Name,
		UID:          d.UID,
		Replicas:     desired,
		Ready:        d.Status.ReadyReplicas,
		UpToDate:     d.Status.UpdatedReplicas,
		Available:    d.Status.AvailableReplicas,
		Unavailable:  d.Status.UnavailableReplicas,
		CreatedAt:    d.CreationTimestamp.Time,
		StrategyType: string(d.Spec.Strategy.Type),
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
