// Package cluster runs per-context probe loops and writes results into
// the shared model.Store. No UI concerns live here.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	"github.com/fmidev/kubetin/internal/kubeconfig"
	"github.com/fmidev/kubetin/internal/model"
)

// ProbeTimeout is the deadline for a single API call in a probe round,
// applied per call rather than to the round as a whole. Aggressive on
// purpose: a hung exec-auth plugin must not stall the loop.
//
// It was once the budget for the whole round, back when the round was
// one /version call. It has since grown to four, and four round trips
// over a high-latency link overrun 5s while every individual call
// succeeds: 1.1s + 1.1s + 2.2s + 1.0s measured against a cluster whose
// nodes were all Ready, which the probe then committed as Degraded,
// "context deadline exceeded". Distance should cost latency, not
// correctness.
//
// A var rather than a const only so tests can shrink it; nothing
// mutates it at runtime.
var ProbeTimeout = 5 * time.Second

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
// the cheap read-only path.
//
// The two "" answers are indistinguishable here, which is a trap: a
// scope call that failed transiently is deliberately not cached, so a
// namespace-restricted context reads as cluster-scoped until a probe
// succeeds. Anything that would act on that "" — listing cluster-wide,
// say — must take the scope the round actually resolved instead.
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
		pf := model.NewProbeFields()
		pf.Reach = model.ReachUnknown
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
	pf := model.NewProbeFields()
	pf.LastProbe = time.Now()

	// prev is stable for the whole round — nothing commits before we
	// do. Rounds abandoned before measuring the health signals carry
	// them forward instead of flapping the fleet dashboard to
	// "unknown" on every transient failure; reach hysteresis would
	// keep the badge steady through the same blip.
	prev, _ := s.store.Get(ctxName)

	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		pf.LastError = "kubeconfig: " + err.Error()
		carryHealth(&pf, prev)
		s.commit(ctxName, pf, model.ReachAuthFailed)
		return
	}
	// Bounds each individual request the same way the per-call contexts
	// below do, including the ones client-go retries internally.
	restCfg.Timeout = ProbeTimeout

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		pf.LastError = "client: " + err.Error()
		carryHealth(&pf, prev)
		s.commit(ctxName, pf, model.ReachUnreachable)
		return
	}

	start := time.Now()
	versionCtx, cancelVersion := context.WithTimeout(parent, ProbeTimeout)
	v, err := serverVersion(versionCtx, clientset.Discovery())
	cancelVersion()
	if err != nil {
		pf.LastError = trimError(err)
		carryHealth(&pf, prev)
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
	// No wrapper deadline: resolveScopeNow applies its own ProbeTimeout,
	// which is what every watcher already relies on.
	scopedNS := s.ResolveScope(parent, ctxName, clientset)
	var res nodeProbeResult
	if scopedNS == "" {
		// Cluster-scoped flow: list nodes for count + allocatable.
		// RBAC failures here don't kill the cluster — they demote it
		// to Degraded.
		var nodeErr error
		nodeCtx, cancelNodes := context.WithTimeout(parent, ProbeTimeout)
		res, nodeErr = s.probeNodes(nodeCtx, clientset)
		cancelNodes()
		if isTimeout(nodeErr) {
			// /version already answered, so the API is up and this is a
			// slow path, not a sick cluster. Committing res here would
			// blank the counts to "unknown", drop the allocatable totals
			// the sidebar bars are drawn from, and paint Degraded — the
			// false alarm a relayed link raises every time it stalls.
			// Carry the last known detail forward and leave reach be.
			pf.NodeCount = prev.NodeCount
			pf.NodeReady = prev.NodeReady
			pf.AllocCPUMilli = prev.AllocCPUMilli
			pf.AllocMemBytes = prev.AllocMemBytes
			pf.NodesNotReadyNames = prev.NodesNotReadyNames
			pf.NodesMemPressure = prev.NodesMemPressure
			pf.NodesDiskPressure = prev.NodesDiskPressure
			pf.NodesPIDPressure = prev.NodesPIDPressure
			pf.NodesCordoned = prev.NodesCordoned
			pf.NodesCordonedReady = prev.NodesCordonedReady
			pf.NodesPressureNames = prev.NodesPressureNames
			if prev.ServerVersion == "" {
				// No round has ever completed, so there is no detail to
				// carry: -1 is the model's "unknown", which the UI
				// already renders as "—". Keyed on the version rather
				// than on a zero count, because a cluster that really
				// does list zero nodes has a zero count worth keeping.
				pf.NodeCount = -1
			}
			res.Reach = prev.Reach
			if res.Reach != model.ReachHealthy && res.Reach != model.ReachDegraded {
				res.Reach = model.ReachHealthy
			}
			pf.LastError = "node list timed out; showing last known"
		} else {
			if nodeErr != nil {
				pf.LastError = trimError(nodeErr)
			}
			pf.NodeCount = res.Count
			pf.NodeReady = res.ReadyCount
			pf.AllocCPUMilli = res.AllocCPUMilli
			pf.AllocMemBytes = res.AllocMemBytes
			if nodeErr == nil {
				pf.NodesNotReadyNames = res.NotReadyNames
				pf.NodesMemPressure = res.MemPressure
				pf.NodesDiskPressure = res.DiskPressure
				pf.NodesPIDPressure = res.PIDPressure
				pf.NodesCordoned = res.Cordoned
				pf.NodesCordonedReady = res.CordonedReady
				pf.NodesPressureNames = res.PressureNames
			}
			if res.Reach == model.ReachDegraded && nodeErr == nil {
				pf.LastError = fmt.Sprintf("nodes %d/%d ready", res.ReadyCount, res.Count)
			}
		}
	} else {
		// Scoped flow: assume Healthy until pod-access proves otherwise.
		// NodeCount = -1 signals "unknown / not available" to the UI so
		// it can hide the node bars and the Nodes view.
		res = nodeProbeResult{Reach: model.ReachHealthy, Count: -1}
		pf.NodeCount = -1
	}

	// Fleet-health workload signals. Run regardless of node-level
	// degradation — a degraded cluster is exactly where these matter —
	// but only after /version proved the API answers at all.
	wh := s.probeWorkloadHealth(parent, scopedNS, clientset)
	switch wh.pods.outcome {
	case signalOK:
		pf.PodsPending = wh.pods.pending
		pf.PodsFailed = wh.pods.failed
		pf.PodsUnknownPhase = wh.pods.unknown
	case signalTimeout:
		pf.PodsPending = prev.PodsPending
		pf.PodsFailed = prev.PodsFailed
		pf.PodsUnknownPhase = prev.PodsUnknownPhase
	}
	switch wh.deploys.outcome {
	case signalOK:
		pf.DeploysTotal = wh.deploys.total
		pf.DeploysDegraded = wh.deploys.degraded
		pf.DeploysZeroReady = wh.deploys.zeroReady
		pf.DegradedDeployNames = wh.deploys.names
	case signalTimeout:
		pf.DeploysTotal = prev.DeploysTotal
		pf.DeploysDegraded = prev.DeploysDegraded
		pf.DeploysZeroReady = prev.DeploysZeroReady
		pf.DegradedDeployNames = prev.DegradedDeployNames
	}
	switch wh.events.outcome {
	case signalOK:
		pf.WarnEvents15m = wh.events.warn15m
	case signalTimeout:
		pf.WarnEvents15m = prev.WarnEvents15m
	}

	// Probe pod LIST. For cluster-scoped: confirms pods are visible
	// alongside nodes (the rke2-tj case where nodes worked but pods
	// 403'd). For namespace-scoped: this IS the liveness check. Runs
	// for Degraded too now that it also carries the pod total, but a
	// denial only ever demotes from Healthy.
	finalReach := res.Reach
	if finalReach == model.ReachHealthy || finalReach == model.ReachDegraded {
		podCtx, cancelPods := context.WithTimeout(parent, ProbeTimeout)
		total, perr := s.probePodSummary(podCtx, scopedNS, clientset)
		cancelPods()
		switch {
		case isTimeout(perr):
			// Abandoning the call is not a finding — and it is not a
			// clean bill of health either. Leaving finalReach at the
			// node result would let one stalled check clear a standing
			// pod-access denial: a cluster with a real 403 would flip
			// to Healthy the first time the link hiccuped. Carry the
			// previous verdict instead, as the node path does.
			//
			// prev.Reach cannot say *which* check degraded the cluster,
			// so a degradation the nodes have since recovered from
			// lingers one round when the pod check also times out. It
			// clears on the next conclusive round, and the alternative
			// clears real findings, which is worse.
			if prev.Reach == model.ReachDegraded {
				finalReach = model.ReachDegraded
			}
			if pf.LastError == "" {
				pf.LastError = "pod access check timed out"
			}
			pf.PodsTotal = prev.PodsTotal
		case perr != nil:
			finalReach = model.ReachDegraded
			if pf.LastError == "" {
				pf.LastError = trimError(perr)
			}
		default:
			pf.PodsTotal = total
		}
	}
	s.commit(ctxName, pf, finalReach)
}

