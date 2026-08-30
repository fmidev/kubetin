package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/fmidev/kubetin/internal/kubeconfig"
	"github.com/fmidev/kubetin/internal/model"
)

func TestReachAccumulator_FirstSampleAcceptedImmediately(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnknown, pending: model.ReachUnknown}
	got := a.update(model.ReachAuthFailed)
	if got != model.ReachAuthFailed {
		t.Fatalf("first sample should be accepted; got %v want %v", got, model.ReachAuthFailed)
	}
}

func TestReachAccumulator_HealthyPromotesInstantly(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnreachable, pending: model.ReachUnreachable}
	got := a.update(model.ReachHealthy)
	if got != model.ReachHealthy {
		t.Fatalf("Healthy should promote immediately; got %v", got)
	}
}

func TestReachAccumulator_DemotionRequiresTwoSamples(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachHealthy, pending: model.ReachHealthy}

	// First demotion sample shouldn't change visible state.
	got := a.update(model.ReachUnreachable)
	if got != model.ReachHealthy {
		t.Fatalf("after one demotion sample, visible should stay Healthy; got %v", got)
	}

	// Second consecutive demotion flips.
	got = a.update(model.ReachUnreachable)
	if got != model.ReachUnreachable {
		t.Fatalf("after two demotion samples, visible should flip; got %v", got)
	}
}

func TestReachAccumulator_PendingResetsOnNonMatch(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachHealthy, pending: model.ReachHealthy}

	// One Unreachable sample (count=1).
	a.update(model.ReachUnreachable)

	// Now an AuthFailed — different non-healthy. pendingCount should
	// reset to 1, not promote AuthFailed instantly.
	got := a.update(model.ReachAuthFailed)
	if got != model.ReachHealthy {
		t.Fatalf("after differing demotion sample, visible should stay Healthy; got %v", got)
	}

	// One more AuthFailed flips.
	got = a.update(model.ReachAuthFailed)
	if got != model.ReachAuthFailed {
		t.Fatalf("after two AuthFailed samples, visible should be AuthFailed; got %v", got)
	}
}

func TestReachAccumulator_MatchingSamplesNeverDemoteFromUnknown(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnknown, pending: model.ReachUnknown}

	// First sample: anything is accepted.
	a.update(model.ReachUnreachable)

	// Identical sample: still Unreachable.
	got := a.update(model.ReachUnreachable)
	if got != model.ReachUnreachable {
		t.Fatalf("repeated samples should not demote; got %v", got)
	}
}

// A probe round is four sequential API calls. They used to share one
// ProbeTimeout, so a cluster far enough away that each call costs real
// latency — roughly 1s a call — blew the budget on the last one and
// committed Degraded, "context deadline exceeded", while every single
// call had in fact succeeded.
//
// Here each call comfortably beats the deadline and their sum does not:
// 4 × 120ms against a 250ms budget. Under the shared budget this probe
// reports Degraded; under a per-call one it reports the truth.
func TestProbeOnceDeadlineIsPerCallNotPerRound(t *testing.T) {
	const perCall = 120 * time.Millisecond

	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	srv := httptest.NewServer((&probeSrv{everyCall: perCall}).handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv)

	sup.probeOnce(context.Background(), "slow")

	st, ok := store.Get("slow")
	if !ok {
		t.Fatal("probe committed nothing")
	}
	if st.Reach != model.ReachHealthy {
		t.Errorf("Reach = %v (%q), want healthy — every call beat its own deadline",
			st.Reach, st.LastError)
	}
	if st.ServerVersion != "v1.30.0" {
		t.Errorf("ServerVersion = %q, want v1.30.0", st.ServerVersion)
	}
	if st.NodeCount != 1 || st.NodeReady != 1 {
		t.Errorf("nodes = %d/%d, want 1/1", st.NodeReady, st.NodeCount)
	}
}

// probeSrv is an API server stand-in for the probe tests: it answers
// the four calls a probe round makes, and slowNodes makes the node list
// (and only the node list) overrun the per-call deadline.
type probeSrv struct {
	slowNodes  atomic.Bool // make the node list, and only it, overrun the deadline
	forbidPods atomic.Bool
	delay      time.Duration // node-list delay when slowNodes is set
	everyCall  time.Duration // delay applied to every call
}

