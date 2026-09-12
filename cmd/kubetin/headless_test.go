package main

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/fmidev/kubetin/internal/cluster"
)

func TestHeadlessCountsCoalescedPodState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := cluster.NewPodWatcher("alpha", 0)
		var count atomic.Int64
		go consumePodEvents(ctx, w, &count)
		for _, tc := range []struct {
			event cluster.PodEvent
			want  int64
		}{
			{cluster.PodEvent{Kind: cluster.PodSynced}, 0},
			{cluster.PodEvent{Kind: cluster.PodSyncDelayed}, 0},
			{cluster.PodEvent{Kind: cluster.PodUpdated, UID: "old", Name: "api"}, 1},
			{cluster.PodEvent{Kind: cluster.PodAdded, UID: "old", Name: "api"}, 1},
			{cluster.PodEvent{Kind: cluster.PodDeleted, UID: "unseen"}, 1},
			{cluster.PodEvent{Kind: cluster.PodDeleted, UID: "old", Name: "api"}, 0},
			{cluster.PodEvent{Kind: cluster.PodUpdated, UID: "replacement", Name: "api"}, 1},
		} {
			w.Out <- tc.event
			synctest.Wait()
			if got := count.Load(); got != tc.want {
				t.Fatalf("after %+v count=%d, want %d", tc.event, got, tc.want)
			}
		}
	})
}
