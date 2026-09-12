package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFleetRunningPodsReadiness(t *testing.T) {
	ready := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "prod"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	crash := *ready.DeepCopy()
	crash.Name = "postgres-0"
	crash.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "postgres", UID: "owner"}}
	crash.Status.Conditions[0].Status = corev1.ConditionFalse
	crash.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", RestartCount: 500,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	unready := *ready.DeepCopy()
	unready.Name = "readiness-failed"
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	completed := *ready.DeepCopy()
	completed.Name, completed.Status.Phase = "completed", corev1.PodSucceeded
	var recoverPod atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/nodes":
			enc.Encode(corev1.NodeList{Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}})
		case "/api/v1/pods":
			q := r.URL.Query()
			if q.Get("fieldSelector") == "" {
				enc.Encode(corev1.PodList{Items: []corev1.Pod{ready}})
				return
			}
			if q.Get("fieldSelector") != "status.phase!=Succeeded" || q.Get("resourceVersion") != "" || q.Get("limit") != "500" {
				t.Errorf("unexpected health/detail query: %s", r.URL.RawQuery)
			}
			if q.Get("continue") == "" {
				enc.Encode(corev1.PodList{ListMeta: metav1.ListMeta{Continue: "page2"}, Items: []corev1.Pod{ready, completed}})
			} else if recoverPod.Load() {
				enc.Encode(corev1.PodList{Items: []corev1.Pod{ready}})
			} else {
				enc.Encode(corev1.PodList{Items: []corev1.Pod{crash, unready}})
			}
		case "/apis/apps/v1/deployments":
			enc.Encode(appsv1.DeploymentList{})
		case "/api/v1/events":
			enc.Encode(corev1.EventList{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	sup, store := newProbeFixture(t, server, "")
	sup.probeOnce(context.Background(), "slow")
	st, _ := store.Get("slow")
	if st.PodsNotReady != 2 || st.PodsPending != 0 || st.PodsFailed != 0 || st.PodsUnknownPhase != 0 {
		t.Fatalf("counts: notReady=%d pending=%d failed=%d unknown=%d", st.PodsNotReady, st.PodsPending, st.PodsFailed, st.PodsUnknownPhase)
	}
	result := sup.FleetDetail(context.Background(), "slow")
	if result.Err != "" || len(result.Truncated) != 0 || len(result.Pods) != 2 {
		t.Fatalf("detail = %+v", result)
	}
	if p := result.Pods[0]; p.Name != "postgres-0" || p.Reason != "CrashLoopBackOff" || p.Restarts != 500 {
		t.Fatalf("crash-loop detail = %+v", p)
	}
	if p := result.Pods[1]; p.Name != "readiness-failed" || p.Reason != "NotReady" {
		t.Fatalf("readiness detail = %+v", p)
	}
	recoverPod.Store(true)
	sup.probeOnce(context.Background(), "slow")
	st, _ = store.Get("slow")
	if st.PodsNotReady != 0 {
		t.Fatalf("recovered pods leave stale count: %d", st.PodsNotReady)
	}
	if result = sup.FleetDetail(context.Background(), "slow"); len(result.Pods) != 0 || result.Err != "" {
		t.Fatalf("healthy Running pods must not populate detail: %+v", result)
	}
}

func TestFleetDetailReportsUnvisitedPages(t *testing.T) {
	for _, resource := range []string{"pods", "deployments", "events"} {
		for _, continued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/continued=%t", resource, continued), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					enc := json.NewEncoder(w)
					kind, name := "", ""
					switch r.URL.Path {
					case "/api/v1/pods":
						kind, name = "PodList", "pods"
					case "/apis/apps/v1/deployments":
						kind, name = "DeploymentList", "deployments"
					case "/api/v1/events":
						kind, name = "EventList", "events"
					default:
						w.WriteHeader(404)
						return
					}
					meta := metav1.ListMeta{}
					if name == resource && !(name == "pods" && r.URL.Query().Get("fieldSelector") == "") {
						page := int(calls.Add(1))
						if page < fleetDetailPageCap || continued {
							meta.Continue = strconv.Itoa(page)
						}
					}
					version := "v1"
					if name == "deployments" {
						version = "apps/v1"
					}
					enc.Encode(map[string]any{"kind": kind, "apiVersion": version, "metadata": meta, "items": []any{}})
				}))
				defer server.Close()
				sup, _ := newProbeFixture(t, server, "")
				result := sup.FleetDetail(context.Background(), "slow")
				if calls.Load() != fleetDetailPageCap || result.Err != "" {
					t.Fatalf("calls=%d result=%+v", calls.Load(), result)
				}
				var want []string
				if continued {
					want = []string{resource}
				}
				if !slices.Equal(result.Truncated, want) {
					t.Fatalf("truncated=%v, want %v", result.Truncated, want)
				}
			})
		}
	}
}
