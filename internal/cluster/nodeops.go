// Node maintenance operations: Cordon, Uncordon, Drain.
//
// Cordon / Uncordon are single strategic-merge PATCH calls on
// /spec/unschedulable. Drain is a higher-level operation: list pods
// on the node, filter out the ones kubectl drain would also skip
// (mirror, DaemonSet-owned, already-completed), and evict the rest
// via the policy/v1 pods/eviction subresource with PDB-aware retry.
//
// We deliberately don't replicate every kubectl-drain flag here.
// --force, --delete-emptydir-data, --grace-period, --timeout: useful
// in scripts, less so for a TUI where the user already sees per-pod
// progress and can hit Esc. v1 ships with safe defaults
// (evict-only, retry on PDB, no force).
package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// NodeOpResult is the outcome of a Cordon or Uncordon call. Context
// lets a UI receiver drop a stale result that arrived after the user
// Tabbed away (same pattern as ScaleResult / DeleteResult).
type NodeOpResult struct {
	Context string
	Node    string
	Op      string // "cordon" | "uncordon"
	OK      bool
	Err     string
}

// Cordon marks a node unschedulable. Idempotent — calling cordon on
// an already-cordoned node is a no-op against the apiserver because
// strategic-merge collapses the patch.
func (s *Supervisor) Cordon(ctx context.Context, ctxName, node string) NodeOpResult {
	return s.setUnschedulable(ctx, ctxName, node, true)
}

// Uncordon marks a node schedulable again.
func (s *Supervisor) Uncordon(ctx context.Context, ctxName, node string) NodeOpResult {
	return s.setUnschedulable(ctx, ctxName, node, false)
}

func (s *Supervisor) setUnschedulable(ctx context.Context, ctxName, node string, value bool) NodeOpResult {
	op := "uncordon"
	if value {
		op = "cordon"
	}
	out := NodeOpResult{Context: ctxName, Node: node, Op: op}

	cs, err := s.coreClient(ctxName)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, value)
	if _, err := cs.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		out.Err = err.Error()
		return out
	}
	out.OK = true
	return out
}

// coreClient is a small helper used by every node op so the
// kubeconfig + clientset construction lives in one place.
func (s *Supervisor) coreClient(ctxName string) (*kubernetes.Clientset, error) {
	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	restCfg.Timeout = 30 * time.Second
	return kubernetes.NewForConfig(restCfg)
}

// DrainProgress is one event in a Drain stream. The sequence the
// UI sees, in order:
//
//	{ Total: N, Phase: "starting" }      ← exactly once, after cordon
//	{ Pod: ns/foo, Phase: "evicting" }   ← once per pod, in order
//	{ Pod: ns/foo, Phase: "evicted" }    ← once per pod that drained
//	{ Pod: ns/bar, Phase: "blocked" }    ← if PDB-blocked past retries
//	{ Phase: "done", Done: K, Total: N } ← exactly once, terminal
//	{ Phase: "error", Err: "…" }         ← exactly once if we abort
//
// "blocked" and "error" carry an Err string; everything else uses
// Pod / Done / Total. Both terminal phases ("done", "error") close
// the stream.
type DrainProgress struct {
	Context string
	Node    string
	Phase   string
	Pod     string
	Done    int
	Total   int
	Err     string
}

// drainPDBMaxRetries bounds retries against a PodDisruptionBudget
// rejection (apiserver returns 429 TooManyRequests for evictions
// blocked by PDB). Each retry uses exponential backoff: 1s, 2s, 4s,
// 8s, 16s — total ≈ 31s per pod. After this, the pod is marked
// "blocked" in progress and the drain moves on.
const drainPDBMaxRetries = 5

