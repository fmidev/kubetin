package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fleetDetailSrv answers a FleetDetail fetch with two pod pages (the
// worst pod hidden on page two), degraded deployments, and warning
// events that group. failEvents turns the event list into a 500.
type fleetDetailSrv struct {
	failEvents atomic.Bool
}

func (f *fleetDetailSrv) handler() http.HandlerFunc {
	recent := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	stale := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	pod := func(name, phase, reason string, restarts int) string {
		st := ""
		if reason != "" || restarts > 0 {
			st = fmt.Sprintf(`,"containerStatuses":[{"name":"c","restartCount":%d,`+
				`"state":{"waiting":{"reason":"%s"}}}]`, restarts, reason)
		}
		return fmt.Sprintf(`{"metadata":{"name":"%s","namespace":"a"},`+
			`"status":{"phase":"%s"%s}}`, name, phase, st)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/pods":
			if q.Get("fieldSelector") == "" {
				// Scope probe.
				io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[]}`)
				return
			}
			if q.Get("continue") == "" {
				items := []string{
					pod("failed-1", "Failed", "Error", 1),
					// Stuck in init: regular containers only say
					// PodInitializing, the truth lives in the init status.
					`{"metadata":{"name":"init-crash","namespace":"a"},"status":{"phase":"Pending",` +
						`"containerStatuses":[{"name":"c","restartCount":0,` +
						`"state":{"waiting":{"reason":"PodInitializing"}}}],` +
						`"initContainerStatuses":[{"name":"i","restartCount":7,` +
						`"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`,
				}
				for i := 0; i < 16; i++ {
					items = append(items, pod(fmt.Sprintf("filler-%02d", i), "Pending", "", 0))
				}
				io.WriteString(w, `{"kind":"PodList","apiVersion":"v1",`+
					`"metadata":{"continue":"page2"},"items":[`+strings.Join(items, ",")+`]}`)
				return
			}
			// Page two carries the worst pod of all: multi-container,
			// one cleanly Completed, one OOMKilled — the failure must
			// win the reason slot.
			io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","metadata":{},"items":[`+
				`{"metadata":{"name":"failed-9","namespace":"a"},"status":{"phase":"Failed",`+
				`"containerStatuses":[`+
				`{"name":"sidecar","restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}},`+
				`{"name":"main","restartCount":9,"state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}]}`)
		case "/apis/apps/v1/deployments":
			io.WriteString(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","metadata":{},"items":[`+
				`{"metadata":{"name":"d-part","namespace":"a"},"spec":{"replicas":4},"status":{"readyReplicas":2,"availableReplicas":2}},`+
				`{"metadata":{"name":"d-zero","namespace":"a"},"spec":{"replicas":3},"status":{}},`+
				`{"metadata":{"name":"d-ok","namespace":"a"},"spec":{"replicas":3},"status":{"readyReplicas":3,"availableReplicas":3}},`+
				`{"metadata":{"name":"d-scaled","namespace":"a"},"spec":{"replicas":0},"status":{}}]}`)
		case "/api/v1/events":
			if f.failEvents.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"boom","code":500}`)
				return
			}
			io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{},"items":[`+
				`{"metadata":{"name":"e1","namespace":"a"},"lastTimestamp":"`+recent+`","type":"Warning","reason":"BackOff","message":"pull fail","count":3},`+
				`{"metadata":{"name":"e2","namespace":"a"},"lastTimestamp":"`+recent+`","type":"Warning","reason":"BackOff","message":"pull fail","count":4},`+
				`{"metadata":{"name":"e3","namespace":"a"},"lastTimestamp":"`+recent+`","type":"Warning","reason":"FailedMount","message":"volume gone","count":5},`+
				`{"metadata":{"name":"e4","namespace":"a"},"lastTimestamp":"`+stale+`","type":"Warning","reason":"Old","message":"ancient","count":99}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestFleetDetailCollectsOrdersAndPaginates(t *testing.T) {
	srv := httptest.NewServer((&fleetDetailSrv{}).handler())
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")

	res := sup.FleetDetail(context.Background(), "slow")
	if res.Err != "" {
		t.Fatalf("Err = %q, want clean fetch", res.Err)
	}
	if len(res.Pods) != fleetDetailPodCap {
		t.Fatalf("pods = %d, want capped at %d", len(res.Pods), fleetDetailPodCap)
	}
	if res.Pods[0].Name != "failed-9" || res.Pods[0].Restarts != 9 {
		t.Errorf("worst pod first = %+v, want failed-9 from page two — pagination must run before the cap", res.Pods[0])
	}
	if res.Pods[0].Reason != "OOMKilled" {
		t.Errorf("pods[0].Reason = %q, want OOMKilled — a Completed sidecar must not mask it", res.Pods[0].Reason)
	}
	if res.Pods[1].Name != "failed-1" {
		t.Errorf("pods[1] = %+v, want failed-1", res.Pods[1])
	}
	if res.Pods[2].Name != "init-crash" || res.Pods[2].Reason != "Init:CrashLoopBackOff" || res.Pods[2].Restarts != 7 {
		t.Errorf("pods[2] = %+v, want the init-container reason and restarts", res.Pods[2])
	}

	if len(res.Deploys) != 2 || res.Deploys[0].Name != "d-zero" || res.Deploys[1].Name != "d-part" {
		t.Errorf("deploys = %+v, want [d-zero d-part] worst ratio first", res.Deploys)
	}

	if len(res.Events) != 2 {
		t.Fatalf("events = %+v, want 2 groups (stale excluded)", res.Events)
	}
	if res.Events[0].Reason != "BackOff" || res.Events[0].Count != 7 {
		t.Errorf("events[0] = %+v, want BackOff ×7 (3+4 merged)", res.Events[0])
	}
	if res.Events[1].Reason != "FailedMount" || res.Events[1].Count != 5 {
		t.Errorf("events[1] = %+v, want FailedMount ×5", res.Events[1])
	}
}

func TestFleetDetailPartialFailureKeepsWhatSucceeded(t *testing.T) {
	fs := &fleetDetailSrv{}
	fs.failEvents.Store(true)
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")

	res := sup.FleetDetail(context.Background(), "slow")
	if res.Err == "" {
		t.Error("a failed list must surface in Err")
	}
	if len(res.Pods) == 0 || len(res.Deploys) == 0 {
		t.Errorf("pods/deploys should survive an event failure: %d/%d", len(res.Pods), len(res.Deploys))
	}
	if len(res.Events) != 0 {
		t.Errorf("events = %+v, want none from a failed list", res.Events)
	}
}
