package cluster

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/fmidev/kubetin/internal/model"
)

// metricsInterval is the cadence of metrics-server polling per cluster.
// 30s is a deliberate compromise: metrics-server itself only resolves
// new readings every ~15-30s, polling faster wastes API calls.
const metricsInterval = 30 * time.Second

// runMetricsLoop polls metrics.k8s.io for each cluster. The loop
// wakes every 2s but only calls metrics-server when the cluster is
// known healthy AND metricsInterval has elapsed since the last call.
// This means metrics appear within ~2s of a cluster first becoming
// reachable, instead of having to wait a whole 30s tick.
func (s *Supervisor) runMetricsLoop(ctx context.Context, ctxName string) {
	const wakeInterval = 2 * time.Second
	t := time.NewTicker(wakeInterval)
	defer t.Stop()

	var lastPoll time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, ok := s.store.Get(ctxName)
		if !ok || st.Reach != model.ReachHealthy {
			continue
		}
		if !lastPoll.IsZero() && time.Since(lastPoll) < metricsInterval {
			continue
		}
		s.pollMetricsOnce(ctx, ctxName)
		lastPoll = time.Now()
	}
}

func (s *Supervisor) pollMetricsOnce(parent context.Context, ctxName string) {
	prev, ok := s.store.Get(ctxName)
	if !ok || prev.Reach != model.ReachHealthy {
		// Skip clusters we don't yet know are reachable. We'll catch
		// up next tick once probe promotes them.
		return
	}

	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		return
	}
	restCfg.Timeout = 5 * time.Second

	mc, err := metricsclientset.NewForConfig(restCfg)
	if err != nil {
		return
	}
	// Validate the kubernetes client too — used nowhere else, but
	// ensures we fail-fast on the same auth errors as the probe loop.
	if _, err := kubernetes.NewForConfig(restCfg); err != nil {
		return
	}

	pollCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	nm, err := mc.MetricsV1beta1().NodeMetricses().List(pollCtx, metav1.ListOptions{})
	if err != nil {
		// 404 / NotFound on the API itself = no metrics-server.
		// 403 = RBAC. Both are non-fatal.
		_ = apierrors.IsNotFound(err)
		s.store.ApplyMetrics(ctxName, model.MetricsFields{
			MetricsAvailable: false,
			MetricsAt:        time.Now(),
		})
		return
	}

	var cpu, mem int64
	for _, n := range nm.Items {
		if c, ok := n.Usage["cpu"]; ok {
			cpu += c.MilliValue()
		}
		if m, ok := n.Usage["memory"]; ok {
			mem += m.Value()
		}
	}

	s.store.ApplyMetrics(ctxName, model.MetricsFields{
		UsageCPUMilli:    cpu,
		UsageMemBytes:    mem,
		MetricsAvailable: true,
		MetricsAt:        time.Now(),
	})
}