// probePodSummary does a single LIST pods?limit=1 that doubles as the
// pod-access liveness check and a best-effort total pod count. The
// call deliberately omits ResourceVersion "0": served from etcd with
// pagination the response carries remainingItemCount, while the watch
// cache sets neither it nor honours Limit. remainingItemCount is
// documented best-effort, so the total is -1 whenever the server
// withholds it.
//
// ns is the scope this round resolved — the kubeconfig
// context's default namespace when one is set, since namespace-
// restricted users (typical OpenShift) always 403 on the cluster-wide
// ("") form. The scope decision matches what the watcher loop will
// use, so a successful probe here implies the watcher will sync.
//
// Taking ns as an argument rather than re-reading NamespaceFor is the
// point: NamespaceFor only knows the *cached* scope, and ResolveScope
// deliberately does not cache a transient failure. A scope call that
// timed out would leave NamespaceFor answering "" for a restricted
// user, sending this probe cluster-wide into a certain 403 — turning a
// stalled call into a confident demotion, the exact thing the timeout
// handling above exists to prevent.
//
// RBAC failures surface as "rbac: list pods denied"; transient list
// failures as the raw error.
func (s *Supervisor) probePodSummary(ctx context.Context, ns string, clientset *kubernetes.Clientset) (int, error) {
	type result struct {
		total int
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		list, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			ch <- result{total: -1, err: err}
			return
		}
		total := len(list.Items)
		switch {
		case list.Continue == "" && list.RemainingItemCount == nil:
			// Nothing beyond this page: the count is exact.
		case list.RemainingItemCount != nil:
			total += int(*list.RemainingItemCount)
		default:
			total = -1
		}
		ch <- result{total: total}
	}()
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case r := <-ch:
		if r.err == nil {
			return r.total, nil
		}
		if apierrors.IsForbidden(r.err) || apierrors.IsUnauthorized(r.err) {
			return -1, fmt.Errorf("rbac: list pods denied")
		}
		return -1, r.err
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
	NotReadyNames []string
	MemPressure   int
	DiskPressure  int
	PIDPressure   int
	Cordoned      int
	CordonedReady int
	PressureNames []string
}

