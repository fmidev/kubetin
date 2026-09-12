package ui

import (
	"fmt"
	"testing"
)

func BenchmarkClusterDashboardScale(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, view := range []View{ViewPods, ViewCluster} {
			b.Run(fmt.Sprintf("pods=%d/view=%d", count, view), func(b *testing.B) {
				m := tableBenchmarkModel(count, "table")
				m.view = view
				for uid, p := range m.pods {
					p.HasMetrics = true
					p.CPUMilli = int64(len(uid) * 17)
					p.MemBytes = 1 << 30
					m.pods[uid] = p
				}
				m.View()
				b.ReportAllocs()
				for b.Loop() {
					m.View()
				}
			})
		}
	}
}
