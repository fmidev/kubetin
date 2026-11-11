package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// Fixtures follow the shapes a Rancher-managed Fleet v0.13 manager
// serves; the failure ones follow the documented status schema.
const (
	bundleHealthyJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"Bundle",
	"metadata":{"name":"platform-dns","namespace":"fleet-local","uid":"b-healthy","generation":10,
		"creationTimestamp":"2026-01-02T03:04:05Z",
		"labels":{"fleet.cattle.io/repo-name":"platform","fleet.cattle.io/commit":"0123456789abcdef0123456789abcdef01234567"}},
	"spec":{"resources":[{"name":"dns/deployment.yaml","content":"H4sIAAAAAAAA","encoding":"base64+gz"}]},
	"status":{"observedGeneration":10,"display":{"readyClusters":"1/1"},"summary":{"desiredReady":1,"ready":1},
		"conditions":[{"type":"Ready","status":"True","lastUpdateTime":"2026-11-07T20:15:48Z"}],
		"partitions":[{"name":"All","count":1}],"resourcesSha256Sum":"b8d2"}}`

	bundleFailingJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"Bundle",
	"metadata":{"name":"platform-ingress","namespace":"fleet-default","uid":"b-failing","generation":4,
		"creationTimestamp":"2026-01-02T03:04:05Z",
		"labels":{"fleet.cattle.io/repo-name":"platform","fleet.cattle.io/commit":"0123456789abcdef0123456789abcdef01234567"},
		"annotations":{"objectset.rio.cattle.io/applied":"H4sIAAAAAAAA-applied-object"}},
	"spec":{"paused":true,"helm":{"values":{"password":"hunter2"}},
		"resources":[{"name":"ingress/values.yaml","content":"H4sIAAAAAAAA-chart","encoding":"base64+gz"}]},
	"status":{"observedGeneration":3,"display":{"readyClusters":"1/2","state":"ErrApplied"},
		"summary":{"desiredReady":2,"ready":1,"errApplied":1,"nonReadyResources":[
			{"name":"fleet-default/edge-a","bundleState":"ErrApplied","message":"install failed: timed out waiting for the condition"}]},
		"conditions":[{"type":"Ready","status":"False","message":"ErrApplied(1) [Cluster fleet-default/edge-a]",
			"lastUpdateTime":"2026-03-04T05:06:07Z"}]}}`

	bundleNoStatusJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"Bundle",
	"metadata":{"name":"fresh","namespace":"fleet-local","uid":"b-fresh","generation":1}}`

	bundleDeploymentJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"BundleDeployment",
	"metadata":{"name":"platform-ingress","namespace":"cluster-fleet-default-edge-a-0a1b2c3d4e5f","uid":"bd-1",
		"labels":{"fleet.cattle.io/bundle-name":"platform-ingress","fleet.cattle.io/bundle-namespace":"fleet-default",
			"fleet.cattle.io/cluster":"edge-a","fleet.cattle.io/cluster-namespace":"fleet-default",
			"fleet.cattle.io/commit":"0123456789abcdef0123456789abcdef01234567","fleet.cattle.io/managed":"true"}},
	"spec":{"deploymentID":"s-abc:def","options":{"helm":{"values":{"password":"hunter2"}}}},
	"status":{"display":{"deployed":"False","monitored":"True","state":"ErrApplied"},
		"release":"ingress/platform-ingress:7",
		"resourceCounts":{"desiredReady":5,"ready":3,"notReady":1,"missing":1},
		"nonReadyStatus":[{"kind":"Deployment","apiVersion":"apps/v1","namespace":"ingress","name":"controller","uid":"x",
			"summary":{"state":"in-progress","transitioning":true,
				"message":["Available: 0/1","","Deployment does not have minimum availability."]}}],
		"modifiedStatus":[
			{"kind":"ConfigMap","apiVersion":"v1","namespace":"ingress","name":"controller-config","patch":"{\"data\":{\"k\":\"v\"}}"},
			{"kind":"Service","apiVersion":"v1","namespace":"ingress","name":"metrics","missing":true},
			{"kind":"Secret","apiVersion":"v1","namespace":"ingress","name":"stale","delete":true}],
		"resources":[{"kind":"Deployment","apiVersion":"apps/v1","namespace":"ingress","name":"controller"}],
		"conditions":[
			{"type":"Deployed","status":"False","message":"install failed: timed out waiting for the condition",
				"lastTransitionTime":"2026-03-04T05:06:07Z"},
			{"type":"Ready","status":"False","reason":"Error","lastUpdateTime":"2026-03-04T05:06:08Z"}]}}`

	gitRepoJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"GitRepo",
	"metadata":{"name":"platform","namespace":"fleet-default","uid":"g-1","generation":12,
		"creationTimestamp":"2026-01-02T03:04:05Z"},
	"spec":{"repo":"https://git.example.com/platform.git","branch":"main","paths":["ingress","dns"]},
	"status":{"commit":"0123456789abcdef0123456789abcdef01234567","observedGeneration":11,
		"display":{"readyBundleDeployments":"3/4","message":"fetch failed","error":true},
		"summary":{"desiredReady":4,"ready":3,"errApplied":1},
		"resources":[{"kind":"Deployment","name":"controller"}],
		"perClusterResourceCounts":{"fleet-default/edge-a":{"desiredReady":5}},
		"conditions":[{"type":"Stalled","status":"True","message":"fatal: could not read from remote repository",
			"lastUpdateTime":"2026-03-04T05:06:07Z"}]}}`

	bundleClusterJSON = `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"Cluster",
	"metadata":{"name":"c-abcde","namespace":"fleet-default","uid":"c-1",
		"labels":{"management.cattle.io/cluster-display-name":"edge-a","provider.cattle.io":"rke2"}},
	"spec":{"kubeConfigSecret":"c-abcde-kubeconfig"},
	"status":{"display":{"readyBundles":"1/2"},"agent":{"lastSeen":"2026-07-01T11:30:16Z","namespace":"cattle-fleet-system"},
		"summary":{"desiredReady":2,"ready":1,"errApplied":1},"apiServerURL":"https://manager.example",
		"conditions":[{"type":"Ready","status":"True","lastUpdateTime":"2025-11-15T15:01:43Z"}]}}`
)

func mustUnstructured(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return u
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func projected(t *testing.T, project func(*unstructured.Unstructured, *BundleEvent), s string) BundleEvent {
	t.Helper()
	w := NewBundleWatcher("alpha", 1)
	w.emit(BundleAdded, mustUnstructured(t, s), project)
	return <-w.Out
}

func TestBundleProjection(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	// metav1.Time decodes into local time.
	created := mustTime(t, "2026-01-02T03:04:05Z").Local()

	t.Run("healthy has no display state", func(t *testing.T) {
		ev := projected(t, projectBundle, bundleHealthyJSON)
		want := &Bundle{
			Namespace: "fleet-local", Name: "platform-dns", UID: "b-healthy",
			Repo: "platform", Commit: commit, Generation: 10, ObservedGeneration: 10,
			Summary: BundleSummary{Known: true, DesiredReady: 1, Ready: 1},
			Conditions: []BundleCondition{{Type: "Ready", Status: "True",
				LastUpdate: mustTime(t, "2026-11-07T20:15:48Z")}},
			CreatedAt: created,
		}
		if ev.Context != "alpha" || ev.UID != "b-healthy" || !reflect.DeepEqual(ev.Bundle, want) {
			t.Fatalf("got %+v\nwant %+v", ev.Bundle, want)
		}
	})

	t.Run("failing", func(t *testing.T) {
		ev := projected(t, projectBundle, bundleFailingJSON)
		want := &Bundle{
			Namespace: "fleet-default", Name: "platform-ingress", UID: "b-failing",
			Repo: "platform", Commit: commit, Paused: true, Generation: 4, ObservedGeneration: 3,
			State: "ErrApplied",
			Summary: BundleSummary{Known: true, DesiredReady: 2, Ready: 1, ErrApplied: 1,
				NonReady: []BundleNonReady{{Name: "fleet-default/edge-a", State: "ErrApplied",
					Message: "install failed: timed out waiting for the condition"}}},
			Conditions: []BundleCondition{{Type: "Ready", Status: "False",
				Message:    "ErrApplied(1) [Cluster fleet-default/edge-a]",
				LastUpdate: mustTime(t, "2026-03-04T05:06:07Z")}},
			CreatedAt: created,
		}
		if !reflect.DeepEqual(ev.Bundle, want) {
			t.Fatalf("got %+v\nwant %+v", ev.Bundle, want)
		}
	})

	t.Run("missing status is unknown, not zero", func(t *testing.T) {
		ev := projected(t, projectBundle, bundleNoStatusJSON)
		if ev.Bundle.Summary.Known || ev.Bundle.State != "" || ev.Bundle.Generation != 1 {
			t.Fatalf("got %+v", ev.Bundle)
		}
	})

	t.Run("bundle deployment", func(t *testing.T) {
		ev := projected(t, projectBundleDeployment, bundleDeploymentJSON)
		want := &BundleDeployment{
			Namespace: "cluster-fleet-default-edge-a-0a1b2c3d4e5f", Name: "platform-ingress", UID: "bd-1",
			BundleNamespace: "fleet-default", BundleName: "platform-ingress",
			ClusterNamespace: "fleet-default", ClusterName: "edge-a", Commit: commit,
			State: "ErrApplied", Release: "ingress/platform-ingress:7",
			Resources: BundleResourceCounts{Known: true, DesiredReady: 5, Ready: 3, NotReady: 1, Missing: 1},
			NotReady: []BundleObjectStatus{{Kind: "Deployment", Namespace: "ingress", Name: "controller",
				State: "in-progress", Transitioning: true,
				Message: "Available: 0/1; Deployment does not have minimum availability."}},
			Modified: []BundleModifiedObject{
				{Kind: "ConfigMap", Namespace: "ingress", Name: "controller-config", Patched: true},
				{Kind: "Service", Namespace: "ingress", Name: "metrics", Missing: true},
				{Kind: "Secret", Namespace: "ingress", Name: "stale", Delete: true},
			},
			Conditions: []BundleCondition{
				{Type: "Deployed", Status: "False", Message: "install failed: timed out waiting for the condition",
					LastUpdate: mustTime(t, "2026-03-04T05:06:07Z")},
				{Type: "Ready", Status: "False", Reason: "Error", LastUpdate: mustTime(t, "2026-03-04T05:06:08Z")},
			},
		}
		if !reflect.DeepEqual(ev.Deployment, want) {
			t.Fatalf("got %+v\nwant %+v", ev.Deployment, want)
		}
	})

	t.Run("git repo", func(t *testing.T) {
		ev := projected(t, projectGitRepo, gitRepoJSON)
		want := &GitRepo{
			Namespace: "fleet-default", Name: "platform", UID: "g-1", Branch: "main", Commit: commit,
			Generation: 12, ObservedGeneration: 11,
			Summary: BundleSummary{Known: true, DesiredReady: 4, Ready: 3, ErrApplied: 1},
			Message: "fetch failed", Error: true,
			Conditions: []BundleCondition{{Type: "Stalled", Status: "True",
				Message:    "fatal: could not read from remote repository",
				LastUpdate: mustTime(t, "2026-03-04T05:06:07Z")}},
			CreatedAt: created,
		}
		if !reflect.DeepEqual(ev.GitRepo, want) {
			t.Fatalf("got %+v\nwant %+v", ev.GitRepo, want)
		}
	})

	t.Run("cluster", func(t *testing.T) {
		ev := projected(t, projectBundleCluster, bundleClusterJSON)
		want := &BundleCluster{
			Namespace: "fleet-default", Name: "c-abcde", UID: "c-1", DisplayName: "edge-a",
			AgentLastSeen: mustTime(t, "2026-07-01T11:30:16Z"),
			Summary:       BundleSummary{Known: true, DesiredReady: 2, Ready: 1, ErrApplied: 1},
			Conditions: []BundleCondition{{Type: "Ready", Status: "True",
				LastUpdate: mustTime(t, "2025-11-15T15:01:43Z")}},
		}
		if !reflect.DeepEqual(ev.Cluster, want) {
			t.Fatalf("got %+v\nwant %+v", ev.Cluster, want)
		}
	})
}

// The informer caches only the trimmed object, so trimming must keep
// every field a projection reads — and nothing that made it large.
func TestTrimBundleObjectKeepsWhatProjectionReads(t *testing.T) {
	fixtures := map[string][]string{
		"bundles":           {bundleHealthyJSON, bundleFailingJSON, bundleNoStatusJSON},
		"bundledeployments": {bundleDeploymentJSON},
		"gitrepos":          {gitRepoJSON},
		"clusters":          {bundleClusterJSON},
	}
	for _, r := range bundleResources {
		for i, s := range fixtures[r.gvr.Resource] {
			t.Run(fmt.Sprintf("%s/%d", r.gvr.Resource, i), func(t *testing.T) {
				orig := mustUnstructured(t, s)
				trimmed := trimBundleObject(orig, r.keep).(*unstructured.Unstructured)

				var want, got BundleEvent
				r.project(orig, &want)
				r.project(trimmed, &got)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("trim changed the projection:\ngot  %+v\nwant %+v", got, want)
				}
				if again := trimBundleObject(trimmed, r.keep); !reflect.DeepEqual(again, trimmed) {
					t.Fatal("trim is not idempotent")
				}
				raw, err := trimmed.MarshalJSON()
				if err != nil {
					t.Fatal(err)
				}
				for _, gone := range []string{"H4sI", "hunter2", "annotations", `"resources"`, "apiServerURL", "perClusterResourceCounts"} {
					if strings.Contains(string(raw), gone) {
						t.Errorf("trimmed object still holds %s: %s", gone, raw)
					}
				}
				if trimmed.GetName() != orig.GetName() || trimmed.GetUID() != orig.GetUID() ||
					trimmed.GetNamespace() != orig.GetNamespace() {
					t.Fatal("trim lost identity")
				}
			})
		}
	}
	if got := trimBundleObject("not unstructured", nil); got != "not unstructured" {
		t.Fatal("non-object input altered")
	}
}

func TestHasAPIResourceSeparatesMissingFromFailed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		want    bool
		wantErr bool
	}{
		{"registered", http.StatusOK, true, false},
		{"not registered", http.StatusNotFound, false, false},
		{"server error", http.StatusInternalServerError, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				if tc.code == http.StatusOK {
					fmt.Fprint(w, fleetDiscoveryJSON)
				}
			}))
			defer srv.Close()
			got, err := hasAPIResource(&rest.Config{Host: srv.URL}, bundlesGVR)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

const fleetDiscoveryJSON = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"fleet.cattle.io/v1alpha1","resources":[
	{"name":"bundles","namespaced":true,"kind":"Bundle","verbs":["list","watch"]},
	{"name":"bundledeployments","namespaced":true,"kind":"BundleDeployment","verbs":["list","watch"]},
	{"name":"gitrepos","namespaced":true,"kind":"GitRepo","verbs":["list","watch"]},
	{"name":"clusters","namespaced":true,"kind":"Cluster","verbs":["list","watch"]}]}`

