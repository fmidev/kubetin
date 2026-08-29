package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmidev/kubetin/internal/model"
)

// TestMain doubles as the entry point for the subprocess cases below.
// With KUBETIN_TEST_MAIN set it runs the real main() — fd-2 silencing,
// os.Exit and all — which is the only way to prove a diagnostic really
// reaches the terminal. An in-process fake would have happily "passed"
// on the original bug, where the message was written to a live
// os.Stderr that had been dup2'd onto /dev/null underneath it.
func TestMain(m *testing.M) {
	if _, ok := os.LookupEnv("KUBETIN_TEST_MAIN"); ok {
		os.Args = append([]string{"kubetin"}, strings.Fields(os.Getenv("KUBETIN_TEST_ARGS"))...)
		if d := os.Getenv("KUBETIN_TEST_PICK_TIMEOUT"); d != "" {
			parsed, err := time.ParseDuration(d)
			if err != nil {
				panic(err)
			}
			watchPickTimeout = parsed
		}
		main()
		return
	}
	os.Exit(m.Run())
}

// runMain re-execs the test binary as kubetin against an isolated
// kubeconfig, trust list and state dir.
func runMain(t *testing.T, home, kubeconfig string, args ...string) (stderr string, code int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(),
		"KUBETIN_TEST_MAIN=1",
		"KUBETIN_TEST_PICK_TIMEOUT=50ms",
		"KUBETIN_TEST_ARGS="+strings.Join(args, " "),
		"KUBECONFIG="+kubeconfig,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run kubetin %v: %v", args, err)
	}
	return errb.String(), code
}

// writeKubeconfig drops cfg in a temp home and blesses it, so the run
// under test isn't intercepted by the trust prompt.
func writeKubeconfig(t *testing.T, cfg string) (home, path string) {
	t.Helper()
	home = t.TempDir()
	path = filepath.Join(home, "kubeconfig.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := runMain(t, home, path, "-trust"); code != 0 {
		t.Fatalf("-trust exited %d", code)
	}
	return home, path
}

const unreachableKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: unreachable
  cluster:
    server: https://198.51.100.7:6443
    insecure-skip-tls-verify: true
contexts:
- name: dead-cluster
  context: {cluster: unreachable, user: nobody}
current-context: dead-cluster
users:
- name: nobody
  user: {token: deadbeef}
`

// A startup that fails must say so on the terminal. main() dup2's
// /dev/null over fd 2 before runTUI to keep exec credential plugins off
// the alt-screen, so every fatal path after that point has to restore
// it first — this is the regression that made kubetin exit 1 with no
// output anywhere, not even debug.log.
func TestStartupFailuresReachStderr(t *testing.T) {
	t.Run("unknown -watch context", func(t *testing.T) {
		home, cfg := writeKubeconfig(t, unreachableKubeconfig)

		stderr, code := runMain(t, home, cfg, "-watch", "nope")
		if code == 0 {
			t.Fatal("an unknown -watch context should be fatal")
		}
		if !strings.Contains(stderr, `context "nope" not found in kubeconfig`) {
			t.Fatalf("diagnostic never reached stderr; got %q", stderr)
		}
	})

	// The headline fix: an unreachable fleet is no longer fatal. The
	// TUI can't attach to a terminal under `go test` so the run still
	// ends unhappily, but debug.log records which branch startup took.
	t.Run("unreachable fleet opens anyway", func(t *testing.T) {
		home, cfg := writeKubeconfig(t, unreachableKubeconfig)

		stderr, _ := runMain(t, home, cfg)
		if strings.Contains(stderr, "no healthy cluster appeared") {
			t.Fatalf("an unreachable fleet must not be fatal; got %q", stderr)
		}

		log, err := os.ReadFile(filepath.Join(home, "state", "kubetin", "debug.log"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(log), `opening on "dead-cluster"`) {
			t.Fatalf("startup did not fall back to the first context; log:\n%s", log)
		}
	})

	t.Run("kubeconfig with no contexts", func(t *testing.T) {
		home, cfg := writeKubeconfig(t, "apiVersion: v1\nkind: Config\ncontexts: []\n")

		stderr, code := runMain(t, home, cfg)
		if code == 0 {
			t.Fatal("a kubeconfig defining no contexts should be fatal")
		}
		if !strings.Contains(stderr, "no contexts defined") {
			t.Fatalf("diagnostic never reached stderr; got %q", stderr)
		}
	})
}

func TestPickWatchContextExplicit(t *testing.T) {
	store := model.NewStore()
	contexts := []string{"alpha", "beta"}

	// An explicit -watch is honoured without waiting on a probe: the
	// whole point is opening a cluster that may well be down.
	got, err := pickWatchContext(context.Background(), store, "beta", contexts, watchPickTimeout)
	if err != nil || got != "beta" {
		t.Fatalf("want beta/nil, got %q/%v", got, err)
	}

	if _, err := pickWatchContext(context.Background(), store, "nope", contexts, watchPickTimeout); err == nil {
		t.Fatal("unknown -watch context should be an error")
	}
}

func TestPickWatchContextPrefersHealthy(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})
	store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachHealthy})

	got, err := pickWatchContext(context.Background(), store, "", []string{"alpha", "beta"}, watchPickTimeout)
	if err != nil || got != "beta" {
		t.Fatalf("want the healthy context, got %q/%v", got, err)
	}
}

// The two ways of running out of options are not interchangeable:
// runTUI opens on contexts[0] after a probe timeout, but must not do
// that when the user just hit Ctrl-C.
func TestPickWatchContextTimeoutIsRecoverable(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})

	_, err := pickWatchContext(context.Background(), store, "", []string{"alpha"}, 10*time.Millisecond)
	if !errors.Is(err, errNoHealthyCluster) {
		t.Fatalf("timeout must be recoverable so runTUI falls back; got %v", err)
	}
}

// The store is only scanned every 500ms, so a probe can land in the gap
// between the last scan and the deadline firing. Giving up there would
// open contexts[0] with a healthy cluster already in the store.
//
// Deterministic by construction: the 200ms deadline is the only case
// that can fire before the 500ms tick, and beta turns healthy well
// before it.
func TestPickWatchContextHealthyOnTheDeadline(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})
	store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachUnreachable})

	go func() {
		time.Sleep(50 * time.Millisecond)
		store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachHealthy})
	}()

	got, err := pickWatchContext(context.Background(), store, "", []string{"alpha", "beta"}, 200*time.Millisecond)
	if err != nil || got != "beta" {
		t.Fatalf("want beta/nil, got %q/%v", got, err)
	}
}

// Cancellation and the deadline can be ready at the same time, and
// select picks a ready case at random — so the classification has to
// hold whichever branch wins the toss, not just the tidy one.
func TestPickWatchContextCancelledBeatsDeadline(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 200; i++ {
		_, err := pickWatchContext(ctx, store, "", []string{"alpha"}, time.Nanosecond)
		if errors.Is(err, errNoHealthyCluster) {
			t.Fatalf("run %d: aborted startup classified as a recoverable timeout", i)
		}
	}
}

func TestPickWatchContextCancelledIsFatal(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pickWatchContext(ctx, store, "", []string{"alpha"}, watchPickTimeout)
	if err == nil {
		t.Fatal("cancelled context should be an error")
	}
	if errors.Is(err, errNoHealthyCluster) {
		t.Fatal("a cancelled startup must not be mistaken for a probe timeout")
	}
}
