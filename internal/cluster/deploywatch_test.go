package cluster

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeployEmitPreservesSelector(t *testing.T) {
	w := NewDeployWatcher("alpha", 1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{UID: "deployment", Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"tier": "backend"},
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"api", "api-v2"}},
			},
		}},
	}
	want := d.Spec.Selector.DeepCopy()
	w.emit(DeployAdded, d)
	ev := <-w.Out
	if !reflect.DeepEqual(ev.Selector, want) {
		t.Fatalf("projected selector = %v, want %v", ev.Selector, want)
	}
	d.Spec.Selector.MatchLabels["tier"] = "other"
	d.Spec.Selector.MatchExpressions[0].Values[0] = "worker"
	if !reflect.DeepEqual(ev.Selector, want) {
		t.Fatal("event selector aliases the informer object")
	}
	d.Spec.Selector = nil
	w.emit(DeployUpdated, d)
	if ev = <-w.Out; ev.Selector != nil {
		t.Fatalf("missing selector projected as %v", ev.Selector)
	}
}
