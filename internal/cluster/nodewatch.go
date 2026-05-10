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

// NodeEventKind classifies a node cache event.
type NodeEventKind uint8

const (
	NodeAdded NodeEventKind = iota
	NodeUpdated
	NodeDeleted
)

func (k NodeEventKind) String() string {
	switch k {
	case NodeAdded:
		return "ADD"
	case NodeUpdated:
		return "UPD"
	case NodeDeleted:
		return "DEL"
	}
	return "???"
}

// NodeEvent is the thin projection of a node cache event used by the UI.
type NodeEvent struct {
	Kind        NodeEventKind
	Context     string
	Name        string
	UID         types.UID
	Ready       bool     // condition Ready == True
	Roles       []string // node-role.kubernetes.io/* labels
	KubeletVer  string
	InternalIP  string
	OS          string
	Arch        string
	OSImage     string // e.g. "Ubuntu 22.04.4 LTS"
	KernelVer   string // e.g. "5.15.0-101-generic"
	Runtime     string // e.g. "containerd://1.7.13"
	CreatedAt   time.Time
	Schedulable bool
}

// NodeWatcher mirrors PodWatcher.
type NodeWatcher struct {
	Context       string
	Out           chan NodeEvent
	DroppedEvents atomic.Uint64
}

// NewNodeWatcher returns a watcher with a buffered channel of cap.
func NewNodeWatcher(ctxName string, cap int) *NodeWatcher {
	return &NodeWatcher{
		Context: ctxName,
		Out:     make(chan NodeEvent, cap),
	}
}

// Run starts the informer and blocks until ctx is done. For namespace-
// restricted users (no cluster list nodes / pods access), node listing
// would 403 — we no-op until cancelled instead of letting the
// cache-sync timeout pollute the debug log every 30s. The scope is
// resolved by probing cluster-list access, not the kubeconfig hint:
// microk8s pins `namespace: default` for users who are nonetheless
// cluster-admins.
func (w *NodeWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	if sup.ResolveScope(ctx, w.Context, clientset) != "" {
		klog.Infof("nodewatch[%s]: skipped (namespace-scoped context)", w.Context)
		<-ctx.Done()
		return nil
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Core().V1().Nodes().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(NodeAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(NodeUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(NodeDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("nodewatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)
	if !synced {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("nodewatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("node cache sync timed out (30s) — RBAC or network")
	}
	klog.Infof("nodewatch[%s]: synced, %d initial nodes", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *NodeWatcher) emit(kind NodeEventKind, obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	ev := NodeEvent{
		Kind:        kind,
		Context:     w.Context,
		Name:        n.Name,
		UID:         n.UID,
		Ready:       isNodeReady(n),
		Roles:       nodeRoles(n),
		KubeletVer:  n.Status.NodeInfo.KubeletVersion,
		InternalIP:  internalIP(n),
		OS:          n.Status.NodeInfo.OperatingSystem,
		Arch:        n.Status.NodeInfo.Architecture,
		OSImage:     n.Status.NodeInfo.OSImage,
		KernelVer:   n.Status.NodeInfo.KernelVersion,
		Runtime:     n.Status.NodeInfo.ContainerRuntimeVersion,
		CreatedAt:   n.CreationTimestamp.Time,
		Schedulable: !n.Spec.Unschedulable,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}

func isNodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeRoles(n *corev1.Node) []string {
	const prefix = "node-role.kubernetes.io/"
	var out []string
	for k := range n.Labels {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k[len(prefix):])
		}
	}
	return out
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}
