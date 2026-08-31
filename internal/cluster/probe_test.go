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
	sup, store := newProbeFixture(t, srv, "")

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
		case "/apis/apps/v1/deployments":
			io.WriteString(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","metadata":{},"items":[]}`)
		case "/api/v1/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{},"items":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newProbeFixture(t *testing.T, srv *httptest.Server, ns string) (*Supervisor, *model.Store) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: srv.URL}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	cfg.CurrentContext = "ctx"

	store := model.NewStore()
	sup := New(&kubeconfig.Discovered{
		Files:    []string{"/fake"},
		Refs:     []kubeconfig.ContextRef{{Name: "slow", RawName: "ctx", File: "/fake", Namespace: ns}},
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
	sup, store := newProbeFixture(t, srv, "")

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
	sup, store := newProbeFixture(t, srv, "")

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
	sup, store := newProbeFixture(t, srv, "")

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
	sup, store := newProbeFixture(t, srv, "")

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

// Not knowing is not the same as being fine. A cluster held at
// Degraded by a real pod-access denial must not be promoted to Healthy
// by a round whose pod check merely timed out — the timeout carries no
// verdict, so the standing one stands.
func TestProbeOncePodCheckTimeoutDoesNotClearADenial(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	// The pod-summary check is the only pods list without a field
	// selector and without resourceVersion=0 — the scope probe and the
	// non-green health list both carry rv=0. First summary call is
	// denied; the second — the second round's — stalls past the
	// deadline.
	var summaryCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/nodes":
			io.WriteString(w, `{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[{`+
				`"metadata":{"name":"n1"},`+
				`"status":{"allocatable":{"cpu":"4","memory":"8Gi"},`+
				`"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/apis/apps/v1/deployments":
			io.WriteString(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","metadata":{},"items":[]}`)
		case "/api/v1/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{},"items":[]}`)
		case "/api/v1/pods":
			q := r.URL.Query()
			if q.Get("fieldSelector") == "" && q.Get("resourceVersion") == "" {
				if summaryCalls.Add(1) > 1 {
					time.Sleep(600 * time.Millisecond)
				}
			}
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",`+
				`"message":"pods is forbidden","reason":"Forbidden","code":403}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sup, store := newProbeFixture(t, srv, "")

	sup.probeOnce(context.Background(), "slow")
	if st, _ := store.Get("slow"); st.Reach != model.ReachDegraded {
		t.Fatalf("first round: Reach = %v (%q), want degraded from the 403", st.Reach, st.LastError)
	}

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.Reach != model.ReachDegraded {
		t.Errorf("Reach = %v (%q), want the denial to stand — the check timed out, it did not pass",
			st.Reach, st.LastError)
	}
	if !strings.Contains(st.LastError, "timed out") {
		t.Errorf("LastError = %q, want it to say the check timed out", st.LastError)
	}
}

// A namespace-restricted context whose scope call stalls. ResolveScope
// deliberately does not cache a transient failure, so NamespaceFor
// still answers "" — and the pod access probe used to read that "" and
// list cluster-wide, which for a restricted user is a certain 403 and
// a confident Degraded. The round already knows the namespace; the
// probe has to use it.
func TestProbeOnceScopedPodCheckSurvivesAScopeTimeout(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	var clusterWide, scoped atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/pods":
			// The scope probe. Stalls the first time — the transient
			// that never gets cached — then answers the only way a
			// namespace-restricted user is ever answered.
			if clusterWide.Add(1) == 1 {
				time.Sleep(600 * time.Millisecond)
			}
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",`+
				`"message":"pods is forbidden","reason":"Forbidden","code":403}`)
		case "/api/v1/namespaces/team-a/pods":
			scoped.Add(1)
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[]}`)
		case "/apis/apps/v1/namespaces/team-a/deployments":
			io.WriteString(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","metadata":{},"items":[]}`)
		case "/api/v1/namespaces/team-a/events":
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{},"items":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sup, store := newProbeFixture(t, srv, "team-a")
	sup.probeOnce(context.Background(), "slow")

	if scoped.Load() == 0 {
		t.Error("pod access went cluster-wide; it should use the namespace this round resolved")
	}
	st, _ := store.Get("slow")
	if st.Reach != model.ReachHealthy {
		t.Errorf("Reach = %v (%q), want healthy — a stalled scope call is not a denial",
			st.Reach, st.LastError)
	}
}