// healthNameCap bounds the node/deployment name samples carried in the
// health fields — enough to name culprits in an alert line, not a dump.
const healthNameCap = 3

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
		var health nodeProbeResult
		for _, n := range nodes.Items {
			if c, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
				cpu += c.MilliValue()
			}
			if m, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
				mem += m.Value()
			}
			var nodeReady, pressured bool
			for _, cond := range n.Status.Conditions {
				if cond.Status != corev1.ConditionTrue {
					continue
				}
				switch cond.Type {
				case corev1.NodeReady:
					nodeReady = true
				case corev1.NodeMemoryPressure:
					health.MemPressure++
					pressured = true
				case corev1.NodeDiskPressure:
					health.DiskPressure++
					pressured = true
				case corev1.NodePIDPressure:
					health.PIDPressure++
					pressured = true
				}
			}
			if nodeReady {
				ready++
			} else if len(health.NotReadyNames) < healthNameCap {
				health.NotReadyNames = append(health.NotReadyNames, n.Name)
			}
			if pressured && len(health.PressureNames) < healthNameCap {
				health.PressureNames = append(health.PressureNames, n.Name)
			}
			if n.Spec.Unschedulable {
				health.Cordoned++
				if nodeReady {
					health.CordonedReady++
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
		health.Reach = reach
		health.Count = len(nodes.Items)
		health.ReadyCount = ready
		health.AllocCPUMilli = cpu
		health.AllocMemBytes = mem
		ch <- result{res: health}
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

// signalOutcome classifies one optional health-list call. These calls
// never touch reach or LastError — they are fleet-dashboard signals,
// not liveness checks.
type signalOutcome uint8

const (
	signalUnknown signalOutcome = iota // denied or errored: the -1 sentinel stands
	signalOK
	signalTimeout // abandoned call: carry the previous values forward
)

type workloadHealth struct {
	pods    podPhaseSignal
	deploys deploySignal
	events  eventSignal
}

type podPhaseSignal struct {
	outcome                  signalOutcome
	pending, failed, unknown int
}

type deploySignal struct {
	outcome                    signalOutcome
	total, degraded, zeroReady int
	names                      []string
}

type eventSignal struct {
	outcome signalOutcome
	warn15m int
}

// warnEventWindow is how far back a Warning event still counts toward
// the fleet "warning events" signal.
const warnEventWindow = 15 * time.Minute

// probeWorkloadHealth gathers the fleet-dashboard workload signals —
// non-green pods, degraded deployments, recent warning events — with
// one field-selected list each, run concurrently so the round grows by
// at most one ProbeTimeout of wall clock.
func (s *Supervisor) probeWorkloadHealth(parent context.Context, ns string, cs *kubernetes.Clientset) workloadHealth {
	var wh workloadHealth
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); wh.pods = probeNonGreenPods(parent, ns, cs) }()
	go func() { defer wg.Done(); wh.deploys = probeDeployHealth(parent, ns, cs) }()
	go func() { defer wg.Done(); wh.events = probeWarnEvents(parent, ns, cs) }()
	wg.Wait()
	return wh
}