func (p *probeSrv) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(p.everyCall)
		if r.URL.Path == "/api/v1/nodes" && p.slowNodes.Load() {
			time.Sleep(p.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/nodes":
			io.WriteString(w, `{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[{`+
				`"metadata":{"name":"n1"},`+
				`"status":{"allocatable":{"cpu":"4","memory":"8Gi"},`+
				`"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/api/v1/pods":
			if p.forbidPods.Load() {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",`+
					`"message":"pods is forbidden","reason":"Forbidden","code":403}`)
				return
			}
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newProbeFixture(t *testing.T, srv *httptest.Server) (*Supervisor, *model.Store) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: srv.URL}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	cfg.CurrentContext = "ctx"

	store := model.NewStore()
	sup := New(&kubeconfig.Discovered{
		Files:    []string{"/fake"},
		Refs:     []kubeconfig.ContextRef{{Name: "slow", RawName: "ctx", File: "/fake"}},
		Configs:  map[string]*clientcmdapi.Config{"/fake": cfg},
		Contexts: []string{"slow"},
	}, store, time.Hour)
	return sup, store
}

// A deadline we chose is not evidence about the cluster. When the node
// list stalls — a relayed link can stall 10s while the API server
// answers the same query in 50ms locally — the probe used to commit
// zero nodes and a Degraded badge over a cluster it had just reached
// on /version. Now it carries the last known detail and leaves reach
// alone.
func TestProbeOnceKeepsLastKnownWhenNodeListTimesOut(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	ps := &probeSrv{delay: 600 * time.Millisecond}
	srv := httptest.NewServer(ps.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv)

	sup.probeOnce(context.Background(), "slow")
	if st, _ := store.Get("slow"); st.Reach != model.ReachHealthy || st.NodeCount != 1 {
		t.Fatalf("first probe: reach=%v nodes=%d/%d (%q), want healthy 1/1",
			st.Reach, st.NodeReady, st.NodeCount, st.LastError)
	}

	ps.slowNodes.Store(true)
	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.Reach != model.ReachHealthy {
		t.Errorf("Reach = %v (%q), want healthy — a stalled list is not a sick cluster",
			st.Reach, st.LastError)
	}
	if st.NodeCount != 1 || st.NodeReady != 1 {
		t.Errorf("nodes = %d/%d, want the last known 1/1 rather than zeros",
			st.NodeReady, st.NodeCount)
	}
	if st.AllocCPUMilli != 4000 {
		t.Errorf("AllocCPUMilli = %d, want the last known 4000", st.AllocCPUMilli)
	}
	if !strings.Contains(st.LastError, "timed out") {
		t.Errorf("LastError = %q, want it to say the list timed out", st.LastError)
	}
}

// With no successful round to carry forward, node detail is unknown
// rather than zero — NodeCount -1 is what the UI already renders as
// "—". Reach still follows /version, which did answer.
func TestProbeOnceFirstRoundNodeTimeoutReportsUnknownNotZero(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	ps := &probeSrv{delay: 600 * time.Millisecond}
	ps.slowNodes.Store(true)
	srv := httptest.NewServer(ps.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv)

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.Reach != model.ReachHealthy {
		t.Errorf("Reach = %v (%q), want healthy — /version answered", st.Reach, st.LastError)
	}
	if st.NodeCount != -1 {
		t.Errorf("NodeCount = %d, want -1 (unknown) rather than a fabricated 0", st.NodeCount)
	}
	if st.ServerVersion != "v1.30.0" {
		t.Errorf("ServerVersion = %q, want v1.30.0", st.ServerVersion)
	}
}

// The counterpart to the timeout cases: an answer of "no" is a finding
// and must still demote. Only abandoning the call is not.
func TestProbeOnceStillDegradesOnPodAccessDenied(t *testing.T) {
	ps := &probeSrv{}
	ps.forbidPods.Store(true)
	srv := httptest.NewServer(ps.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv)

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.Reach != model.ReachDegraded {
		t.Errorf("Reach = %v (%q), want degraded — the cluster said no", st.Reach, st.LastError)
	}
	if st.LastError == "" || strings.Contains(st.LastError, "timed out") {
		t.Errorf("LastError = %q, want the RBAC refusal, not a timeout", st.LastError)
	}
}

// A measured zero is not the same as never having measured. A cluster
// whose node list legitimately comes back empty must keep that 0 across
// a later timeout, rather than having it rewritten to "unknown".
func TestProbeOnceKeepsAMeasuredZeroNodeCount(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	ps := &probeSrv{delay: 600 * time.Millisecond}
	ps.slowNodes.Store(true)
	srv := httptest.NewServer(ps.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv)

	// A completed round that measured an empty cluster.
	store.ApplyProbe("slow", model.ProbeFields{
		Reach:         model.ReachHealthy,
		ServerVersion: "v1.30.0",
		NodeCount:     0,
	})

	sup.probeOnce(context.Background(), "slow")

	if st, _ := store.Get("slow"); st.NodeCount != 0 {
		t.Errorf("NodeCount = %d, want the measured 0 kept, not rewritten to unknown", st.NodeCount)
	}
}
