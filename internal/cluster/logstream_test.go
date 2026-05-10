package cluster

import (
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// parseLogTimestamp must extract kubelet's RFC3339Nano prefix or
// report ok=false. Resume-after-reconnect correctness depends on this.
func TestParseLogTimestamp(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		want time.Time
	}{
		{
			"normal",
			"2026-05-10T19:30:01.123456789Z some log content",
			true,
			time.Date(2026, 5, 10, 19, 30, 1, 123456789, time.UTC),
		},
		{
			"no nanos",
			"2026-05-10T19:30:01Z log",
			true,
			time.Date(2026, 5, 10, 19, 30, 1, 0, time.UTC),
		},
		{"no space", "2026-05-10T19:30:01Zlog", false, time.Time{}},
		{"plain text", "just a regular log line", false, time.Time{}},
		{"empty", "", false, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseLogTimestamp(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (line=%q)", ok, tc.ok, tc.line)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("time = %v, want %v", got, tc.want)
			}
		})
	}
}

// isFatalStreamErr distinguishes auth/not-found (don't retry) from
// transient network errors (retry). Wrong classification either
// retries forever on bad creds or gives up on every TCP blip.
func TestIsFatalStreamErr(t *testing.T) {
	gv := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unauthorized", apierrors.NewUnauthorized("nope"), true},
		{"forbidden", apierrors.NewForbidden(gv, "p", errors.New("rbac")), true},
		{"not found", apierrors.NewNotFound(gv, "p"), true},
		{"plain network error", errors.New("read tcp: connection reset by peer"), false},
		{"timeout", errors.New("i/o timeout"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFatalStreamErr(tc.err); got != tc.want {
				t.Errorf("isFatalStreamErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	_ = metav1.Time{} // metav1 imported only by the package code under test
}