// healthSrv is an API server stand-in with interesting fleet-health
// data: a NotReady node, pressure conditions, a cordon, non-green
// pods, degraded deployments, and warning events of varying age.
// slowHealth stalls (and forbidHealth denies) exactly the three
// health lists — the field-selected pod list, deployments, events —
// leaving the liveness calls untouched.
type healthSrv struct {
	forbidHealth atomic.Bool
	slowHealth   atomic.Bool
	delay        time.Duration
	podFS        atomic.Value // fieldSelector seen on the non-green pod list
	eventFS      atomic.Value // fieldSelector seen on the event list
}

func (h *healthSrv) handler() http.HandlerFunc {
	now := time.Now()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	stale := now.Add(-2 * time.Hour).Format(time.RFC3339)
	recentMicro := now.Add(-3 * time.Minute).Format("2006-01-02T15:04:05.000000Z07:00")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		isHealthList := (r.URL.Path == "/api/v1/pods" && q.Get("fieldSelector") != "") ||
			r.URL.Path == "/apis/apps/v1/deployments" ||
			r.URL.Path == "/api/v1/events"
		if isHealthList {
			if h.slowHealth.Load() {
				time.Sleep(h.delay)
			}
			if h.forbidHealth.Load() {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",`+
					`"message":"forbidden","reason":"Forbidden","code":403}`)
				return
			}
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/nodes":
			io.WriteString(w, `{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[`+
				`{"metadata":{"name":"n1"},"status":{"allocatable":{"cpu":"4","memory":"8Gi"},`+
				`"conditions":[{"type":"Ready","status":"True"}]}},`+
				`{"metadata":{"name":"n2"},"status":{"allocatable":{"cpu":"4","memory":"8Gi"},`+
				`"conditions":[{"type":"Ready","status":"False"},{"type":"MemoryPressure","status":"True"}]}},`+
				`{"metadata":{"name":"n3"},"spec":{"unschedulable":true},`+
				`"status":{"allocatable":{"cpu":"4","memory":"8Gi"},`+
				`"conditions":[{"type":"Ready","status":"True"},{"type":"DiskPressure","status":"True"},`+
				`{"type":"PIDPressure","status":"True"}]}}]}`)
		case "/api/v1/pods":
			if fs := q.Get("fieldSelector"); fs != "" {
				h.podFS.Store(fs)
				io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[`+
					`{"metadata":{"name":"p1","namespace":"a"},"status":{"phase":"Pending"}},`+
					`{"metadata":{"name":"p2","namespace":"a"},"status":{"phase":"Pending"}},`+
					`{"metadata":{"name":"p3","namespace":"a"},"status":{"phase":"Failed"}},`+
					`{"metadata":{"name":"p4","namespace":"a"},"status":{"phase":"Unknown"}}]}`)
				return
			}
			if q.Get("resourceVersion") == "" {
				// The pod summary: one page plus a best-effort remainder.
				io.WriteString(w, `{"kind":"PodList","apiVersion":"v1",`+
					`"metadata":{"continue":"tok","remainingItemCount":41},"items":[`+
					`{"metadata":{"name":"p1","namespace":"a"}}]}`)
				return
			}
			// The scope probe.
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[]}`)
		case "/apis/apps/v1/deployments":
			io.WriteString(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","metadata":{},"items":[`+
				`{"metadata":{"name":"d1","namespace":"a"},"spec":{"replicas":5},`+
				`"status":{"readyReplicas":3,"availableReplicas":2}},`+
				`{"metadata":{"name":"d2","namespace":"a"},"spec":{"replicas":2},`+
				`"status":{"availableReplicas":0}},`+
				`{"metadata":{"name":"d3","namespace":"a"},"spec":{},`+
				`"status":{"readyReplicas":1,"availableReplicas":1}},`+
				`{"metadata":{"name":"d4","namespace":"a"},"spec":{"replicas":0},"status":{}}]}`)
		case "/api/v1/events":
			h.eventFS.Store(q.Get("fieldSelector"))
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{},"items":[`+
				`{"metadata":{"name":"e1","namespace":"a"},"lastTimestamp":"`+recent+`",`+
				`"type":"Warning","reason":"BackOff"},`+
				`{"metadata":{"name":"e2","namespace":"a"},"lastTimestamp":"`+stale+`",`+
				`"type":"Warning","reason":"Failed"},`+
				`{"metadata":{"name":"e3","namespace":"a"},"eventTime":"`+recentMicro+`",`+
				`"type":"Warning","reason":"FailedScheduling"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestProbeOnceCollectsHealthSignals(t *testing.T) {
	hs := &healthSrv{}
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv, "")

	sup.probeOnce(context.Background(), "slow")

	st, ok := store.Get("slow")
	if !ok {
		t.Fatal("probe committed nothing")
	}
	if st.Reach != model.ReachDegraded || !strings.Contains(st.LastError, "nodes 2/3 ready") {
		t.Errorf("reach = %v (%q), want degraded from the NotReady node", st.Reach, st.LastError)
	}
	if got := st.NodesNotReadyNames; len(got) != 1 || got[0] != "n2" {
		t.Errorf("NodesNotReadyNames = %v, want [n2]", got)
	}
	if st.NodesMemPressure != 1 || st.NodesDiskPressure != 1 || st.NodesPIDPressure != 1 {
		t.Errorf("pressure = mem %d disk %d pid %d, want 1/1/1",
			st.NodesMemPressure, st.NodesDiskPressure, st.NodesPIDPressure)
	}
	if st.NodesCordoned != 1 {
		t.Errorf("NodesCordoned = %d, want 1", st.NodesCordoned)
	}
	if got := st.NodesPressureNames; len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Errorf("NodesPressureNames = %v, want [n2 n3]", got)
	}
	if st.PodsPending != 2 || st.PodsFailed != 1 || st.PodsUnknownPhase != 1 {
		t.Errorf("pods = pending %d failed %d unknown %d, want 2/1/1",
			st.PodsPending, st.PodsFailed, st.PodsUnknownPhase)
	}
	if st.PodsTotal != 42 {
		t.Errorf("PodsTotal = %d, want 42 (1 item + 41 remaining)", st.PodsTotal)
	}
	if st.DeploysTotal != 4 || st.DeploysDegraded != 2 || st.DeploysZeroReady != 1 {
		t.Errorf("deploys = total %d degraded %d zeroReady %d, want 4/2/1",
			st.DeploysTotal, st.DeploysDegraded, st.DeploysZeroReady)
	}
	wantNames := []string{"a/d2 0/2", "a/d1 3/5"}
	if len(st.DegradedDeployNames) != 2 ||
		st.DegradedDeployNames[0] != wantNames[0] || st.DegradedDeployNames[1] != wantNames[1] {
		t.Errorf("DegradedDeployNames = %v, want %v (worst ratio first)", st.DegradedDeployNames, wantNames)
	}
	if st.WarnEvents15m != 2 {
		t.Errorf("WarnEvents15m = %d, want 2 (recent lastTimestamp + eventTime-only)", st.WarnEvents15m)
	}
	if fs, _ := hs.podFS.Load().(string); fs != "status.phase!=Running,status.phase!=Succeeded" {
		t.Errorf("pod list fieldSelector = %q", fs)
	}
	if fs, _ := hs.eventFS.Load().(string); fs != "type=Warning" {
		t.Errorf("event list fieldSelector = %q", fs)
	}
}

