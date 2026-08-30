package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	got := projectContainerInfo(statuses, map[string]int64{"api": 512 << 20})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	if got[0].MemLimitBytes != 512<<20 {
		t.Errorf("mem limit = %d, want joined by name (512Mi)", got[0].MemLimitBytes)
	}
	if got[1].MemLimitBytes != 0 {
		t.Errorf("mem limit = %d, want 0 for a container not in the map", got[1].MemLimitBytes)
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
	if got := projectContainerInfo(nil, nil); got != nil {
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

// The pod-level limit is all-or-nothing: a partial sum understates
// the ceiling and fakes >100% usage, so any counted container
// without a limit collapses the total to 0 ("no limit"). Plain init
// containers don't count (terminated before usage is measured);
// sidecar (Always-restart) init containers do. The status-reported
// limit wins over spec — after an in-place resize only status
// carries the enforced value.
func TestPodMemLimit(t *testing.T) {
	lim := func(s string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(s)}
	}
	always := corev1.ContainerRestartPolicyAlways

	cases := []struct {
		name string
		pod  corev1.Pod
		want int64
	}{
		{
			name: "all containers limited",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				{Name: "envoy", Resources: corev1.ResourceRequirements{Limits: lim("512Mi")}},
			}}},
			want: 768 << 20,
		},
		{
			name: "one container unlimited",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				{Name: "envoy"},
			}}},
			want: 0,
		},
		{
			name: "no limits at all",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "api"},
			}}},
			want: 0,
		},
		{
			name: "plain init container excluded",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				},
				InitContainers: []corev1.Container{
					{Name: "migrate", Resources: corev1.ResourceRequirements{Limits: lim("1Gi")}},
				},
			}},
			want: 256 << 20,
		},
		{
			name: "sidecar init container included",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				},
				InitContainers: []corev1.Container{
					{Name: "proxy", RestartPolicy: &always,
						Resources: corev1.ResourceRequirements{Limits: lim("128Mi")}},
				},
			}},
			want: 384 << 20,
		},
		{
			name: "sidecar without limit collapses to zero",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				},
				InitContainers: []corev1.Container{
					{Name: "proxy", RestartPolicy: &always},
				},
			}},
			want: 0,
		},
		{
			name: "status resources override spec",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
					{Name: "api", Resources: &corev1.ResourceRequirements{Limits: lim("512Mi")}},
				}},
			},
			want: 512 << 20,
		},
		{
			name: "nil status resources fall back to spec",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "api", Resources: corev1.ResourceRequirements{Limits: lim("256Mi")}},
				}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
					{Name: "api"},
				}},
			},
			want: 256 << 20,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podMemLimit(&tc.pod, effectiveMemLimits(&tc.pod))
			if got != tc.want {
				t.Errorf("podMemLimit = %d, want %d", got, tc.want)
			}
		})
	}
}
