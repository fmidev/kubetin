package cluster

import (
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func TestReachAccumulator_FirstSampleAcceptedImmediately(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnknown, pending: model.ReachUnknown}
	got := a.update(model.ReachAuthFailed)
	if got != model.ReachAuthFailed {
		t.Fatalf("first sample should be accepted; got %v want %v", got, model.ReachAuthFailed)
	}
}

func TestReachAccumulator_HealthyPromotesInstantly(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnreachable, pending: model.ReachUnreachable}
	got := a.update(model.ReachHealthy)
	if got != model.ReachHealthy {
		t.Fatalf("Healthy should promote immediately; got %v", got)
	}
}

func TestReachAccumulator_DemotionRequiresTwoSamples(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachHealthy, pending: model.ReachHealthy}

	// First demotion sample shouldn't change visible state.
	got := a.update(model.ReachUnreachable)
	if got != model.ReachHealthy {
		t.Fatalf("after one demotion sample, visible should stay Healthy; got %v", got)
	}

	// Second consecutive demotion flips.
	got = a.update(model.ReachUnreachable)
	if got != model.ReachUnreachable {
		t.Fatalf("after two demotion samples, visible should flip; got %v", got)
	}
}

func TestReachAccumulator_PendingResetsOnNonMatch(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachHealthy, pending: model.ReachHealthy}

	// One Unreachable sample (count=1).
	a.update(model.ReachUnreachable)

	// Now an AuthFailed — different non-healthy. pendingCount should
	// reset to 1, not promote AuthFailed instantly.
	got := a.update(model.ReachAuthFailed)
	if got != model.ReachHealthy {
		t.Fatalf("after differing demotion sample, visible should stay Healthy; got %v", got)
	}

	// One more AuthFailed flips.
	got = a.update(model.ReachAuthFailed)
	if got != model.ReachAuthFailed {
		t.Fatalf("after two AuthFailed samples, visible should be AuthFailed; got %v", got)
	}
}

func TestReachAccumulator_MatchingSamplesNeverDemoteFromUnknown(t *testing.T) {
	a := &reachAccumulator{visible: model.ReachUnknown, pending: model.ReachUnknown}

	// First sample: anything is accepted.
	a.update(model.ReachUnreachable)

	// Identical sample: still Unreachable.
	got := a.update(model.ReachUnreachable)
	if got != model.ReachUnreachable {
		t.Fatalf("repeated samples should not demote; got %v", got)
	}
}
