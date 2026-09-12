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
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func logTestClient(t *testing.T, handler http.HandlerFunc) *kubernetes.Clientset {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestLogReconnect(t *testing.T) {
	const (
		old = "2026-09-07T10:00:00.100000000Z earlier"
		a   = "2026-09-07T10:00:00.500000000Z repeated"
		b   = "2026-09-07T10:00:00.500000000Z different"
		c   = "2026-09-07T10:00:00.600000000Z later"
	)
	for _, tc := range []struct {
		name      string
		streams   [][]string
		want      []string
		phase     corev1.PodPhase
		forbidden bool
		failOpen  bool
		failRead  bool
		wantError bool
		requests  int
	}{
		{name: "completed retained logs", streams: [][]string{{a}, {a}}, want: []string{a}, phase: corev1.PodSucceeded, requests: 2},
		{name: "final write before completion status", streams: [][]string{{a}, {a, c}}, want: []string{a, c}, phase: corev1.PodSucceeded, requests: 2},
		{name: "same timestamp and identical occurrences", streams: [][]string{{a, a}, {a, a, b, a}, {a, a, b, a, c}, {c}}, want: []string{a, a, b, a, c}, phase: corev1.PodSucceeded, requests: 4},
		{name: "server truncates to whole second", streams: [][]string{{old, a}, {old, a, c}, {old, a, c}}, want: []string{old, a, c}, phase: corev1.PodSucceeded, requests: 3},
		{name: "running container replay is bounded", streams: [][]string{{a}}, want: []string{a}, phase: corev1.PodRunning, wantError: true, requests: 6},
		{name: "pod status forbidden is bounded", streams: [][]string{{a}}, want: []string{a}, forbidden: true, wantError: true, requests: 6},
		{name: "transient open failure recovers", streams: [][]string{{a}, nil, {a, c}, {c}}, want: []string{a, c}, phase: corev1.PodSucceeded, failOpen: true, requests: 4},
		{name: "transient read failure on completed pod", streams: [][]string{{}, {a}, {a}}, want: []string{a}, phase: corev1.PodSucceeded, failRead: true, requests: 3},
		{name: "empty completed logs", streams: [][]string{{}}, phase: corev1.PodSucceeded, requests: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			cs := logTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/ns/pods/pod" {
					if tc.forbidden {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					phase := tc.phase
					if int(requests.Load()) < len(tc.streams)-1 {
						phase = corev1.PodRunning
					}
					_ = json.NewEncoder(w).Encode(corev1.Pod{Status: corev1.PodStatus{Phase: phase}})
					return
				}
				if r.URL.Path != "/api/v1/namespaces/ns/pods/pod/log" {
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				n := int(requests.Add(1))
				q := r.URL.Query()
				if q.Get("follow") != "true" || q.Get("timestamps") != "true" || q.Get("container") != "app" {
					t.Errorf("unexpected log options: %v", q)
				}
				if n == 1 {
					if q.Get("tailLines") != "100" || q.Has("sinceTime") {
						t.Errorf("initial options: %v", q)
					}
				} else {
					if q.Has("tailLines") || len(q["sinceTime"]) != 1 {
						t.Errorf("reconnect options: %v", q)
					}
					// Inspect the real query, including preservation of fractional
					// seconds and an inclusive (not +1ns) boundary.
					if n == 2 && len(tc.streams[0]) > 0 && q.Get("sinceTime") != "2026-09-07T10:00:00.5Z" {
						t.Errorf("sinceTime = %q", q.Get("sinceTime"))
					}
					var since metav1.Time
					if err := since.UnmarshalQueryParameter(q.Get("sinceTime")); err != nil {
						t.Errorf("server cannot decode sinceTime: %v", err)
					}
				}
				if tc.failOpen && n == 2 {
					http.Error(w, "temporary failure", http.StatusInternalServerError)
					return
				}
				lines := tc.streams[min(n-1, len(tc.streams)-1)]
				if tc.failRead && n == 1 {
					w.Header().Set("Content-Length", "1000")
				}
				for _, line := range lines {
					fmt.Fprintln(w, line)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			tail := int64(100)
			initial, err := openStream(ctx, cs, "ns", "pod", "app", &tail, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			ls := &LogStreamer{Out: make(chan LogLine, 256)}
			go runStream(ctx, cs, ls, initial, "ns", "pod", "app", 100)
			var lines []string
			var fatal, eos int
			for msg := range ls.Out {
				if msg.Line != "" {
					lines = append(lines, msg.Line)
				}
				if msg.Err != "" && !msg.Reconnecting {
					fatal++
				}
				if msg.EOS {
					eos++
				}
			}
			if ctx.Err() != nil {
				t.Fatal("stream did not terminate within retry budget")
			}
			if !reflect.DeepEqual(lines, tc.want) {
				t.Errorf("lines = %q, want %q", lines, tc.want)
			}
			if (fatal == 1) != tc.wantError || fatal > 1 || eos != 1 {
				t.Errorf("fatal = %d (want error %v), EOS = %d", fatal, tc.wantError, eos)
			}
			if got := int(requests.Load()); got != tc.requests {
				t.Errorf("requests = %d, want %d", got, tc.requests)
			}
		})
	}
}

func TestLogCursorOverlap(t *testing.T) {
	a := time.Date(2026, 9, 7, 10, 0, 0, 500000000, time.UTC)
	var cursor logCursor
	for i, stream := range [][]struct {
		at   time.Time
		line string
		want bool
	}{
		{{a, "a", true}, {a, "a", true}, {a, "b", true}},
		{{a.Add(-time.Nanosecond), "old", false}, {a, "a", false}},
		{{a, "b", false}, {a, "a", false}, {a, "a", false}, {a, "a", true}},
		{{a, "a", false}, {a, "a", false}, {a, "a", false}, {a, "b", false}, {a.Add(time.Nanosecond), "new", true}},
	} {
		cursor.beginStream()
		for _, line := range stream {
			if got := cursor.accept(line.at, line.line); got != line.want {
				t.Fatalf("stream %d, line %q: accept = %v, want %v", i, line.line, got, line.want)
			}
		}
	}
	if len(cursor.seen) != 1 || cursor.seen["new"] != 1 {
		t.Fatalf("retained obsolete timestamp counts: %v", cursor.seen)
	}
}

func TestContainerLogsComplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   corev1.RestartPolicy
		override corev1.ContainerRestartPolicy
		exitCode int32
		kind     string
		phase    corev1.PodPhase
		rules    bool
		waiting  bool
		want     bool
	}{
		{name: "succeeded pod", phase: corev1.PodSucceeded, want: true},
		{name: "failed pod", phase: corev1.PodFailed, want: true},
		{name: "always restarts", policy: corev1.RestartPolicyAlways},
		{name: "default policy restarts"},
		{name: "never restarts", policy: corev1.RestartPolicyNever, exitCode: 1, want: true},
		{name: "successful on-failure", policy: corev1.RestartPolicyOnFailure, want: true},
		{name: "failed on-failure restarts", policy: corev1.RestartPolicyOnFailure, exitCode: 1},
		{name: "waiting with previous termination", policy: corev1.RestartPolicyNever, waiting: true},
		{name: "container always overrides never", policy: corev1.RestartPolicyNever, override: corev1.ContainerRestartPolicyAlways},
		{name: "container never overrides always", policy: corev1.RestartPolicyAlways, override: corev1.ContainerRestartPolicyNever, want: true},
		{name: "successful init", kind: "init", policy: corev1.RestartPolicyAlways, want: true},
		{name: "failed init retries", kind: "init", policy: corev1.RestartPolicyAlways, exitCode: 1},
		{name: "failed init never restarts", kind: "init", policy: corev1.RestartPolicyNever, exitCode: 1, want: true},
		{name: "sidecar restarts", kind: "init", policy: corev1.RestartPolicyNever, override: corev1.ContainerRestartPolicyAlways},
		{name: "ephemeral never restarts", kind: "ephemeral", policy: corev1.RestartPolicyAlways, want: true},
		{name: "unknown target", kind: "unknown", policy: corev1.RestartPolicyNever},
		{name: "rules defer to kubelet", policy: corev1.RestartPolicyNever, rules: true},
		{name: "terminal pod with rules", phase: corev1.PodSucceeded, rules: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := corev1.Container{Name: "app"}
			if tc.override != "" {
				c.RestartPolicy = &tc.override
			}
			status := corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: tc.exitCode},
			}}
			if tc.waiting {
				status.LastTerminationState = status.State
				status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			}
			p := &corev1.Pod{Spec: corev1.PodSpec{RestartPolicy: tc.policy}, Status: corev1.PodStatus{Phase: tc.phase}}
			switch tc.kind {
			case "init":
				p.Spec.InitContainers = []corev1.Container{c}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			case "ephemeral":
				p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{status}
			case "unknown":
				status.Name = "other"
				fallthrough
			default:
				p.Spec.Containers = []corev1.Container{c}
				p.Status.ContainerStatuses = []corev1.ContainerStatus{status}
			}
			if tc.rules {
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "other", RestartPolicyRules: []corev1.ContainerRestartRule{{}}})
			}
			if got := containerLogsComplete(p, "app"); got != tc.want {
				t.Fatalf("complete = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogStreamCancellation(t *testing.T) {
	for _, stage := range []string{"reading", "backoff", "status", "reopening"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := make(chan struct{})
			cs := logTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if stage == "reopening" && !strings.HasSuffix(r.URL.Path, "/log") {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(corev1.Pod{})
					return
				}
				if stage == "status" && strings.HasSuffix(r.URL.Path, "/log") {
					return
				}
				if stage == "backoff" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(corev1.Pod{})
					return
				}
				close(reached)
				if stage == "reading" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			})
			var initial io.ReadCloser = io.NopCloser(strings.NewReader(""))
			if stage == "reading" {
				var err error
				initial, err = openStream(ctx, cs, "ns", "pod", "app", nil, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
			} else if stage != "status" {
				initial = io.NopCloser(strings.NewReader("2026-09-07T10:00:00.5Z line\n"))
			}
			ls := &LogStreamer{Out: make(chan LogLine, 256)}
			go runStream(ctx, cs, ls, initial, "ns", "pod", "app", 100)
			if stage == "backoff" {
				for msg := range ls.Out {
					if msg.Reconnecting {
						break
					}
				}
			} else {
				select {
				case <-reached:
				case <-time.After(5 * time.Second):
					t.Fatal("did not reach cancellation stage")
				}
			}
			cancel()
			timeout := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-ls.Out:
					if !ok {
						return
					}
				case <-timeout:
					t.Fatal("stream did not close after cancellation")
				}
			}
		})
	}
}
