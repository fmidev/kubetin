package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func checkWatchSync[T any](t *testing.T, resource, kind string, count int, out <-chan T,
	run func(context.Context, *Supervisor) error, observe func(T) (types.UID, string, bool)) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/pods" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/namespaces/team/"+resource) {
			t.Errorf("unexpected scope: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := make([]map[string]any, 0, count)
		for i := range count {
			items = append(items, map[string]any{"metadata": map[string]string{
				"name": fmt.Sprintf("item-%d", i), "uid": fmt.Sprint(i), "namespace": "team",
			}})
		}
		version := "v1"
		if resource == "deployments" {
			version = "apps/v1"
		}
		if r.URL.Query().Get("watch") == "true" {
			enc := json.NewEncoder(w)
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				for _, item := range items {
					item["apiVersion"], item["kind"] = version, kind
					enc.Encode(map[string]any{"type": "ADDED", "object": item})
				}
				enc.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
					"apiVersion": version, "kind": kind, "metadata": map[string]any{
						"resourceVersion": "1", "annotations": map[string]string{"k8s.io/initial-events-end": "true"},
					},
				}})
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind + "List",
			"metadata": map[string]string{"resourceVersion": "1"}, "items": items})
	}))
	defer server.Close()
	sup, _ := newProbeFixture(t, server, "team")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, sup) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	seen := make(map[types.UID]bool)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case ev := <-out:
			uid, origin, synced := observe(ev)
			if origin != "slow" {
				t.Fatalf("lost context: %q", origin)
			}
			if synced {
				if len(seen) != count {
					t.Fatalf("sync preceded initial delivery: %d/%d", len(seen), count)
				}
				return
			}
			seen[uid] = true
		case <-timeout.C:
			t.Fatal("no cache completion marker")
		}
	}
}

func TestWatchSyncIncludesEmptyListsAndQueuedInitialObjects(t *testing.T) {
	for _, count := range []int{0, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Run("pods", func(t *testing.T) {
				w := NewPodWatcher("slow", 1)
				checkWatchSync(t, "pods", "Pod", count, w.Out, w.Run, func(ev PodEvent) (types.UID, string, bool) {
					if ev.Kind == PodSynced && ev.WatchNamespace != "team" {
						t.Errorf("lost effective namespace: %q", ev.WatchNamespace)
					}
					return ev.UID, ev.Context, ev.Kind == PodSynced
				})
			})
			t.Run("deployments", func(t *testing.T) {
				w := NewDeployWatcher("slow", 1)
				checkWatchSync(t, "deployments", "Deployment", count, w.Out, w.Run, func(ev DeployEvent) (types.UID, string, bool) {
					return ev.UID, ev.Context, ev.Kind == DeploySynced
				})
			})
			t.Run("events", func(t *testing.T) {
				w := NewEventWatcher("slow", 1)
				checkWatchSync(t, "events", "Event", count, w.Out, w.Run, func(ev EventEvent) (types.UID, string, bool) {
					return ev.UID, ev.Context, ev.Kind == EvtSynced
				})
			})
		})
	}
}
