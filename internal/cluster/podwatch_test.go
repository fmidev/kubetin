package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// ContainerInfo is the single source of truth the coarse
// ContainerStates slice is derived from, so the reason/exit-code
// detail must line up with the four-bucket state on every entry.
func TestProjectContainerInfo(t *testing.T) {
	statuses := []corev1.ContainerStatus{
		{
			Name:         "api",
			Image:        "ghcr.io/x/api:1.2",
			Ready:        true,
			RestartCount: 0,
			State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		},
		{
			Name:         "envoy",
			Image:        "envoy:v1.29",
			Ready:        false,
			RestartCount: 3,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff",
			}},
		},
		{
			Name:         "migrate",
			Image:        "migrate:1",
			Ready:        false,
			RestartCount: 0,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason:   "OOMKilled",
				ExitCode: 137,
			}},
		},
	}

	got := projectContainerInfo(statuses)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	if got[0].State != ContainerReady || got[0].Reason != "" {
		t.Errorf("running+ready: state = %v, reason = %q; want ContainerReady with no reason",
			got[0].State, got[0].Reason)
	}
	if got[0].Image != "ghcr.io/x/api:1.2" {
		t.Errorf("image = %q, want the status image", got[0].Image)
	}

	if got[1].State != ContainerError {
		t.Errorf("CrashLoopBackOff: state = %v, want ContainerError", got[1].State)
	}
	if got[1].Reason != "CrashLoopBackOff" {
		t.Errorf("waiting reason = %q, want CrashLoopBackOff", got[1].Reason)
	}
	if got[1].Restarts != 3 {
		t.Errorf("restarts = %d, want 3", got[1].Restarts)
	}

	if got[2].State != ContainerError {
		t.Errorf("exit 137: state = %v, want ContainerError", got[2].State)
	}
	if got[2].Reason != "OOMKilled" || got[2].ExitCode != 137 {
		t.Errorf("terminated: reason = %q exit = %d; want OOMKilled/137",
			got[2].Reason, got[2].ExitCode)
	}
}

// An empty status slice must project to nil rather than an empty
// slice — consumers length-check to decide whether kubelet has
// reported yet, and the pod table's dot column renders "—" on nil.
func TestProjectContainerInfoEmpty(t *testing.T) {
	if got := projectContainerInfo(nil); got != nil {
		t.Errorf("projectContainerInfo(nil) = %v, want nil", got)
	}
}

// The scheduler's Reason/Message on a False condition is the only
// place the "why is this pod Pending" text exists — Phase alone says
// nothing. Verify it survives projection verbatim.
func TestProjectPodConditions(t *testing.T) {
	conds := []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: "Unschedulable", Message: "0/5 nodes are available"},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}

	got := projectPodConditions(conds)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Type != "PodScheduled" || got[0].Status != "False" {
		t.Errorf("got[0] = %+v, want PodScheduled/False", got[0])
	}
	if got[0].Reason != "Unschedulable" || got[0].Message != "0/5 nodes are available" {
		t.Errorf("reason/message = %q / %q; want them verbatim", got[0].Reason, got[0].Message)
	}
	// corev1.PodReady's value is "Ready", not "PodReady" — the
	// dashboard matches on these strings, so pin the real wire value.
	if got[1].Type != "Ready" || got[1].Status != "True" {
		t.Errorf("got[1] = %+v, want Ready/True", got[1])
	}
}

// copyLabels must detach from the informer's map: these event structs
// exist so nothing downstream holds informer memory.
func TestCopyLabelsDetaches(t *testing.T) {
	src := map[string]string{"app": "payments"}
	got := copyLabels(src)

	src["app"] = "mutated"
	if got["app"] != "payments" {
		t.Errorf("copy aliased the source map: got %q after mutating src", got["app"])
	}

	if copyLabels(nil) != nil {
		t.Error("copyLabels(nil) should be nil, not an empty map")
	}
	if copyLabels(map[string]string{}) != nil {
		t.Error("copyLabels(empty) should be nil, not an empty map")
	}
}
