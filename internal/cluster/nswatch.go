package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// NsEventKind classifies a Namespace cache event.
type NsEventKind uint8

const (
	NsAdded NsEventKind = iota
	NsUpdated
	NsDeleted
)

// NamespaceEvent is the thin UI projection. Kept tight: only the
// columns the table renders (plus UID for diffing). Labels travel as
// a copied map so the consumer can mutate freely without poking the
// informer cache.
//
// ResourceKind identifies whether this came from the core
// `namespaces` API or the OpenShift `project.openshift.io/v1 projects`
// API. The UI uses it to retitle the table ("namespaces" vs
// "projects") and to render Project-only fields like DisplayName.
//
// DisplayName mirrors the `openshift.io/display-name` annotation
// that Projects routinely carry. Empty for plain Namespaces and
// Projects without the annotation.
type NamespaceEvent struct {
	Kind         NsEventKind
	Context      string
	Name         string
	UID          types.UID
	Phase        corev1.NamespacePhase // Active | Terminating
	CreatedAt    time.Time
	Labels       map[string]string
	ResourceKind string // "Namespace" or "Project"
	DisplayName  string
}

// NamespaceWatcher mirrors NodeWatcher: a cluster-scoped informer
// forwarding events to a bounded channel. Drops on consumer back-
// pressure rather than blocking the informer.
type NamespaceWatcher struct {
	Context       string
	Out           chan NamespaceEvent
	DroppedEvents atomic.Uint64
}

// NewNamespaceWatcher returns a watcher with the given channel cap.
func NewNamespaceWatcher(ctxName string, cap int) *NamespaceWatcher {
	return &NamespaceWatcher{
		Context: ctxName,
		Out:     make(chan NamespaceEvent, cap),
	}
}

// Run starts the informer and blocks until ctx is done. For
// namespace-restricted users (OpenShift Project / multi-tenant
// kubeconfig), listing namespaces would 403 — we no-op until
// cancelled instead of letting the cache-sync timeout pollute the
// debug log every 30s. Same skip pattern as NodeWatcher; scope
// is decided by the supervisor's actual-access probe, not the
// kubeconfig hint, so a microk8s admin (kubeconfig pins `default`)
// still sees namespaces.
//
// We also skip when the cluster exposes project.openshift.io/v1 AND
// the user can list projects: that's the case where the ProjectWatcher
// is the source of truth, and running both would double-emit rows.
func (w *NamespaceWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	if sup.ResolveScope(ctx, w.Context, clientset) != "" {
		klog.Infof("nswatch[%s]: skipped (namespace-scoped context)", w.Context)
		<-ctx.Done()
		return nil
	}
	if projectsPreferred(ctx, sup, w.Context, restCfg) {
		klog.Infof("nswatch[%s]: skipped (Projects API preferred on this cluster)", w.Context)
		<-ctx.Done()
		return nil
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Core().V1().Namespaces().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(NsAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(NsUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(NsDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("nswatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("nswatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("namespace cache sync timed out (30s)")
	}
	klog.Infof("nswatch[%s]: synced, %d initial namespaces", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *NamespaceWatcher) emit(kind NsEventKind, obj any) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	// Copy labels so consumers can keep / drop / mutate them without
	// risking a write into the informer cache map.
	var labels map[string]string
	if len(ns.Labels) > 0 {
		labels = make(map[string]string, len(ns.Labels))
		for k, v := range ns.Labels {
			labels[k] = v
		}
	}
	ev := NamespaceEvent{
		Kind:         kind,
		Context:      w.Context,
		Name:         ns.Name,
		UID:          ns.UID,
		Phase:        ns.Status.Phase,
		CreatedAt:    ns.CreationTimestamp.Time,
		Labels:       labels,
		ResourceKind: "Namespace",
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