// fakeFleetManager serves discovery, access reviews and the four lists.
type fakeFleetManager struct {
	t         *testing.T
	discovery func(attempt int) int
	denied    map[string]bool
	items     map[string][]string

	mu       sync.Mutex
	attempts int
	listed   map[string]bool
}

func (f *fakeFleetManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	const prefix = "/apis/fleet.cattle.io/v1alpha1"
	switch {
	case r.URL.Path == prefix:
		f.mu.Lock()
		f.attempts++
		code := f.discovery(f.attempts)
		f.mu.Unlock()
		w.WriteHeader(code)
		if code == http.StatusOK {
			fmt.Fprint(w, fleetDiscoveryJSON)
		}
	case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		var review authv1.SelfSubjectAccessReview
		raw, _ := io.ReadAll(r.Body)
		// client-go sends built-in types as protobuf.
		if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(raw, nil, &review); err != nil {
			f.t.Error(err)
			return
		}
		attrs := review.Spec.ResourceAttributes
		if attrs.Verb != "list" || attrs.Group != "fleet.cattle.io" || attrs.Namespace != "" {
			f.t.Errorf("unexpected review: %+v", attrs)
		}
		review.Status.Allowed = !f.denied[attrs.Resource]
		json.NewEncoder(w).Encode(review)
	case strings.HasPrefix(r.URL.Path, prefix+"/"):
		resource := strings.TrimPrefix(r.URL.Path, prefix+"/")
		f.mu.Lock()
		f.listed[resource] = true
		f.mu.Unlock()
		if r.URL.Query().Get("watch") == "true" {
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","reason":"BadRequest","code":400}`)
				return
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, `{"apiVersion":"fleet.cattle.io/v1alpha1","kind":"List","metadata":{"resourceVersion":"1"},"items":[%s]}`,
			strings.Join(f.items[resource], ","))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeFleetManager) wasListed(resource string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed[resource]
}

// runFakeManager runs a BundleWatcher against f and returns its events
// up to and including the first one done accepts, failing on timeout.
func runFakeManager(t *testing.T, f *fakeFleetManager, done func(BundleEvent) bool) []BundleEvent {
	t.Helper()
	f.t = t
	f.listed = map[string]bool{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")
	sup.scopes.Store("slow", "")

	ctx, cancel := context.WithCancel(context.Background())
	w := NewBundleWatcher("slow", 4)
	exited := make(chan error, 1)
	go func() { exited <- w.Run(ctx, sup) }()
	defer func() {
		cancel()
		select {
		case err := <-exited:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("watcher did not stop")
		}
	}()

	var got []BundleEvent
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case ev := <-w.Out:
			if ev.Context != "slow" {
				t.Fatalf("lost origin: %+v", ev)
			}
			got = append(got, ev)
			if done(ev) {
				return got
			}
		case err := <-exited:
			t.Fatalf("watcher exited early: %v", err)
		case <-timeout.C:
			t.Fatalf("timed out after %d events: %+v", len(got), got)
		}
	}
}