// carryHealth copies every fleet-health field forward from the last
// committed state, for rounds abandoned before any of them could be
// measured.
func carryHealth(pf *model.ProbeFields, prev model.ClusterState) {
	pf.NodesNotReadyNames = prev.NodesNotReadyNames
	pf.NodesMemPressure = prev.NodesMemPressure
	pf.NodesDiskPressure = prev.NodesDiskPressure
	pf.NodesPIDPressure = prev.NodesPIDPressure
	pf.NodesCordoned = prev.NodesCordoned
	pf.NodesCordonedReady = prev.NodesCordonedReady
	pf.NodesPressureNames = prev.NodesPressureNames
	pf.PodsTotal = prev.PodsTotal
	pf.PodsPending = prev.PodsPending
	pf.PodsFailed = prev.PodsFailed
	pf.PodsUnknownPhase = prev.PodsUnknownPhase
	pf.DeploysTotal = prev.DeploysTotal
	pf.DeploysDegraded = prev.DeploysDegraded
	pf.DeploysZeroReady = prev.DeploysZeroReady
	pf.DegradedDeployNames = prev.DegradedDeployNames
	pf.WarnEvents15m = prev.WarnEvents15m
}

// healthErrOutcome maps a health-list error to its outcome: abandoned
// calls carry forward, denials leave the "unknown" sentinel quietly,
// anything else leaves it with a breadcrumb.
func healthErrOutcome(what string, err error) signalOutcome {
	if isTimeout(err) {
		return signalTimeout
	}
	if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) {
		klog.Warningf("health probe: %s list failed: %v", what, err)
	}
	return signalUnknown
}

