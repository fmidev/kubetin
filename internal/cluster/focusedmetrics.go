package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

// FocusedInterval is the metrics-server poll cadence for the
// currently-focused cluster. metrics-server resolves new readings
// every ~15-30s, so 15s is the upper end of usefulness.
const FocusedInterval = 15 * time.Second

// PodMetric is the per-pod resource snapshot the UI renders.
type PodMetric struct {
	UID       types.UID
	Namespace string
	Name      string
	CPUMilli  int64 // sum of container usage.cpu (millicores)
	MemBytes  int64 // sum of container usage.memory (bytes)
}

// NodeMetric is the per-node resource snapshot.
type NodeMetric struct {
	Name     string
	UID      types.UID
	CPUMilli int64
	MemBytes int64
}

// MetricsSnapshot is one tick's worth of pod + node metrics for the
// focused cluster. We send the whole snapshot rather than per-pod
// deltas because metrics-server is poll-based and we already pay the
// cost of the full list call.
type MetricsSnapshot struct {
	Context string
	Pods    []PodMetric
	Nodes   []NodeMetric
	At      time.Time
	OK      bool   // false if the call failed (no metrics-server, RBAC, etc)
	Error   string // human-readable when !OK
}

// FocusedMetricsPoller polls metrics for one cluster context.
type FocusedMetricsPoller struct {
	Context       string
	Out           chan MetricsSnapshot
	DroppedEvents atomic.Uint64
}

// NewFocusedMetricsPoller constructs a poller over a buffered channel.
func NewFocusedMetricsPoller(ctxName string, cap int) *FocusedMetricsPoller {
	return &FocusedMetricsPoller{
		Context: ctxName,
		Out:     make(chan MetricsSnapshot, cap),
	}
}

// Run blocks until ctx is done, polling every FocusedInterval.
func (p *FocusedMetricsPoller) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(p.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	restCfg.Timeout = 5 * time.Second
	mc, err := metricsclientset.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("metrics client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("core client: %w", err)
	}

	scopedNS := sup.ResolveScope(ctx, p.Context, cs)

	t := time.NewTicker(FocusedInterval)
	defer t.Stop()
	p.tick(ctx, mc, scopedNS) // prompt first read
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.tick(ctx, mc, scopedNS)
		}
	}
}

func (p *FocusedMetricsPoller) tick(parent context.Context, mc *metricsclientset.Clientset, scopedNS string) {
	pollCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	snap := MetricsSnapshot{Context: p.Context, At: time.Now()}

	// Pods: scope to the kubeconfig namespace when set; cluster-wide
	// otherwise. PodMetricses("") is cluster-scoped and 403s for
	// namespace-restricted users on the same RBAC rules as core/pods.
	pmList, perr := mc.MetricsV1beta1().PodMetricses(scopedNS).List(pollCtx, metav1.ListOptions{})
	if perr != nil {
		snap.OK = false
		snap.Error = perr.Error()
		p.send(snap)
		return
	}
	for _, pm := range pmList.Items {
		var cpu, mem int64
		for _, c := range pm.Containers {
			if v, ok := c.Usage["cpu"]; ok {
				cpu += v.MilliValue()
			}
			if v, ok := c.Usage["memory"]; ok {
				mem += v.Value()
			}
		}
		snap.Pods = append(snap.Pods, PodMetric{
			UID:       pm.UID,
			Namespace: pm.Namespace,
			Name:      pm.Name,
			CPUMilli:  cpu,
			MemBytes:  mem,
		})
	}

	// Nodes — only attempt when not namespace-scoped. NodeMetricses
	// is cluster-scoped and 403s for restricted users; treating that
	// as a hard fail would also drop the per-pod metrics we just got.
	if scopedNS != "" {
		snap.OK = true
		p.send(snap)
		return
	}
	nmList, nerr := mc.MetricsV1beta1().NodeMetricses().List(pollCtx, metav1.ListOptions{})
	if nerr != nil {
		// Mirror the pod-list error path above: surface the failure
		// instead of returning a half-empty snapshot with OK=true,
		// which the UI would interpret as "node metrics are healthy
		// and the cluster simply has zero nodes".
		snap.OK = false
		snap.Error = nerr.Error()
		p.send(snap)
		return
	}
	for _, nm := range nmList.Items {
		var cpu, mem int64
		if v, ok := nm.Usage["cpu"]; ok {
			cpu += v.MilliValue()
		}
		if v, ok := nm.Usage["memory"]; ok {
			mem += v.Value()
		}
		snap.Nodes = append(snap.Nodes, NodeMetric{
			Name:     nm.Name,
			UID:      nm.UID,
			CPUMilli: cpu,
			MemBytes: mem,
		})
	}

	snap.OK = true
	p.send(snap)
}

func (p *FocusedMetricsPoller) send(snap MetricsSnapshot) {
	select {
	case p.Out <- snap:
	default:
		p.DroppedEvents.Add(1)
	}
}
