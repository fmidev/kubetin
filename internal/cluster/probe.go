// Package cluster runs per-context probe loops and writes results into
// the shared model.Store. No UI concerns live here.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/fmidev/kubetin/internal/kubeconfig"
	"github.com/fmidev/kubetin/internal/model"
)

// ProbeTimeout is the per-attempt deadline for a single /version probe.
// Aggressive on purpose: a hung exec-auth plugin must not stall the loop.
const ProbeTimeout = 5 * time.Second

// listAllOpts is the no-cache, no-pagination ListOptions we use for
// the cheap node-count probe.
func listAllOpts() metav1.ListOptions {
	return metav1.ListOptions{Limit: 500, ResourceVersion: "0"}
}

// Supervisor runs one probe loop per context, writing into store.
type Supervisor struct {
	contexts []string
	// refs maps unique context Name -> source file. Used so RestConfigFor
	// uses the file that actually owns the context's auth credentials,
	// avoiding the merge-collision problem on RKE2 configs that all
	// name their user "default".
	refs     map[string]contextSource
	store    *model.Store
	interval time.Duration

	hysteresis sync.Map // ctxName -> *reachAccumulator

	// scopes caches the *resolved* informer scope per context: "" means
	// cluster-wide (kubetin user has cluster-list-pods), non-empty means
	// the user is namespace-restricted and watchers must scope there.
	// Resolved lazily by ResolveScope and reused by every watcher and
	// the probe itself.
	scopes sync.Map // ctxName -> string
	// scopeMu serialises concurrent ResolveScope calls per context so
	// six watchers spinning up at once don't all fire the same probe.
	scopeMu sync.Map // ctxName -> *sync.Mutex
}

// contextSource pairs a kubeconfig file with the raw context name to
// use inside that file (which may differ from the Supervisor's view if
// kubetin disambiguated a duplicated name).
//
// Namespace is the kubeconfig context's `namespace:` field — a hint
// for kubectl shorthand, NOT an RBAC scope. microk8s pins `default`
// even for cluster-admins; we have to probe actual access (see
// ResolveScope) to know whether to treat it as a scope.
type contextSource struct {
	File      string
	RawName   string
	Config    *clientcmdapi.Config
	Namespace string // kubeconfig hint only; do not use as a scope directly
}

// NamespaceFor returns the *resolved* informer scope for ctxName, or
// "" if the context is cluster-scoped or hasn't been resolved yet.
// Callers that need a definitive answer (the watchers, on startup)
// should call ResolveScope with a clientset instead — this lookup is
// the cheap read-only path used after resolution and by the UI.
func (s *Supervisor) NamespaceFor(ctxName string) string {
	if v, ok := s.scopes.Load(ctxName); ok {
		return v.(string)
	}
	return ""
}

// ResolveScope returns the resolved informer scope for ctxName,
// performing one cluster-list probe the first time it's called.
//
// Resolution rule: try `pods("").List(limit=1)`. If allowed, the user
// has cluster-level pod-list and "" is the right scope. If it returns
// Forbidden/Unauthorized, fall back to the kubeconfig context's
// namespace hint. Any other error is treated as transient — we return
// the hint as a best-effort answer but do not cache, so the next
// caller will retry the probe.
//
// The decision is sticky: once a definitive answer is cached, every
// watcher and every probe tick uses it. This is what stops microk8s
// (kubeconfig hints `default`, user is cluster-admin) from being
// silently scoped to `default`.
func (s *Supervisor) ResolveScope(ctx context.Context, ctxName string, cs *kubernetes.Clientset) string {
	if v, ok := s.scopes.Load(ctxName); ok {
		return v.(string)
	}
	muV, _ := s.scopeMu.LoadOrStore(ctxName, &sync.Mutex{})
	mu := muV.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := s.scopes.Load(ctxName); ok {
		return v.(string)
	}
	ns, definitive := s.resolveScopeNow(ctx, ctxName, cs)
	if definitive {
		s.scopes.Store(ctxName, ns)
	}
	return ns
}