// probeNonGreenPods counts pods stuck outside Running/Succeeded. The
// field selector keeps the response proportional to how broken the
// cluster is rather than how big. Blind spot: a crash-looping pod
// usually reports Phase=Running and is invisible here — deployment
// readiness and warning events cover that side.
func probeNonGreenPods(parent context.Context, ns string, cs *kubernetes.Clientset) podPhaseSignal {
	ctx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()
	type result struct {
		list *corev1.PodList
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector:   "status.phase!=Running,status.phase!=Succeeded",
			Limit:           500,
			ResourceVersion: "0",
		})
		ch <- result{list: list, err: err}
	}()
	select {
	case <-ctx.Done():
		return podPhaseSignal{outcome: signalTimeout}
	case r := <-ch:
		if r.err != nil {
			return podPhaseSignal{outcome: healthErrOutcome("pods", r.err)}
		}
		sig := podPhaseSignal{outcome: signalOK}
		for _, p := range r.list.Items {
			switch p.Status.Phase {
			case corev1.PodPending:
				sig.pending++
			case corev1.PodFailed:
				sig.failed++
			default:
				sig.unknown++
			}
		}
		return sig
	}
}

func probeDeployHealth(parent context.Context, ns string, cs *kubernetes.Clientset) deploySignal {
	ctx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()
	type result struct {
		list *appsv1.DeploymentList
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		list, err := cs.AppsV1().Deployments(ns).List(ctx, listAllOpts())
		ch <- result{list: list, err: err}
	}()
	select {
	case <-ctx.Done():
		return deploySignal{outcome: signalTimeout}
	case r := <-ch:
		if r.err != nil {
			return deploySignal{outcome: healthErrOutcome("deployments", r.err)}
		}
		sig := deploySignal{outcome: signalOK, total: len(r.list.Items)}
		type degraded struct {
			label string
			ratio float64
		}
		var worst []degraded
		for _, d := range r.list.Items {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if desired == 0 || d.Status.ReadyReplicas >= desired {
				continue
			}
			sig.degraded++
			// Full-outage signal keyed on Ready, not Available:
			// readiness is what gates endpoints, and with
			// minReadySeconds a healthy rollout sits at
			// available=0 while its ready pods already serve.
			if d.Status.ReadyReplicas == 0 {
				sig.zeroReady++
			}
			worst = append(worst, degraded{
				label: fmt.Sprintf("%s/%s %d/%d", d.Namespace, d.Name, d.Status.ReadyReplicas, desired),
				ratio: float64(d.Status.ReadyReplicas) / float64(desired),
			})
		}
		sort.Slice(worst, func(i, j int) bool {
			if worst[i].ratio != worst[j].ratio {
				return worst[i].ratio < worst[j].ratio
			}
			return worst[i].label < worst[j].label
		})
		for i := 0; i < len(worst) && i < healthNameCap; i++ {
			sig.names = append(sig.names, worst[i].label)
		}
		return sig
	}
}

func probeWarnEvents(parent context.Context, ns string, cs *kubernetes.Clientset) eventSignal {
	ctx, cancel := context.WithTimeout(parent, ProbeTimeout)
	defer cancel()
	type result struct {
		list *corev1.EventList
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		list, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector:   "type=Warning",
			Limit:           500,
			ResourceVersion: "0",
		})
		ch <- result{list: list, err: err}
	}()
	select {
	case <-ctx.Done():
		return eventSignal{outcome: signalTimeout}
	case r := <-ch:
		if r.err != nil {
			return eventSignal{outcome: healthErrOutcome("events", r.err)}
		}
		sig := eventSignal{outcome: signalOK}
		cutoff := time.Now().Add(-warnEventWindow)
		for _, e := range r.list.Items {
			// Freshness chain matches the event watcher: batch events
			// carry eventTime, aggregated ones lastTimestamp.
			last := e.LastTimestamp.Time
			if last.IsZero() {
				last = e.EventTime.Time
			}
			if last.IsZero() {
				last = e.CreationTimestamp.Time
			}
			if last.After(cutoff) {
				sig.warn15m++
			}
		}
		return sig
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

// isTimeout reports whether err is us giving up on a call rather than
// the cluster answering. The distinction matters: a deadline we chose
// is not a finding about the cluster's health. Over a relayed link a
// single list can stall for 10s while the API server answers the same
// query in 50ms when asked locally.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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
