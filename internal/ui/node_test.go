package ui

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

func podOn(node string, states ...cluster.ContainerState) podRow {
	return podRow{Node: node, ContainerStates: states}
}

func TestNodeContainerStatesBucketsByState(t *testing.T) {
	got := nodeContainerStates(map[types.UID]podRow{
		"a": podOn("n1", cluster.ContainerReady, cluster.ContainerReady),
		"b": podOn("n1", cluster.ContainerTerminated),
		"c": podOn("n1", cluster.ContainerError, cluster.ContainerWaiting),
		"d": podOn("n2", cluster.ContainerReady),
		// Unscheduled: no node to attribute the containers to.
		"e": podOn("", cluster.ContainerError),
	})

	want := nodeContainerCounts{ready: 2, waiting: 1, errored: 1, terminated: 1}
	if got["n1"] != want {
		t.Errorf("n1 = %+v, want %+v", got["n1"], want)
	}
	if got["n2"].ready != 1 || got["n2"].total() != 1 {
		t.Errorf("n2 = %+v, want ready:1 only", got["n2"])
	}
	if _, ok := got[""]; ok {
		t.Error("pod with no NodeName was attributed to a node")
	}
}

// A container that exited 0 is not ready, but it is also not a
// problem — completed helm-install Jobs pin themselves to a control
// plane node and stay in the pod cache forever. Counting not-ready as
// red painted those nodes solid red while the Pod view (which reads
// the four-state enum) showed nothing wrong.
func TestContainerDotsCompletedContainersAreNotErrors(t *testing.T) {
	withColour(t)
	th := DefaultTheme()

	c := nodeContainerCounts{ready: 28, terminated: 7}
	got := containerDots(c, 200, th)

	if n := strings.Count(got, th.StatusBad.Render("■")); n != 0 {
		t.Errorf("%d red dots for a node with no errored containers, want 0", n)
	}
	if n := strings.Count(got, th.StatusOK.Render("■")); n != 28 {
		t.Errorf("green dots = %d, want 28", n)
	}
	if n := strings.Count(got, th.StatusDim.Render("■")); n != 7 {
		t.Errorf("dim dots = %d, want 7", n)
	}
}

// Dots are emitted worst-first so a narrow column spends its budget on
// the containers worth seeing rather than on healthy or completed ones.
func TestContainerDotsWorstFirstUnderBudget(t *testing.T) {
	withColour(t)
	th := DefaultTheme()

	c := nodeContainerCounts{ready: 40, waiting: 1, errored: 2, terminated: 5}
	got := containerDots(c, 12, th)

	if n := strings.Count(got, th.StatusBad.Render("■")); n != 2 {
		t.Errorf("red dots = %d, want both errored containers to survive the budget", n)
	}
	if n := strings.Count(got, th.StatusWrn.Render("■")); n != 1 {
		t.Errorf("yellow dots = %d, want 1", n)
	}
	if n := strings.Count(got, th.StatusDim.Render("■")); n != 0 {
		t.Errorf("dim dots = %d, want terminated dropped first", n)
	}
	if !strings.Contains(got, "+44") {
		t.Errorf("containerDots(%+v, 12) = %q, want a +44 overflow suffix", c, got)
	}
}

func TestContainerDotsEmpty(t *testing.T) {
	withColour(t)
	th := DefaultTheme()
	if got, want := containerDots(nodeContainerCounts{}, 20, th), th.Dim.Render("—"); got != want {
		t.Errorf("containerDots(empty) = %q, want %q", got, want)
	}
}
