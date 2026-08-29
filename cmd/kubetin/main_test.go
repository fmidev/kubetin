package main

import (
	"context"
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func TestPickWatchContextExplicit(t *testing.T) {
	store := model.NewStore()
	contexts := []string{"alpha", "beta"}

	// An explicit -watch is honoured without waiting on a probe: the
	// whole point is opening a cluster that may well be down.
	got, err := pickWatchContext(context.Background(), store, "beta", contexts)
	if err != nil || got != "beta" {
		t.Fatalf("want beta/nil, got %q/%v", got, err)
	}

	if _, err := pickWatchContext(context.Background(), store, "nope", contexts); err == nil {
		t.Fatal("unknown -watch context should be an error")
	}
}

func TestPickWatchContextPrefersHealthy(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})
	store.ApplyProbe("beta", model.ProbeFields{Reach: model.ReachHealthy})

	got, err := pickWatchContext(context.Background(), store, "", []string{"alpha", "beta"})
	if err != nil || got != "beta" {
		t.Fatalf("want the healthy context, got %q/%v", got, err)
	}
}

func TestPickWatchContextCancelled(t *testing.T) {
	store := model.NewStore()
	store.ApplyProbe("alpha", model.ProbeFields{Reach: model.ReachUnreachable})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Ctrl-C during the startup wait must stay an error. runTUI only
	// falls back to the first context on the probe deadline; a
	// cancelled startup should not silently open a UI the user just
	// asked to abort.
	if _, err := pickWatchContext(ctx, store, "", []string{"alpha"}); err == nil {
		t.Fatal("cancelled context should be an error")
	}
}
