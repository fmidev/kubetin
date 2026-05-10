// Per-cluster pod network rate poller.
//
// Why this exists separately from FocusedMetricsPoller: metrics-server
// only carries cpu/memory. Network counters live in cAdvisor, which
// runs inside every kubelet and exposes Prometheus-format metrics on
// /metrics/cadvisor. We reach those endpoints through the apiserver's
// node-proxy facility (`/api/v1/nodes/{name}/proxy/metrics/cadvisor`)
// because it inherits the kubeconfig auth — no need for a separate
// connection or a Prometheus deployment.
//
// Cost shape: each kubelet returns ~0.5–2 MiB of cAdvisor metrics per
// scrape. We sample every NetworkInterval (15s) and parse the response
// inline. On a 20-node cluster that's ~30 MiB/min of parsing — fine
// for a single user-driven CLI but worth knowing if we ever go
// "always-on" against many large clusters.
//
// RBAC: requires `get` on `nodes/proxy`. Many users won't have that;
// the snapshot then carries OK=false and the UI hides network panels
// silently. There is no per-pod aggregate available from less
// privileged endpoints, so this is a hard requirement.
//
// Rates: cAdvisor counters are monotonic since container start. We
// remember the previous sample per (pod, namespace) and per node and
// emit BytesPerSec = max(0, (cur - prev) / dt). Restarts/wraps yield
// negative deltas, which we clamp to zero — better than emitting a
// huge spike.
package cluster

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// NetworkInterval is the cAdvisor scrape cadence per node. Lower would
// pay parse cost more often without producing more interesting data —
// kubelet's internal cAdvisor housekeeping runs on a similar period.
const NetworkInterval = 15 * time.Second

// PodNetwork is a per-pod rate snapshot.
type PodNetwork struct {
	Namespace     string
	Name          string
	RXBytesPerSec int64
	TXBytesPerSec int64
}

// ClusterNetwork is the cluster-aggregate rate (sum of all pods).
type ClusterNetwork struct {
	RXBytesPerSec int64
	TXBytesPerSec int64
}

// NetworkSnapshot is one tick's worth of rates.
type NetworkSnapshot struct {
	Context string
	Pods    []PodNetwork
	Cluster ClusterNetwork
	At      time.Time
	OK      bool
	Error   string
}

// podKey identifies a pod across samples. We don't have UID from
// cAdvisor labels, so namespace/name is the join key — collisions
// across recreates are tolerable because a recreated pod resets its
// counters and the rate clamps to zero.
type podKey struct{ ns, name string }

// counterSample stores a pair (value, time) so the next scrape can
// derive a rate without re-walking history.
type counterSample struct {
	rx, tx int64
	at     time.Time
}

// NetworkPoller scrapes cAdvisor on every node of one cluster context
// and emits per-pod + aggregate rates. Designed to mirror the
// FocusedMetricsPoller lifecycle so main can spawn one alongside it.
type NetworkPoller struct {
	Context       string
	Out           chan NetworkSnapshot
	DroppedEvents atomic.Uint64

	mu   sync.Mutex
	prev map[podKey]counterSample
}

// NewNetworkPoller returns a poller. cap is the channel buffer.
func NewNetworkPoller(ctxName string, cap int) *NetworkPoller {
	return &NetworkPoller{
		Context: ctxName,
		Out:     make(chan NetworkSnapshot, cap),
		prev:    map[podKey]counterSample{},
	}
}

// Run blocks until ctx is done.
func (p *NetworkPoller) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(p.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	// Generous per-scrape timeout: parse + walk + multiplied by node
	// count happens inside one tick. Kubelet proxy can be slow under
	// load.
	restCfg.Timeout = 0
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	// Namespace-restricted users can't list nodes or hit nodes/proxy.
	// Skip the poller entirely instead of 403'ing every NetworkInterval.
	// Scope is determined by probing actual cluster-list access, not
	// the kubeconfig hint.
	if sup.ResolveScope(ctx, p.Context, cs) != "" {
		klog.Infof("networkpoll[%s]: skipped (namespace-scoped context)", p.Context)
		<-ctx.Done()
		return nil
	}

	t := time.NewTicker(NetworkInterval)
	defer t.Stop()
	p.tick(ctx, cs)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.tick(ctx, cs)
		}
	}
}

