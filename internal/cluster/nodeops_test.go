package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func drainTestPod(name string) corev1.Pod {
	controller := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "replicas",
				UID: "controller-uid", Controller: &controller,
			}},
		},
		Spec:   corev1.PodSpec{NodeName: "worker"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

type drainTestAPI struct {
	pods              []corev1.Pod
	controllerUID     types.UID
	controllerStatus  int
	controllerDeleted bool
	discoveryStatus   int
	unknownKind       bool
	clusterController bool
	listStatus        int
	evictionStatus    int

	mu             sync.Mutex
	requests       []string
	evictions      []policyv1.Eviction
	controllerGets int
}

func (a *drainTestAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	status := func(code int) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   "Failure", Code: int32(code), Message: http.StatusText(code),
		})
	}
	switch {
	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/nodes/worker":
		_ = json.NewEncoder(w).Encode(corev1.Node{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{Name: "worker"},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
		if r.URL.Query().Get("fieldSelector") != "spec.nodeName=worker" {
			status(http.StatusBadRequest)
			return
		}
		if a.listStatus != 0 {
			status(a.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: a.pods,
		})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1":
		if a.discoveryStatus != 0 {
			status(a.discoveryStatus)
			return
		}
		list := metav1.APIResourceList{
			TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
			GroupVersion: "apps/v1",
		}
		if !a.unknownKind {
			list.APIResources = []metav1.APIResource{{Name: "replicasets", Kind: "ReplicaSet", Namespaced: !a.clusterController}}
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && (r.URL.Path == "/apis/apps/v1/namespaces/default/replicasets/replicas" && !a.clusterController ||
		r.URL.Path == "/apis/apps/v1/replicasets/replicas" && a.clusterController):
		a.controllerGets++
		if a.controllerStatus != 0 {
			status(a.controllerStatus)
			return
		}
		uid := a.controllerUID
		if uid == "" {
			uid = "controller-uid"
		}
		metadata := metav1.ObjectMeta{Name: "replicas", UID: uid}
		if a.controllerDeleted {
			now := metav1.Now()
			metadata.DeletionTimestamp = &now
		}
		_ = json.NewEncoder(w).Encode(struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta `json:"metadata"`
		}{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}, Metadata: metadata})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/eviction"):
		var eviction policyv1.Eviction
		if err := json.NewDecoder(r.Body).Decode(&eviction); err != nil {
			status(http.StatusBadRequest)
			return
		}
		a.evictions = append(a.evictions, eviction)
		if a.evictionStatus != 0 {
			status(a.evictionStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success", Code: 200,
		})
	default:
		status(http.StatusNotFound)
	}
}

func runTestDrain(t *testing.T, api *drainTestAPI) []DrainProgress {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := make(chan DrainProgress, 32)
	sup.Drain(ctx, "slow", "worker", out)
	var progress []DrainProgress
	for ev := range out {
		if ev.Context != "slow" || ev.Node != "worker" {
			t.Fatalf("progress lost origin: %+v", ev)
		}
		progress = append(progress, ev)
	}
	if len(progress) == 0 {
		t.Fatal("drain returned no progress")
	}
	return progress
}