func (s *Supervisor) resolveScopeNow(parent context.Context, ctxName string, cs *kubernetes.Clientset) (string, bool) {
	hint := ""
	if src, ok := s.refs[ctxName]; ok {
		hint = src.Namespace
	}
	probeCtx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()
	_, err := cs.CoreV1().Pods("").List(probeCtx, metav1.ListOptions{Limit: 1, ResourceVersion: "0"})
	if err == nil {
		return "", true
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		// Cluster-list denied. If kubeconfig pinned a namespace, use
		// it; otherwise we have no fallback — return "" and let the
		// watcher surface the real RBAC error.
		return hint, true
	}
	// Network / timeout / unknown: best-effort fallback to hint, but
	// not cached — a retry can converge.
	return hint, false
}

// reachAccumulator implements light hysteresis: instant promotion to
// Healthy, but a non-healthy state must persist for two consecutive
// samples before it replaces the visible state. This stops the
// flickering between Connecting → Unreachable → Connecting that the
// W1 spike showed against slow clusters.
type reachAccumulator struct {
	visible      model.Reach
	pending      model.Reach
	pendingCount int
}

func (a *reachAccumulator) update(seen model.Reach) model.Reach {
	// First-ever sample (or seen state matches visible, or instant
	// promotion to Healthy): accept immediately. Hysteresis only
	// applies when demoting from an established non-Unknown state.
	if a.visible == model.ReachUnknown || seen == model.ReachHealthy || seen == a.visible {
		a.visible = seen
		a.pending = seen
		a.pendingCount = 0
		return a.visible
	}
	if seen == a.pending {
		a.pendingCount++
	} else {
		a.pending = seen
		a.pendingCount = 1
	}
	if a.pendingCount >= 2 {
		a.visible = a.pending
		a.pendingCount = 0
	}
	return a.visible
}

func (s *Supervisor) accumulator(ctxName string) *reachAccumulator {
	if v, ok := s.hysteresis.Load(ctxName); ok {
		return v.(*reachAccumulator)
	}
	a := &reachAccumulator{visible: model.ReachUnknown, pending: model.ReachUnknown}
	actual, _ := s.hysteresis.LoadOrStore(ctxName, a)
	return actual.(*reachAccumulator)
}

// New returns a Supervisor over the given Discovered kubeconfigs.
func New(d *kubeconfig.Discovered, store *model.Store, interval time.Duration) *Supervisor {
	refs := make(map[string]contextSource, len(d.Refs))
	for _, r := range d.Refs {
		refs[r.Name] = contextSource{
			File:      r.File,
			RawName:   r.RawName,
			Config:    d.Configs[r.File],
			Namespace: r.Namespace,
		}
	}
	contexts := make([]string, len(d.Contexts))
	copy(contexts, d.Contexts)
	return &Supervisor{
		contexts: contexts,
		refs:     refs,
		store:    store,
		interval: interval,
	}
}

// Run launches a probe goroutine per context and blocks until ctx is done.
// Each context is initialised in the store as Unknown immediately, so the
// first render has every cluster represented.
func (s *Supervisor) Run(ctx context.Context) {
	for _, name := range s.contexts {
		pf := model.ProbeFields{Reach: model.ReachUnknown}
		if src, ok := s.refs[name]; ok {
			pf.RawName = src.RawName
			pf.File = src.File
		}
		s.store.ApplyProbe(name, pf)
	}
	for _, name := range s.contexts {
		go s.runOne(ctx, name)
		go s.runMetricsLoop(ctx, name)
	}
	<-ctx.Done()
}

func (s *Supervisor) runOne(ctx context.Context, ctxName string) {
	// First probe immediately so we don't sit on Unknown for a full interval.
	s.probeOnce(ctx, ctxName)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.probeOnce(ctx, ctxName)
		}
	}
}

