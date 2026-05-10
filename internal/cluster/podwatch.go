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

// PodEventKind classifies a pod cache event.
type PodEventKind uint8

const (
	PodAdded PodEventKind = iota
	PodUpdated
	PodDeleted
)

// ContainerState is a coarse-grained classification of a single
// container's status, mapped to one of four colours in the UI. We
// deliberately collapse the apiserver's richer state onto the four
// buckets Lens-style tooling uses, so there's a stable visual
// vocabulary across the table.
type ContainerState uint8

const (
	// ContainerReady: Running and the readiness probe (or its absence)
	// reports Ready=true. Green.
	ContainerReady ContainerState = iota
	// ContainerWaiting: legitimate startup state — ContainerCreating,
	// PodInitializing, or Running but not yet Ready. Yellow.
	ContainerWaiting
	// ContainerError: stuck-on-failure states — CrashLoopBackOff,
	// ImagePullBackOff, ErrImagePull, OOMKilled, or any non-zero exit.
	// Red. The colour the user wants to scan for.
	ContainerError
	// ContainerTerminated: completed cleanly (exit code 0, "Completed"
	// reason) — initContainers and short-lived workers end up here.
	// Dim grey.
	ContainerTerminated
)

func (k PodEventKind) String() string {
	switch k {
	case PodAdded:
		return "ADD"
	case PodUpdated:
		return "UPD"
	case PodDeleted:
		return "DEL"
	}
	return "???"
}

// PodEvent is a thin projection of a pod cache event — only the fields
// the spike consumes, so we don't pin the full *corev1.Pod in memory
// once it's emitted.
type PodEvent struct {
	Kind       PodEventKind
	Context    string
	Namespace  string
	Name       string
	UID        types.UID
	Phase      corev1.PodPhase
	NodeName   string
	Restarts   int32
	CreatedAt  time.Time
	Containers []string // names of spec.containers (excluding init/ephemeral)

	// Per-container readiness, parallel to ContainerStatuses ordering
	// in the apiserver response. Drives the per-container dot column
	// in the Node view; we project just the bool because the rest of
	// the status (image, state) isn't needed downstream and we don't
	// want to pin the full *corev1.Pod in memory.
	ContainerReady []bool

	// Per-container coarse state, parallel to ContainerReady. Drives
	// the four-colour dot column in the Pod view (green/yellow/red/
	// dim). Kept alongside ContainerReady because the Node view's
	// aggregate cares only about ready/not-ready and we don't want
	// every consumer to re-project from a richer enum.
	ContainerStates []ContainerState
}

// PodWatcher runs a SharedInformerFactory for v1.Pods against one
// cluster and forwards events to Out. Out is bounded; if the consumer
// can't keep up, events are dropped and DroppedEvents is incremented.
type PodWatcher struct {
	Context       string
	Out           chan PodEvent
	DroppedEvents atomic.Uint64
}

// NewPodWatcher returns a watcher with a buffered channel of cap.
func NewPodWatcher(ctxName string, cap int) *PodWatcher {
	return &PodWatcher{
		Context: ctxName,
		Out:     make(chan PodEvent, cap),
	}
}

// Run starts the informer and blocks until ctx is done. It returns the
// first error encountered building the client; informer-internal errors
// (watch 410, reconnects) are handled by the informer machinery itself
// and surface only as resync events on the consumer channel.
func (w *PodWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	// resync=0 → use only watch events; we don't need periodic full
	// resyncs. If the kubeconfig pins a namespace (typical OpenShift /
	// multi-tenant setup), scope the factory there — listing pods at
	// cluster scope would 403 for a namespace-restricted user.
	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Core().V1().Pods().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(PodAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(PodUpdated, obj) },
		DeleteFunc: func(obj any) {
			// On final-state-unknown we still get the last cached *Pod.
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(PodDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("podwatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	// Bound the wait so a stuck list-watch (RBAC, network) is
	// surfaced quickly instead of hanging the watcher silently for
	// the default 5 minutes.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)
	if !synced {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("podwatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("pod cache sync timed out (30s) — RBAC or network")
	}
	klog.Infof("podwatch[%s]: synced, %d initial pods", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *PodWatcher) emit(kind PodEventKind, obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	containers := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		containers = append(containers, c.Name)
	}
	// ContainerStatuses can be sparse during pod startup — only emit
	// readiness/state for entries we actually have. The slice may be
	// shorter than Containers; consumers treat missing entries as
	// "not yet known".
	ready := make([]bool, 0, len(pod.Status.ContainerStatuses))
	states := make([]ContainerState, 0, len(pod.Status.ContainerStatuses))
	for _, cs := range pod.Status.ContainerStatuses {
		ready = append(ready, cs.Ready)
		states = append(states, projectContainerState(cs))
	}
	ev := PodEvent{
		Kind:            kind,
		Context:         w.Context,
		Namespace:       pod.Namespace,
		Name:            pod.Name,
		UID:             pod.UID,
		Phase:           pod.Status.Phase,
		NodeName:        pod.Spec.NodeName,
		Restarts:        totalRestarts(pod),
		CreatedAt:       pod.CreationTimestamp.Time,
		Containers:      containers,
		ContainerReady:  ready,
		ContainerStates: states,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}

// projectContainerState collapses the apiserver's State (oneof
// Running/Waiting/Terminated) plus Ready into one of four buckets.
// The Waiting "reason" enumeration is the most fragile part — the
// list below tracks what kubelet emits for stuck states; anything
// not in the explicit error list is treated as a transient Waiting
// (more permissive but only affects colour, not behaviour).
func projectContainerState(cs corev1.ContainerStatus) ContainerState {
	switch {
	case cs.State.Running != nil:
		if cs.Ready {
			return ContainerReady
		}
		return ContainerWaiting
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		if t.ExitCode == 0 {
			return ContainerTerminated
		}
		return ContainerError
	case cs.State.Waiting != nil:
		switch cs.State.Waiting.Reason {
		case "CrashLoopBackOff",
			"ImagePullBackOff",
			"ErrImagePull",
			"InvalidImageName",
			"CreateContainerConfigError",
			"CreateContainerError",
			"RunContainerError",
			"OOMKilled":
			return ContainerError
		}
		return ContainerWaiting
	}
	return ContainerWaiting
}

func totalRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}
