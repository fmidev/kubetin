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

// ContainerInfo is the per-container projection the status dashboard
// renders. ContainerReady / ContainerStates are derived from this same
// slice in emit, so the table's dot colours and the dashboard's
// container rows can never disagree about a container's state.
type ContainerInfo struct {
	Name     string
	Image    string
	Ready    bool
	State    ContainerState
	Restarts int32
	// Reason is kubelet's waiting/terminated reason
	// ("CrashLoopBackOff", "OOMKilled", "Completed"); empty while the
	// container is running normally.
	Reason string
	// ExitCode is only meaningful when the container terminated.
	ExitCode int32
}

// PodCondition projects one entry of pod.Status.Conditions. Reason and
// Message carry the scheduler's explanation for a False condition
// ("Unschedulable" / "0/5 nodes are available…") — the single most
// useful text when a pod is stuck Pending, and not derivable from
// Phase alone.
type PodCondition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

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

	// Rich per-container detail for the status dashboard. Same
	// ordering and length as ContainerReady / ContainerStates (both
	// are derived from this), so it is likewise shorter than
	// Containers until kubelet reports every status.
	ContainerInfo     []ContainerInfo
	InitContainerInfo []ContainerInfo

	PodIP          string
	HostIP         string
	QOSClass       string
	ServiceAccount string
	StartedAt      time.Time // pod.Status.StartTime; zero before scheduling

	// Labels back the Deployment dashboard's owned-pod lookup, which
	// matches on the deployment's selector rather than the name-prefix
	// heuristic.
	Labels map[string]string

	Conditions []PodCondition
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
	info := projectContainerInfo(pod.Status.ContainerStatuses)
	ready := make([]bool, 0, len(info))
	states := make([]ContainerState, 0, len(info))
	for _, ci := range info {
		ready = append(ready, ci.Ready)
		states = append(states, ci.State)
	}
	var started time.Time
	if pod.Status.StartTime != nil {
		started = pod.Status.StartTime.Time
	}
	ev := PodEvent{
		Kind:              kind,
		Context:           w.Context,
		Namespace:         pod.Namespace,
		Name:              pod.Name,
		UID:               pod.UID,
		Phase:             pod.Status.Phase,
		NodeName:          pod.Spec.NodeName,
		Restarts:          totalRestarts(pod),
		CreatedAt:         pod.CreationTimestamp.Time,
		Containers:        containers,
		ContainerReady:    ready,
		ContainerStates:   states,
		ContainerInfo:     info,
		InitContainerInfo: projectContainerInfo(pod.Status.InitContainerStatuses),
		PodIP:             pod.Status.PodIP,
		HostIP:            pod.Status.HostIP,
		QOSClass:          string(pod.Status.QOSClass),
		ServiceAccount:    pod.Spec.ServiceAccountName,
		StartedAt:         started,
		Labels:            copyLabels(pod.Labels),
		Conditions:        projectPodConditions(pod.Status.Conditions),
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}

// projectContainerInfo builds the rich per-container projection from
// a status slice. Used for both regular and init containers.
func projectContainerInfo(statuses []corev1.ContainerStatus) []ContainerInfo {
	if len(statuses) == 0 {
		return nil
	}
	out := make([]ContainerInfo, 0, len(statuses))
	for _, cs := range statuses {
		ci := ContainerInfo{
			Name:     cs.Name,
			Image:    cs.Image,
			Ready:    cs.Ready,
			State:    projectContainerState(cs),
			Restarts: cs.RestartCount,
		}
		switch {
		case cs.State.Waiting != nil:
			ci.Reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			ci.Reason = cs.State.Terminated.Reason
			ci.ExitCode = cs.State.Terminated.ExitCode
		}
		out = append(out, ci)
	}
	return out
}

// projectPodConditions keeps the conditions verbatim. We don't filter
// to a known set here — an unrecognised condition type (custom
// readiness gates) is still worth showing in the dashboard.
func projectPodConditions(conds []corev1.PodCondition) []PodCondition {
	if len(conds) == 0 {
		return nil
	}
	out := make([]PodCondition, 0, len(conds))
	for _, c := range conds {
		out = append(out, PodCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return out
}

// copyLabels detaches the label map from the informer's cached object.
// Objects in the store must not be mutated and are replaced wholesale
// on update, so aliasing would be safe today — but the whole point of
// these event structs is that they don't hold informer memory.
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