func (s *Supervisor) probeOnce(parent context.Context, ctxName string) {
	// Build a fresh probe-fields struct as we go. We deliberately do
	// NOT touch metrics-owned slots — ApplyProbe leaves them alone
	// in the store. Eliminates the lost-update race where a probe
	// snapshotted prev, then took 10s of API calls, then committed
	// over a metrics write that landed in between.
	pf := model.ProbeFields{LastProbe: time.Now()}

	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		pf.LastError = "kubeconfig: " + err.Error()
		s.commit(ctxName, pf, model.ReachAuthFailed)
		return
	}
	restCfg.Timeout = ProbeTimeout

	probeCtx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		pf.LastError = "client: " + err.Error()
		s.commit(ctxName, pf, model.ReachUnreachable)
		return
	}

	start := time.Now()
	v, err := serverVersion(probeCtx, clientset.Discovery())
	if err != nil {
		pf.LastError = trimError(err)
		s.commit(ctxName, pf, classify(err))
		return
	}
	pf.ServerVersion = v
	pf.ProbeLatency = time.Since(start)

	// Resolve once whether this context is cluster-scoped or
	// namespace-restricted. Cached on the supervisor so every watcher
	// and every subsequent probe tick reuses the answer. We MUST do
	// this before deciding to skip the node probe — the kubeconfig's
	// namespace hint alone is unreliable (microk8s pins "default"
	// even for cluster-admins).
	scopedNS := s.ResolveScope(probeCtx, ctxName, clientset)
	var res nodeProbeResult
	if scopedNS == "" {
		// Cluster-scoped flow: list nodes for count + allocatable.
		// RBAC failures here don't kill the cluster — they demote it
		// to Degraded.
		var nodeErr error
		res, nodeErr = s.probeNodes(probeCtx, clientset)
		if nodeErr != nil {
			pf.LastError = trimError(nodeErr)
		}
		pf.NodeCount = res.Count
		pf.NodeReady = res.ReadyCount
		pf.AllocCPUMilli = res.AllocCPUMilli
		pf.AllocMemBytes = res.AllocMemBytes
		if res.Reach == model.ReachDegraded && nodeErr == nil {
			pf.LastError = fmt.Sprintf("nodes %d/%d ready", res.ReadyCount, res.Count)
		}
	} else {
		// Scoped flow: assume Healthy until pod-access proves otherwise.
		// NodeCount = -1 signals "unknown / not available" to the UI so
		// it can hide the node bars and the Nodes view.
		res = nodeProbeResult{Reach: model.ReachHealthy, Count: -1}
		pf.NodeCount = -1
	}

	// Probe pod LIST. For cluster-scoped: confirms pods are visible
	// alongside nodes (the rke2-tj case where nodes worked but pods
	// 403'd). For namespace-scoped: this IS the liveness check.
	finalReach := res.Reach
	if finalReach == model.ReachHealthy {
		if perr := s.probePodAccess(probeCtx, ctxName, clientset); perr != nil {
			finalReach = model.ReachDegraded
			if pf.LastError == "" {
				pf.LastError = trimError(perr)
			}
		}
	}
	s.commit(ctxName, pf, finalReach)
}

// probePodAccess does a single LIST pods?limit=1 to confirm pod-list
// access. Scoped to the kubeconfig-context's default namespace when
// one is set — namespace-restricted users (typical OpenShift) will
// always 403 on the cluster-wide ("") form. The scope decision matches
// what the watcher loop will use, so a successful probe here implies
// the watcher will sync.
//
// RBAC failures surface as "rbac: list pods denied"; transient list
// failures as the raw error.
func (s *Supervisor) probePodAccess(ctx context.Context, ctxName string, clientset *kubernetes.Clientset) error {
	ns := s.NamespaceFor(ctxName)
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		_, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1, ResourceVersion: "0"})
		ch <- result{err: err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err == nil {
			return nil
		}
		if apierrors.IsForbidden(r.err) || apierrors.IsUnauthorized(r.err) {
			return fmt.Errorf("rbac: list pods denied")
		}
		return r.err
	}
}

// nodeProbeResult bundles what probeNodes derived from a single
// nodes.list call. Reach + err describe the call's success; count and
// AllocCPU/Mem are the aggregated allocatable resources.
type nodeProbeResult struct {
	Reach         model.Reach
	Count         int
	ReadyCount    int
	AllocCPUMilli int64
	AllocMemBytes int64
}