// Drain cordons the node, then evicts every pod on it that kubectl
// drain would also evict (skipping mirror, DaemonSet-owned, and
// already-completed pods). Progress events flow to out; the caller
// is expected to range over it from a goroutine and ship each event
// to the UI.
//
// Cancellation: parent ctx is honored at every yield point — between
// pods, between PDB retries, and during the eviction call itself.
// On cancel the function emits a final {Phase:"error", Err:"cancelled"}
// and closes out.
//
// The "best-effort cordon first" step matches kubectl drain. If the
// user has a watcher running it'll instantly reflect the cordon in
// the table; that's signal that something is happening even before
// the first eviction lands.
func (s *Supervisor) Drain(ctx context.Context, ctxName, node string, out chan<- DrainProgress) {
	defer close(out)

	send := func(ev DrainProgress) {
		ev.Context = ctxName
		ev.Node = node
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}

	cs, err := s.coreClient(ctxName)
	if err != nil {
		send(DrainProgress{Phase: "error", Err: err.Error()})
		return
	}

	// Best-effort cordon first. If this fails we surface the error
	// and bail — draining while still schedulable means new pods can
	// land on the node before we evict the existing ones.
	if r := s.Cordon(ctx, ctxName, node); !r.OK {
		send(DrainProgress{Phase: "error", Err: "cordon: " + r.Err})
		return
	}

	pods, err := listDrainablePods(ctx, cs, node)
	if err != nil {
		send(DrainProgress{Phase: "error", Err: "list pods: " + err.Error()})
		return
	}

	total := len(pods)
	send(DrainProgress{Phase: "starting", Total: total})

	done := 0
	for _, p := range pods {
		if err := ctx.Err(); err != nil {
			send(DrainProgress{Phase: "error", Err: "cancelled"})
			return
		}
		podRef := p.Namespace + "/" + p.Name
		send(DrainProgress{Phase: "evicting", Pod: podRef, Done: done, Total: total})

		if err := evictWithRetry(ctx, cs, p.Namespace, p.Name); err != nil {
			// "blocked" means we gave up on this specific pod after
			// repeated PDB rejections (or got some other persistent
			// error). The drain itself continues to the next pod —
			// reporting failure per-pod lets the user see exactly
			// what's stuck rather than aborting the whole run on
			// the first PDB.
			send(DrainProgress{Phase: "blocked", Pod: podRef, Done: done, Total: total, Err: trimError(err)})
			continue
		}
		done++
		send(DrainProgress{Phase: "evicted", Pod: podRef, Done: done, Total: total})
	}
	send(DrainProgress{Phase: "done", Done: done, Total: total})
}

// listDrainablePods returns the pods on the node that kubectl drain
// would also evict. Three buckets are skipped:
//
//   - Mirror pods (have the kubernetes.io/config.mirror annotation):
//     these are static pods owned by the kubelet, not by the
//     apiserver. Evicting them is a no-op; kubelet just recreates.
//
//   - DaemonSet-owned pods: kubectl drain skips these by default
//     (--ignore-daemonsets, which has been on by default since k8s
//     1.10ish). DS controllers will re-create them on the next
//     scheduling pass anyway.
//
//   - Already-completed pods (Phase == Succeeded or Failed): nothing
//     to evict.
func listDrainablePods(ctx context.Context, cs *kubernetes.Clientset, node string) ([]corev1.Pod, error) {
	all, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(all.Items))
	for _, p := range all.Items {
		if isMirrorPod(&p) {
			continue
		}
		if isDaemonSetOwned(&p) {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func isMirrorPod(p *corev1.Pod) bool {
	if p.Annotations == nil {
		return false
	}
	_, ok := p.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

func isDaemonSetOwned(p *corev1.Pod) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// evictWithRetry posts a policy/v1 Eviction for the named pod. The
// apiserver returns 429 TooManyRequests when a PodDisruptionBudget
// is blocking the eviction (the PDB still requires more healthy
// replicas than would remain). Backoff and retry up to
// drainPDBMaxRetries times — at 1/2/4/8/16s that's ~31s total per
// pod, which is long enough for a typical rolling-replacement to
// finish but short enough to keep the drain making progress.
//
// 404 is treated as success: somebody else (the kubelet, a
// controller) already removed the pod between our list and our
// evict — there's nothing left to do.
func evictWithRetry(ctx context.Context, cs *kubernetes.Clientset, ns, name string) error {
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		err := evictOnce(ctx, cs, ns, name)
		if err == nil {
			return nil
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if !apierrors.IsTooManyRequests(err) {
			return err
		}
		if attempt >= drainPDBMaxRetries {
			return fmt.Errorf("PDB blocked after %d retries", drainPDBMaxRetries)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return errors.New("cancelled")
		}
		backoff *= 2
	}
}

func evictOnce(ctx context.Context, cs *kubernetes.Clientset, ns, name string) error {
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	return cs.PolicyV1().Evictions(ns).Evict(ctx, ev)
}
