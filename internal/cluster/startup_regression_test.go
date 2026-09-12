package cluster

import (
	"context"
	"fmt"
	"k8s.io/client-go/kubernetes"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFocusedPodMetricsPreserveSampleTime(t *testing.T) {
	sample := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/apis/metrics.k8s.io/v1beta1/pods" {
			fmt.Fprintf(w, `{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetricsList","items":[{"metadata":{"namespace":"prod","name":"api"},"timestamp":%q,"window":"15s","containers":[{"name":"api","usage":{"cpu":"123m","memory":"1Mi"}}]}]}`, sample.Format(time.RFC3339))
			return
		}
		fmt.Fprint(w, `{"apiVersion":"metrics.k8s.io/v1beta1","kind":"NodeMetricsList","items":[]}`)
	}))
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")
	cfg, _ := sup.RestConfigFor("slow")
	client, err := metricsclientset.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	poller := NewFocusedMetricsPoller("slow", 1)
	poller.tick(context.Background(), client, "")
	snapshot := <-poller.Out
	if !snapshot.OK || len(snapshot.Pods) != 1 || !snapshot.Pods[0].At.Equal(sample) {
		t.Fatalf("lost sample timestamp: %+v", snapshot)
	}
}

func TestScopeCallSharesTransientFailureAndRetriesLater(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			w.WriteHeader(503)
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","reason":"ServiceUnavailable","code":503}`)
			return
		}
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
	}))
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "team")
	cfg, _ := sup.RestConfigFor("slow")
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	results := make(chan string, n)
	go func() { results <- sup.ResolveScope(context.Background(), "slow", cs) }()
	<-entered
	var started sync.WaitGroup
	started.Add(n - 1)
	for range n - 1 {
		go func() { started.Done(); results <- sup.ResolveScope(context.Background(), "slow", cs) }()
	}
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range n {
		select {
		case got := <-results:
			if got != "team" {
				t.Fatalf("fallback=%q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("scope waiter stuck")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent callers made %d requests, want 1", calls.Load())
	}
	if _, ok := sup.scopes.Load("slow"); ok {
		t.Fatal("transient failure was cached")
	}
	if got := sup.ResolveScope(context.Background(), "slow", cs); got != "" {
		t.Fatalf("retry=%q, want cluster scope", got)
	}
	sup.ResolveScope(context.Background(), "slow", cs)
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want one failed and one cached success", calls.Load())
	}
}

func TestScopeWaiterCancellationDoesNotCancelSharedRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			t.Error("caller cancellation killed shared request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
	}))
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "team")
	cfg, _ := sup.RestConfigFor("slow")
	cs, _ := kubernetes.NewForConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan string, 1)
	go func() { result <- sup.ResolveScope(ctx, "slow", cs) }()
	<-entered
	secondCtx, secondCancel := context.WithCancel(context.Background())
	second := make(chan string, 1)
	go func() { second <- sup.ResolveScope(secondCtx, "slow", cs) }()
	secondCancel()
	cancel()
	for _, out := range []chan string{result, second} {
		select {
		case got := <-out:
			if got != "team" {
				t.Fatalf("cancel fallback=%q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled scope caller blocked")
		}
	}
	close(release)
	if got := sup.ResolveScope(context.Background(), "slow", cs); got != "" {
		t.Fatalf("shared request result=%q", got)
	}
}

func TestPodWatcherRecoversAfterInitialSyncTimeout(t *testing.T) {
	old := podSyncTimeout
	podSyncTimeout = 50 * time.Millisecond
	defer func() { podSyncTimeout = old }()
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403}`)
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			// Fall back to LIST when the client tries streaming initial events.
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"BadRequest","code":400}`)
				return
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[{"metadata":{"uid":"p","name":"api","namespace":"prod"},"status":{"phase":"Running"}}]}`)
	}))
	defer srv.Close()
	sup, _ := newProbeFixture(t, srv, "")
	sup.scopes.Store("slow", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher := NewPodWatcher("slow", 10)
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx, sup) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("watcher did not stop")
		}
	}()
	delayed, added := false, false
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("watcher exited instead of recovering: %v", err)
		case ev := <-watcher.Out:
			if ev.Context != "slow" {
				t.Fatalf("lost origin: %+v", ev)
			}
			switch ev.Kind {
			case PodSyncDelayed:
				delayed = true
				ready.Store(true)
			case PodAdded:
				added = true
			case PodSynced:
				if !delayed || !added {
					t.Fatalf("recovery order delayed=%v added=%v", delayed, added)
				}
				return
			}
		case <-timer.C:
			t.Fatal("watcher did not recover")
		}
	}
}

func TestPodSyncCompletionFollowsQueuedInitialPods(t *testing.T) {
	w := NewPodWatcher("slow", 1)
	w.publish("first", PodEvent{Kind: PodUpdated}, false)
	w.emitSync(PodSyncDelayed, "prod")
	w.publish("pod", PodEvent{Kind: PodAdded, UID: "pod"}, false)
	w.emitSync(PodSynced, "prod")
	ctx, cancel := context.WithCancel(context.Background())
	_, stop := w.start(ctx)
	defer stop()
	defer cancel()
	for _, want := range []PodEventKind{PodUpdated, PodSyncDelayed, PodAdded, PodSynced} {
		select {
		case got := <-w.Out:
			if got.Kind != want {
				t.Fatalf("event kind = %d, want %d", got.Kind, want)
			}
		case <-time.After(time.Second):
			t.Fatal("queued event was lost")
		}
	}
}