// Denied health lists leave the "unknown" sentinels and must not touch
// reach or LastError — they are dashboard signals, not liveness checks.
func TestProbeOnceHealthDenialsLeaveUnknownAndReachAlone(t *testing.T) {
	hs := &healthSrv{}
	hs.forbidHealth.Store(true)
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv, "")

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.Reach != model.ReachDegraded || !strings.Contains(st.LastError, "nodes 2/3 ready") {
		t.Errorf("reach = %v (%q), want the node verdict untouched by health denials",
			st.Reach, st.LastError)
	}
	if st.PodsPending != -1 || st.PodsFailed != -1 || st.PodsUnknownPhase != -1 {
		t.Errorf("pod phases = %d/%d/%d, want -1 sentinels on denial",
			st.PodsPending, st.PodsFailed, st.PodsUnknownPhase)
	}
	if st.DeploysTotal != -1 || st.WarnEvents15m != -1 {
		t.Errorf("deploys %d / warnEvents %d, want -1 sentinels on denial",
			st.DeploysTotal, st.WarnEvents15m)
	}
	if st.PodsTotal != 42 {
		t.Errorf("PodsTotal = %d, want 42 — the summary call is not a health list", st.PodsTotal)
	}
	if st.NodesMemPressure != 1 {
		t.Errorf("NodesMemPressure = %d, want 1 — node health rides the node list", st.NodesMemPressure)
	}
}

