package ui

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/model"
)

func tableBenchmarkModel(count int, overlay string) Model {
	m := New("bench", model.NewStore(), []string{"bench"})
	m.width, m.height = 160, 40
	m.syncedPods = true
	for i := 0; i < count; i++ {
		uid := types.UID(fmt.Sprintf("pod-%05d", i))
		m.pods[uid] = podRow{UID: uid, Name: string(uid), Namespace: "default", Phase: corev1.PodRunning, CreatedAt: time.Unix(1, 0)}
	}
	switch overlay {
	case "logs":
		m.logs.open = true
		m.logs.lines = []string{"one log line"}
	case "help":
		m.helpOpen = true
	case "describe":
		m.describe.open = true
		m.describe.result.YAML = "kind: Pod"
	}
	return m
}

func BenchmarkViewResourceScale(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, overlay := range []string{"table", "logs", "help", "describe"} {
			b.Run(fmt.Sprintf("pods=%d/%s", count, overlay), func(b *testing.B) {
				m := tableBenchmarkModel(count, overlay)
				m.View()
				b.ReportAllocs()
				for b.Loop() {
					m.View()
				}
			})
		}
	}
}

func BenchmarkViewAfterPodUpdate(b *testing.B) {
	for _, overlay := range []string{"table", "logs"} {
		for _, sortKey := range []SortKey{SortName, SortRestarts} {
			b.Run(overlay+"/sort="+sortKey.label(), func(b *testing.B) {
				m := tableBenchmarkModel(10000, overlay)
				m.sortKey = sortKey
				m.View()
				event := PodEventMsg{Context: "bench", UID: "pod-00000", Name: "pod-00000", Namespace: "default", Phase: corev1.PodRunning}
				b.ReportAllocs()
				for b.Loop() {
					event.Restarts++
					updated, _ := m.Update(event)
					m = updated.(Model)
					m.View()
				}
			})
		}
	}
}
