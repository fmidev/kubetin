package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeEmitProjectsAllocatable(t *testing.T) {
	w := NewNodeWatcher("alpha", 1)
	w.emit(NodeAdded, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", UID: "u1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3500m"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
		},
	})
	ev := <-w.Out
	if ev.AllocCPUMilli != 3500 {
		t.Errorf("AllocCPUMilli = %d, want 3500", ev.AllocCPUMilli)
	}
	if ev.AllocMemBytes != 16<<30 {
		t.Errorf("AllocMemBytes = %d, want %d", ev.AllocMemBytes, 16<<30)
	}

	w.emit(NodeAdded, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2", UID: "u2"}})
	ev = <-w.Out
	if ev.AllocCPUMilli != 0 || ev.AllocMemBytes != 0 {
		t.Errorf("missing allocatable should project as 0, got %d/%d", ev.AllocCPUMilli, ev.AllocMemBytes)
	}
}
