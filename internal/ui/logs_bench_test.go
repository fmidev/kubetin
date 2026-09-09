package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func logSearchBenchmarkModel(count int, term string) Model {
	m := New("bench", model.NewStore(), []string{"bench"})
	m.logs.open = true
	m.logs.cap = count
	m.logs.follow = true
	m.logs.lines = make([]string, count)
	for i := range m.logs.lines {
		m.logs.lines[i] = strings.Repeat("INFO request accepted ", 8)
	}
	m.logs.searchTerm = term
	m.recomputeLogsMatches()
	return m
}

func BenchmarkLogSearchAppend(b *testing.B) {
	for _, count := range []int{defaultLogCap, fullLogCap} {
		for _, term := range []string{"", "request", "missing"} {
			b.Run(fmt.Sprintf("lines=%d/search=%s", count, term), func(b *testing.B) {
				m := logSearchBenchmarkModel(count, term)
				batch := make([]string, 64)
				for i := range batch {
					batch[i] = m.logs.lines[0]
				}
				b.ReportAllocs()
				for b.Loop() {
					m.applyLogLines(batch)
				}
			})
		}
	}
}

func BenchmarkLogSearchRender(b *testing.B) {
	for _, count := range []int{defaultLogCap, fullLogCap} {
		b.Run(fmt.Sprintf("lines=%d", count), func(b *testing.B) {
			m := logSearchBenchmarkModel(count, "request")
			b.ReportAllocs()
			for b.Loop() {
				m.renderLogs(160, 40)
			}
		})
	}
}
