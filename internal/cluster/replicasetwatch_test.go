package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestReplicaSetControllerProjection(t *testing.T) {
	for _, tc := range []struct {
		name, kind, version string
		controller          bool
		want                types.UID
	}{
		{"deployment", "Deployment", "apps/v1", true, "owner"},
		{"non-controller", "Deployment", "apps/v1", false, ""},
		{"other-kind", "StatefulSet", "apps/v1", true, ""},
		{"other-api", "Deployment", "other/v1", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewReplicaSetWatcher("alpha", 4)
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{UID: "rs", Name: "replicas", Namespace: "team", OwnerReferences: []metav1.OwnerReference{
				{APIVersion: tc.version, Kind: tc.kind, Name: "owner", UID: "owner", Controller: &tc.controller},
			}}}
			w.emit(ReplicaSetAdded, rs)
			ev := <-w.Out
			if ev.DeploymentUID != tc.want || ev.UID != "rs" || ev.Namespace != "team" || ev.Context != "alpha" {
				t.Fatalf("projection: %+v", ev)
			}
			rs.OwnerReferences = nil
			w.emit(ReplicaSetUpdated, rs)
			if ev = <-w.Out; ev.DeploymentUID != "" {
				t.Fatal("removed owner remains cached")
			}
			w.emit(ReplicaSetDeleted, rs)
			if ev = <-w.Out; ev.Kind != ReplicaSetDeleted || ev.UID != "rs" {
				t.Fatal("missing deletion identity")
			}
		})
	}
}

func TestEventProjectionPreservesInvolvedUID(t *testing.T) {
	w := NewEventWatcher("alpha", 1)
	w.emit(EvtAdded, &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "event"}, InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Name: "rs", UID: "rs-uid"}})
	if ev := <-w.Out; ev.InvolvedUID != "rs-uid" {
		t.Fatalf("lost object identity: %+v", ev)
	}
}

func TestReplicaSetWatcherRecoversAfterSyncDeadline(t *testing.T) {
	old := replicaSetSyncTimeout
	replicaSetSyncTimeout = 20 * time.Millisecond
	defer func() { replicaSetSyncTimeout = old }()
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				select {
				case <-r.Context().Done():
					return
				case <-release:
				}
				if r.URL.Query().Get("watch") == "true" {
					if r.URL.Query().Get("sendInitialEvents") == "true" {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","reason":"BadRequest","code":400}`)
						return
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				fmt.Fprint(w, `{"apiVersion":"apps/v1","kind":"ReplicaSetList","metadata":{"resourceVersion":"1"},"items":[{"metadata":{"uid":"rs","name":"api-rs","namespace":"team","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"api","uid":"dep","controller":true}]}}]}`)
			}))
			defer srv.Close()
			sup, _ := newProbeFixture(t, srv, "")
			sup.scopes.Store("slow", "")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			w := NewReplicaSetWatcher("slow", 4)
			go func() { done <- w.Run(ctx, sup) }()
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
			select {
			case err := <-done:
				done <- err
				t.Fatalf("watcher stopped at deadline: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			if cancelEarly {
				return
			}
			close(release)
			added := false
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			for {
				select {
				case ev := <-w.Out:
					if ev.Context != "slow" {
						t.Fatal("lost origin")
					}
					if ev.Kind == ReplicaSetAdded {
						added = true
						if ev.DeploymentUID != "dep" {
							t.Fatal("lost owner")
						}
					}
					if ev.Kind == ReplicaSetSynced {
						if !added {
							t.Fatal("synced before initial event")
						}
						return
					}
				case <-timer.C:
					t.Fatal("did not sync after recovery")
				}
			}
		})
	}
}