// A stalled health list is not a finding: the previous round's values
// stand, exactly like the node-list timeout path.
func TestProbeOnceHealthTimeoutCarriesForward(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	hs := &healthSrv{delay: 600 * time.Millisecond}
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv, "")

	sup.probeOnce(context.Background(), "slow")
	if st, _ := store.Get("slow"); st.PodsPending != 2 {
		t.Fatalf("first round: PodsPending = %d, want 2", st.PodsPending)
	}

	hs.slowHealth.Store(true)
	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.PodsPending != 2 || st.PodsFailed != 1 || st.PodsUnknownPhase != 1 {
		t.Errorf("pod phases = %d/%d/%d, want the last known 2/1/1 carried",
			st.PodsPending, st.PodsFailed, st.PodsUnknownPhase)
	}
	if st.DeploysTotal != 4 || st.DeploysDegraded != 2 {
		t.Errorf("deploys = %d/%d, want the last known 4/2 carried",
			st.DeploysTotal, st.DeploysDegraded)
	}
	if st.WarnEvents15m != 2 {
		t.Errorf("WarnEvents15m = %d, want the last known 2 carried", st.WarnEvents15m)
	}
}

// With nothing to carry, a first-round health timeout reports unknown.
// Run seeds every context with the -1 sentinels before the first round;
// the seed here mimics that.
func TestProbeOnceFirstRoundHealthTimeoutReportsUnknown(t *testing.T) {
	old := ProbeTimeout
	ProbeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = old })

	hs := &healthSrv{delay: 600 * time.Millisecond}
	hs.slowHealth.Store(true)
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv, "")

	seed := model.NewProbeFields()
	seed.Reach = model.ReachUnknown
	store.ApplyProbe("slow", seed)

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.PodsPending != -1 || st.DeploysTotal != -1 || st.WarnEvents15m != -1 {
		t.Errorf("health = pods %d deploys %d events %d, want -1 unknowns, not fabricated zeros",
			st.PodsPending, st.DeploysTotal, st.WarnEvents15m)
	}
}

// An empty healthy cluster measures zeros — which are findings, not
// unknowns.
func TestProbeOnceHealthZerosAreMeasurements(t *testing.T) {
	srv := httptest.NewServer((&probeSrv{}).handler())
	defer srv.Close()
	sup, store := newProbeFixture(t, srv, "")

	sup.probeOnce(context.Background(), "slow")

	st, _ := store.Get("slow")
	if st.PodsPending != 0 || st.PodsFailed != 0 || st.DeploysTotal != 0 || st.WarnEvents15m != 0 {
		t.Errorf("health = pending %d failed %d deploys %d events %d, want measured zeros",
			st.PodsPending, st.PodsFailed, st.DeploysTotal, st.WarnEvents15m)
	}
	if st.PodsTotal != 0 {
		t.Errorf("PodsTotal = %d, want an exact 0 from a complete list", st.PodsTotal)
	}
}