func isSynced(ev BundleEvent) bool { return ev.Kind == BundleSynced }

func TestBundleWatcherIdlesWithoutFleetAPI(t *testing.T) {
	f := &fakeFleetManager{discovery: func(int) int { return http.StatusNotFound }}
	got := runFakeManager(t, f, func(BundleEvent) bool { return true })
	if ev := got[0]; ev.Kind != BundleUnavailable || ev.Resource != "bundles" || ev.Reason != BundleNoAPI {
		t.Fatalf("got %+v", ev)
	}
}

func TestBundleWatcherParksWhenBundlesForbidden(t *testing.T) {
	f := &fakeFleetManager{
		discovery: func(int) int { return http.StatusOK },
		denied:    map[string]bool{"bundles": true},
	}
	got := runFakeManager(t, f, func(BundleEvent) bool { return true })
	if ev := got[0]; ev.Kind != BundleUnavailable || ev.Resource != "bundles" || ev.Reason != BundleForbidden {
		t.Fatalf("got %+v", ev)
	}
	time.Sleep(50 * time.Millisecond)
	for _, r := range bundleResources {
		if f.wasListed(r.gvr.Resource) {
			t.Errorf("listed %s without permission to list bundles", r.gvr.Resource)
		}
	}
}

func TestBundleWatcherSyncsAfterInitialObjectsAndSkipsDenied(t *testing.T) {
	f := &fakeFleetManager{
		discovery: func(int) int { return http.StatusOK },
		denied:    map[string]bool{"bundledeployments": true},
		items: map[string][]string{
			"bundles":           {bundleHealthyJSON, bundleFailingJSON},
			"bundledeployments": {bundleDeploymentJSON},
			"gitrepos":          {gitRepoJSON},
			"clusters":          {bundleClusterJSON},
		},
	}
	got := runFakeManager(t, f, isSynced)

	seen := map[string]bool{}
	var unavailable []BundleEvent
	for _, ev := range got[:len(got)-1] {
		switch {
		case ev.Kind == BundleUnavailable:
			unavailable = append(unavailable, ev)
		case ev.Bundle != nil:
			seen["bundle/"+ev.Bundle.Name] = true
		case ev.GitRepo != nil:
			seen["gitrepo/"+ev.GitRepo.Name] = true
		case ev.Cluster != nil:
			seen["cluster/"+ev.Cluster.Name] = true
		case ev.Deployment != nil:
			t.Fatal("delivered a resource the user may not list")
		}
	}
	want := map[string]bool{
		"bundle/platform-dns": true, "bundle/platform-ingress": true,
		"gitrepo/platform": true, "cluster/c-abcde": true,
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("synced before initial objects: saw %v", seen)
	}
	if len(unavailable) != 1 || unavailable[0].Resource != "bundledeployments" || unavailable[0].Reason != BundleForbidden {
		t.Fatalf("unavailable markers: %+v", unavailable)
	}
	if synced := got[len(got)-1]; synced.WatchNamespace != "" {
		t.Fatalf("lost scope: %+v", synced)
	}
	if f.wasListed("bundledeployments") {
		t.Fatal("listed a denied resource")
	}
}

func TestBundleWatcherRetriesFailedDiscovery(t *testing.T) {
	old := bundleGateRetry
	bundleGateRetry = 10 * time.Millisecond
	t.Cleanup(func() { bundleGateRetry = old })

	f := &fakeFleetManager{discovery: func(attempt int) int {
		if attempt == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}}
	got := runFakeManager(t, f, isSynced)
	if ev := got[0]; ev.Kind != BundleUnavailable || ev.Resource != "bundles" || ev.Reason != BundleCheckFailed {
		t.Fatalf("a failed check must not read as no API: %+v", ev)
	}
}