func (p *NetworkPoller) tick(parent context.Context, cs *kubernetes.Clientset) {
	tickCtx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	snap := NetworkSnapshot{Context: p.Context, At: time.Now()}

	nodes, err := cs.CoreV1().Nodes().List(tickCtx, metav1.ListOptions{})
	if err != nil {
		snap.Error = "list nodes: " + err.Error()
		p.send(snap)
		return
	}
	if len(nodes.Items) == 0 {
		snap.OK = true // no nodes is not an error, just nothing to report
		p.send(snap)
		return
	}

	// Aggregate cumulative counters per pod across all nodes. cAdvisor
	// reports each pod's network on the node where it runs, so summing
	// across nodes is correct (not double-counting).
	cur := map[podKey]counterSample{}
	now := time.Now()
	scraped := 0
	for _, n := range nodes.Items {
		// Only scrape Ready nodes — a NotReady kubelet either won't
		// answer or will time out, dragging the whole tick down.
		if !nodeReady(n) {
			continue
		}
		body, err := scrapeKubelet(tickCtx, cs, n.Name)
		if err != nil {
			klog.V(2).Infof("net[%s]: scrape %s: %v", p.Context, n.Name, err)
			continue
		}
		scraped++
		parseCAdvisor(body, now, cur)
	}

	if scraped == 0 {
		snap.Error = "no nodes scraped (RBAC nodes/proxy missing, or all nodes NotReady)"
		p.send(snap)
		return
	}

	// Rate calculation against last tick. First-ever sample emits
	// zeros; subsequent samples use real deltas.
	p.mu.Lock()
	prev := p.prev
	p.prev = cur
	p.mu.Unlock()

	var clusterRX, clusterTX int64
	for k, c := range cur {
		var rx, tx int64
		if pv, ok := prev[k]; ok {
			dt := c.at.Sub(pv.at).Seconds()
			if dt > 0 {
				rx = clampRate(c.rx, pv.rx, dt)
				tx = clampRate(c.tx, pv.tx, dt)
			}
		}
		clusterRX += rx
		clusterTX += tx
		snap.Pods = append(snap.Pods, PodNetwork{
			Namespace:     k.ns,
			Name:          k.name,
			RXBytesPerSec: rx,
			TXBytesPerSec: tx,
		})
	}
	snap.Cluster = ClusterNetwork{RXBytesPerSec: clusterRX, TXBytesPerSec: clusterTX}
	snap.OK = true
	p.send(snap)
}

func clampRate(cur, prev int64, dt float64) int64 {
	d := cur - prev
	if d < 0 {
		// Counter reset (pod restart, kubelet restart, container
		// recreate). We can't tell true rate, so emit zero rather
		// than a misleading huge number.
		return 0
	}
	return int64(float64(d) / dt)
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// scrapeKubelet pulls /metrics/cadvisor through the apiserver proxy.
// Returns the raw exposition body so the parser can stream it.
func scrapeKubelet(ctx context.Context, cs *kubernetes.Clientset, node string) ([]byte, error) {
	return cs.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/nodes/" + node + "/proxy/metrics/cadvisor").
		DoRaw(ctx)
}

// parseCAdvisor extracts container_network_{receive,transmit}_bytes_total
// per (namespace, pod) and folds the readings into out, summing across
// network interfaces. Lines we don't care about are skipped on the
// fast path (string prefix check before any label parsing).
//
// Why we don't pull in github.com/prometheus/common/expfmt: it would
// add a sizable dep tree for a 30-line parser. We only need two
// metric names and four labels (namespace, pod, name=, image=). The
// exposition format guarantees one sample per line, value as the
// last whitespace-separated field, and labels in {} braces — that's
// trivial to parse safely.
func parseCAdvisor(body []byte, at time.Time, out map[podKey]counterSample) {
	const (
		rxPrefix = "container_network_receive_bytes_total{"
		txPrefix = "container_network_transmit_bytes_total{"
	)
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	// cAdvisor lines can be long when there are many labels.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		var isRX bool
		var rest string
		switch {
		case strings.HasPrefix(line, rxPrefix):
			isRX = true
			rest = line[len(rxPrefix):]
		case strings.HasPrefix(line, txPrefix):
			isRX = false
			rest = line[len(txPrefix):]
		default:
			continue
		}
		// rest is now: <labels>} <value> [<timestamp>]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			continue
		}
		labels := rest[:end]
		valStr := strings.TrimSpace(rest[end+1:])
		if i := strings.IndexByte(valStr, ' '); i >= 0 {
			valStr = valStr[:i] // drop optional timestamp
		}
		ns := labelValue(labels, "namespace")
		pod := labelValue(labels, "pod")
		if ns == "" || pod == "" {
			// cAdvisor emits some non-pod cgroups (system slices); skip.
			continue
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		k := podKey{ns: ns, name: pod}
		entry := out[k]
		entry.at = at
		if isRX {
			entry.rx += int64(v)
		} else {
			entry.tx += int64(v)
		}
		out[k] = entry
	}
}

// labelValue extracts `value` from a labels string of the form
// `a="x",b="y",c="z"`. Returns "" if the key isn't present. Match is
// anchored at a label boundary (start-of-string or comma) so
// hypothetical labels like `kubepod="x"` can't shadow a real
// `pod="y"` lookup. Doesn't honour Prometheus's escaped-quote rules
// — pod and namespace names can't contain quotes anyway.
func labelValue(labels, key string) string {
	needle := key + `="`
	off := 0
	for off < len(labels) {
		i := strings.Index(labels[off:], needle)
		if i < 0 {
			return ""
		}
		abs := off + i
		// Boundary check: must be at start-of-string or right after a
		// comma, otherwise we've matched a label name's suffix.
		if abs == 0 || labels[abs-1] == ',' {
			start := abs + len(needle)
			end := strings.IndexByte(labels[start:], '"')
			if end < 0 {
				return ""
			}
			return labels[start : start+end]
		}
		off = abs + len(needle)
	}
	return ""
}

func (p *NetworkPoller) send(snap NetworkSnapshot) {
	select {
	case p.Out <- snap:
	default:
		p.DroppedEvents.Add(1)
	}
}