// probeNodes lists nodes and classifies the result. On success it
// also sums Allocatable CPU + memory across nodes so the sidebar can
// render utilisation bars without an extra API call.
func (s *Supervisor) probeNodes(ctx context.Context, clientset *kubernetes.Clientset) (nodeProbeResult, error) {
	type result struct {
		res nodeProbeResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		nodes, err := clientset.CoreV1().Nodes().List(ctx, listAllOpts())
		if err != nil {
			ch <- result{err: err}
			return
		}
		var cpu, mem int64
		var ready int
		for _, n := range nodes.Items {
			if c, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
				cpu += c.MilliValue()
			}
			if m, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
				mem += m.Value()
			}
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					ready++
					break
				}
			}
		}
		// Reach: all nodes ready → Healthy; some NotReady → Degraded.
		// Empty cluster (no nodes registered) is also Degraded — the
		// API works but the cluster has no work capacity.
		reach := model.ReachHealthy
		if len(nodes.Items) == 0 || ready < len(nodes.Items) {
			reach = model.ReachDegraded
		}
		ch <- result{res: nodeProbeResult{
			Reach:         reach,
			Count:         len(nodes.Items),
			ReadyCount:    ready,
			AllocCPUMilli: cpu,
			AllocMemBytes: mem,
		}}
	}()
	select {
	case <-ctx.Done():
		return nodeProbeResult{Reach: model.ReachDegraded, Count: -1}, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if apierrors.IsForbidden(r.err) || apierrors.IsUnauthorized(r.err) {
				return nodeProbeResult{Reach: model.ReachDegraded, Count: -1}, fmt.Errorf("rbac: list nodes denied")
			}
			return nodeProbeResult{Reach: model.ReachDegraded, Count: -1}, r.err
		}
		return r.res, nil
	}
}

// commit applies hysteresis and atomically merges probe-owned fields
// into the store. Metrics-owned slots are preserved by ApplyProbe so
// a concurrent metrics tick can't lose its update under us.
func (s *Supervisor) commit(ctxName string, pf model.ProbeFields, seen model.Reach) {
	if src, ok := s.refs[ctxName]; ok {
		pf.RawName = src.RawName
		pf.File = src.File
	}
	pf.Reach = s.accumulator(ctxName).update(seen)
	s.store.ApplyProbe(ctxName, pf)
}

// RestConfigFor builds a non-interactive rest.Config for the named
// context. It loads from the SOURCE FILE for that context — never the
// merged view — so duplicate user names across files don't poison
// each other's auth.
func (s *Supervisor) RestConfigFor(ctxName string) (*rest.Config, error) {
	src, ok := s.refs[ctxName]
	if !ok {
		return nil, fmt.Errorf("unknown context %q", ctxName)
	}
	if src.Config == nil {
		return nil, fmt.Errorf("context %q has no parsed config", ctxName)
	}
	cc := clientcmd.NewNonInteractiveClientConfig(
		*src.Config, src.RawName, &clientcmd.ConfigOverrides{}, nil,
	)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS = 20
	cfg.Burst = 40
	cfg.WarningHandler = rest.NoWarnings{}
	return cfg, nil
}

// serverVersion calls /version with the supplied context.
// client-go's discovery client doesn't honour ctx directly on this call
// (the legacy ServerVersion signature), so we run it in a goroutine and
// race it against ctx.Done(). The HTTP timeout in restCfg.Timeout still
// caps the underlying request.
func serverVersion(ctx context.Context, d discovery.DiscoveryInterface) (string, error) {
	type result struct {
		v   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		info, err := d.ServerVersion()
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{v: info.GitVersion}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.v, r.err
	}
}

func classify(err error) model.Reach {
	switch {
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return model.ReachAuthFailed
	case isNetworkError(err):
		return model.ReachUnreachable
	case errors.Is(err, context.DeadlineExceeded):
		return model.ReachUnreachable
	}
	// Unknown error: probably HTTP-level but reachable. Call it degraded.
	if strings.Contains(strings.ToLower(err.Error()), "unauthorized") ||
		strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		return model.ReachAuthFailed
	}
	return model.ReachDegraded
}

func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"no such host", "i/o timeout", "connection refused",
		"network is unreachable", "tls:", "x509:", "eof",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func trimError(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return fmt.Sprintf("%s", s)
}
