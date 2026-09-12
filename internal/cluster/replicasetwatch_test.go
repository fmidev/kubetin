package cluster

import (
	"testing"

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
