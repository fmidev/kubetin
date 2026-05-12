package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// projectsGVR is the OpenShift Project resource we watch. Project is
// OpenShift's per-user-filtered facade in front of Namespace — same
// metadata shape, plus an `openshift.io/display-name` annotation that
// the UI surfaces alongside the name.
var projectsGVR = schema.GroupVersionResource{
	Group:    "project.openshift.io",
	Version:  "v1",
	Resource: "projects",
}

// projectsPreferred returns true when this cluster (a) exposes
// project.openshift.io/v1 in its API surface and (b) the user is
// allowed to list projects. In that case ProjectWatcher is the
// authoritative source for the namespace table; NamespaceWatcher
// skips so we don't double-emit rows.
//
// Both checks are cheap (one discovery GET + one SSAR). We re-do them
// in each watcher rather than caching: the cluster scope only matters
// for the lifetime of a focused-cluster watcher group, and the
// answer is stable across that lifetime.
func projectsPreferred(ctx context.Context, sup *Supervisor, ctxName string, restCfg *rest.Config) bool {
	has, err := hasAPIResource(restCfg, projectsGVR)
	if err != nil || !has {
		return false
	}
	res := sup.CanI(ctx, ctxName, "list", projectsGVR.Group, projectsGVR.Resource, "")
	return res.Allowed && res.Err == ""
}

// ProjectWatcher mirrors NamespaceWatcher but talks to the OpenShift
// Project resource through a dynamic informer (no typed client in
// client-go core). Emits the same NamespaceEvent shape; the
// ResourceKind / DisplayName fields tell the UI it's a Project.
type ProjectWatcher struct {
	Context       string
	Out           chan NamespaceEvent
	DroppedEvents atomic.Uint64
}

func NewProjectWatcher(ctxName string, cap int) *ProjectWatcher {
	return &ProjectWatcher{
		Context: ctxName,
		Out:     make(chan NamespaceEvent, cap),
	}
}

// Run starts the informer and blocks until ctx is done. Skips
// cleanly when the cluster isn't OpenShift or when the user can't
// list projects, so it's safe to spawn unconditionally alongside
// NamespaceWatcher — exactly one of the two will actually emit.
func (w *ProjectWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	if !projectsPreferred(ctx, sup, w.Context, restCfg) {
		klog.Infof("projectwatch[%s]: skipped (no Projects API or RBAC)", w.Context)
		<-ctx.Done()
		return nil
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	informer := factory.ForResource(projectsGVR).Informer()

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

	klog.Infof("projectwatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("projectwatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("project cache sync timed out (30s)")
	}
	klog.Infof("projectwatch[%s]: synced, %d initial projects", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

// emit projects an unstructured Project into our shared
// NamespaceEvent shape. Defensive: anything missing in the object
// (no status phase, no labels) maps to a sensible zero value.
func (w *ProjectWatcher) emit(kind NsEventKind, obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	// Labels copied so downstream consumers can mutate freely without
	// touching the informer cache map.
	var labels map[string]string
	if src := u.GetLabels(); len(src) > 0 {
		labels = make(map[string]string, len(src))
		for k, v := range src {
			labels[k] = v
		}
	}

	// status.phase is a free-form string in the Project schema, but it
	// uses the same NamespacePhase values as the underlying Namespace.
	// Convert through the typed enum so the UI's switch on it works
	// without a special case.
	var phase corev1.NamespacePhase
	if s, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok {
		phase = corev1.NamespacePhase(s)
	}

	displayName := u.GetAnnotations()["openshift.io/display-name"]

	ev := NamespaceEvent{
		Kind:         kind,
		Context:      w.Context,
		Name:         u.GetName(),
		UID:          types.UID(u.GetUID()),
		Phase:        phase,
		CreatedAt:    u.GetCreationTimestamp().Time,
		Labels:       labels,
		ResourceKind: "Project",
		DisplayName:  displayName,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