func TestDrainPreflightRefusesBeforeAnyEviction(t *testing.T) {
	cases := []struct {
		name string
		edit func(*corev1.Pod, *drainTestAPI)
		want string
	}{
		{"unmanaged", func(p *corev1.Pod, _ *drainTestAPI) { p.OwnerReferences = nil }, "no controller"},
		{"non-controller owner", func(p *corev1.Pod, _ *drainTestAPI) { p.OwnerReferences[0].Controller = nil }, "no controller"},
		{"emptyDir", func(p *corev1.Pod, _ *drainTestAPI) {
			p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}, `emptyDir volume "scratch"`},
		{"memory emptyDir", func(p *corev1.Pod, _ *drainTestAPI) {
			p.Spec.Volumes = []corev1.Volume{{Name: "memory", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
		}, `emptyDir volume "memory"`},
		{"missing controller", func(p *corev1.Pod, a *drainTestAPI) { a.controllerStatus = http.StatusNotFound }, "cannot verify controller ReplicaSet/replicas"},
		{"unreadable controller", func(p *corev1.Pod, a *drainTestAPI) { a.controllerStatus = http.StatusForbidden }, "cannot verify controller ReplicaSet/replicas"},
		{"controller API failure", func(p *corev1.Pod, a *drainTestAPI) { a.controllerStatus = http.StatusInternalServerError }, "cannot verify controller ReplicaSet/replicas"},
		{"replaced controller", func(p *corev1.Pod, a *drainTestAPI) { a.controllerUID = "replacement-uid" }, "UID does not match"},
		{"missing owner UID", func(p *corev1.Pod, a *drainTestAPI) { p.OwnerReferences[0].UID = "" }, "UID does not match"},
		{"terminating controller", func(p *corev1.Pod, a *drainTestAPI) { a.controllerDeleted = true }, "is being deleted"},
		{"discovery denied", func(p *corev1.Pod, a *drainTestAPI) { a.discoveryStatus = http.StatusForbidden }, "cannot discover controller"},
		{"unknown controller kind", func(p *corev1.Pod, a *drainTestAPI) { a.unknownKind = true }, "cannot resolve controller kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := drainTestPod("unsafe")
			api := &drainTestAPI{}
			tc.edit(&candidate, api)
			api.pods = []corev1.Pod{candidate}
			progress := runTestDrain(t, api)
			last := progress[len(progress)-1]
			if last.Phase != "error" || !strings.Contains(last.Err, "default/unsafe") || !strings.Contains(last.Err, tc.want) {
				t.Fatalf("got %+v, want refusal naming pod and %q", last, tc.want)
			}
			if len(api.evictions) != 0 {
				t.Fatalf("unsafe drain sent evictions: %+v", api.evictions)
			}
		})
	}
}

func TestDrainPreflightChecksLaterPodsBeforeEvictingSafePods(t *testing.T) {
	unsafe := drainTestPod("unsafe")
	unsafe.OwnerReferences = nil
	api := &drainTestAPI{pods: []corev1.Pod{drainTestPod("safe"), unsafe}}
	progress := runTestDrain(t, api)
	last := progress[len(progress)-1]
	if last.Phase != "error" || !strings.Contains(last.Err, "default/unsafe") || len(api.evictions) != 0 {
		t.Fatalf("partially drained before refusing: last=%+v evictions=%+v", last, api.evictions)
	}
	if api.controllerGets != 1 {
		t.Fatalf("first pod was not validated: controller GETs=%d", api.controllerGets)
	}
}

func TestDrainPreflightPreservesSafeAndSkippedPods(t *testing.T) {
	for _, clusterScoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster-scoped-controller=%v", clusterScoped), func(t *testing.T) {
			mirror := drainTestPod("mirror")
			mirror.OwnerReferences = nil
			mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "hash"}
			daemon := drainTestPod("daemon")
			daemon.OwnerReferences[0].Kind = "DaemonSet"
			daemon.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			succeeded, failed := drainTestPod("succeeded"), drainTestPod("failed")
			succeeded.OwnerReferences, failed.OwnerReferences = nil, nil
			succeeded.Status.Phase, failed.Status.Phase = corev1.PodSucceeded, corev1.PodFailed
			api := &drainTestAPI{
				pods:              []corev1.Pod{mirror, daemon, succeeded, failed, drainTestPod("one"), drainTestPod("two")},
				clusterController: clusterScoped,
			}
			progress := runTestDrain(t, api)
			last := progress[len(progress)-1]
			if last.Phase != "done" || last.Total != 2 || len(api.evictions) != 2 {
				t.Fatalf("safe pods did not proceed: %+v, evictions=%+v", last, api.evictions)
			}
			if api.controllerGets != 1 {
				t.Errorf("shared controller fetched %d times, want once", api.controllerGets)
			}
			for i, eviction := range api.evictions {
				want := api.pods[i+4]
				if eviction.Name != want.Name || eviction.DeleteOptions == nil || eviction.DeleteOptions.Preconditions == nil ||
					eviction.DeleteOptions.Preconditions.UID == nil || *eviction.DeleteOptions.Preconditions.UID != want.UID {
					t.Errorf("eviction is not bound to validated pod %s: %+v", want.Name, eviction)
				}
			}
		})
	}
}

func TestDrainRefusesWhenPodListFails(t *testing.T) {
	api := &drainTestAPI{listStatus: http.StatusForbidden}
	progress := runTestDrain(t, api)
	last := progress[len(progress)-1]
	if last.Phase != "error" || len(api.evictions) != 0 || !strings.Contains(last.Err, "cordoned") {
		t.Fatalf("unexpected failure behavior: %+v, evictions=%+v", last, api.evictions)
	}
}

func TestDrainDoesNotRetryEvictionWithoutUIDPrecondition(t *testing.T) {
	api := &drainTestAPI{pods: []corev1.Pod{drainTestPod("replaced")}, evictionStatus: http.StatusConflict}
	progress := runTestDrain(t, api)
	if len(api.evictions) != 1 {
		t.Fatalf("got %d attempts after UID conflict, want one", len(api.evictions))
	}
	for _, ev := range progress {
		if ev.Phase == "blocked" && ev.Pod == "default/replaced" && ev.Done == 0 {
			return
		}
	}
	t.Fatalf("UID conflict was not reported as blocked: %+v", progress)
}
