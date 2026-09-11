package ui

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

func TestLogSearchMatchesFullScan(t *testing.T) {
	for _, capacity := range []int{0, 1, 7, 64} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			m := Model{}
			m.logs.cap = capacity
			m.logs.follow = true
			rng := rand.New(rand.NewSource(89))
			terms := []string{"", "request", "REQUEST", "missing", "ÉTÉ", "error"}
			input := []string{"INFO request accepted", "error request", "noise", "Été", "\x1b[31mERROR\x1b[0m", "request\x1b[2J"}
			var retained []string
			for step := 0; step < 300; step++ {
				if step%11 == 0 {
					m.logs.searchTerm = terms[rng.Intn(len(terms))]
					m.recomputeLogsMatches()
				}
				batch := make([]string, rng.Intn(capacity+4))
				for i := range batch {
					batch[i] = input[rng.Intn(len(input))]
					retained = append(retained, sanitizeLogLine(batch[i]))
				}
				if len(retained) > capacity {
					retained = retained[len(retained)-capacity:]
				}
				m.applyLogLines(batch)
				if !slices.Equal(m.logs.lines, retained) {
					t.Fatalf("step %d: retained lines differ", step)
				}
				var want, got []int
				if m.logs.searchTerm != "" {
					for i, line := range retained {
						if strings.Contains(strings.ToLower(line), strings.ToLower(m.logs.searchTerm)) {
							want = append(want, i)
						}
					}
				}
				for _, seq := range m.logs.searchMatches {
					got = append(got, seq-m.logs.lineBase)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("step %d term %q: matches %v, want %v", step, m.logs.searchTerm, got, want)
				}
				if len(got) > 0 && (m.logs.searchIdx < 0 || m.logs.searchIdx >= len(got)) {
					t.Fatalf("step %d: cursor %d outside %d matches", step, m.logs.searchIdx, len(got))
				}
			}
		})
	}
}

func TestLogSearchSelectionSurvivesEviction(t *testing.T) {
	m := Model{height: 12}
	m.logs.cap, m.logs.follow, m.logs.searchTerm = 8, true, "match"
	m.applyLogLines([]string{"match 0", "match 1", "match 2", "match 3", "match 4", "match 5", "match 6", "match 7"})
	m.logs.searchIdx = 5
	m.applyLogLines([]string{"match 8", "noise 9", "match 10"})
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 5 {
		t.Fatalf("selected line changed to %d after eviction, want 5", got)
	}
	m.stepLogsMatch(1)
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 6 || m.logs.scroll != 3 {
		t.Fatalf("next match = %d, scroll = %d; want 6, 3", got, m.logs.scroll)
	}
	m.stepLogsMatch(-1)
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 5 {
		t.Fatalf("previous match = %d, want 5", got)
	}
	m.applyLogLines([]string{"noise 11", "noise 12", "noise 13", "noise 14"})
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 7 {
		t.Fatalf("expired selection should move to oldest match: got %d, want 7", got)
	}
	m.stepLogsMatch(-1)
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 10 {
		t.Fatalf("previous should wrap to last match: got %d, want 10", got)
	}
	m.stepLogsMatch(1)
	if got := m.logs.searchMatches[m.logs.searchIdx]; got != 7 {
		t.Fatalf("next should wrap to first match: got %d, want 7", got)
	}
	m.applyLogLines([]string{"no", "no", "no", "no", "no", "no", "no", "no", "no"})
	m.stepLogsMatch(1)
	if len(m.logs.searchMatches) != 0 || m.logs.searchIdx != 0 {
		t.Fatal("all expired matches must clear the selection")
	}
}

func TestLogSearchClearAndRestart(t *testing.T) {
	for _, focused := range []bool{false, true} {
		t.Run(fmt.Sprint(focused), func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.logs.cap, m.logs.follow, m.logs.searchTerm = 2, true, "match"
			m.applyLogLines([]string{"old", "match 1", "match 2"})
			m.logs.searchFocused = focused
			out, _ := m.handleLogsKey(tea.KeyMsg{Type: tea.KeyEsc})
			m = out.(Model)
			m.applyLogLine("match 3")
			m.logs.searchFocused = true
			out, _ = m.handleLogsSearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("match")})
			m = out.(Model)
			if len(m.logs.searchMatches) != 2 {
				t.Fatal("re-entering the same term must search all retained lines")
			}
			m.startDashboardLogs()
			if len(m.logs.lines) != 0 || len(m.logs.searchMatches) != 0 {
				t.Fatal("dashboard without a pod must clear the buffer and its matches")
			}
			m.applyLogLine("match after clear")
			if len(m.logs.searchMatches) != 1 {
				t.Fatal("search must resume after clearing the dashboard buffer")
			}

			oldSession := m.logs.session
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { return req }
			m.beginLogStreamTail(cluster.DescribeRef{Name: "new-pod"}, "app", logTailAll)
			defer m.logs.cancel()
			out, _ = m.Update(LogLinesMsg{Session: oldSession, Lines: []string{"match stale"}})
			m = out.(Model)
			if len(m.logs.lines) != 0 || len(m.logs.searchMatches) != 0 || m.logs.lineBase != 0 {
				t.Fatal("restarted stream retained old data or accepted a stale batch")
			}
			m.logs.searchTerm = "match"
			m.applyLogLine("match fresh")
			if !slices.Equal(m.logs.searchMatches, []int{0}) {
				t.Fatalf("restarted search = %v, want first new line", m.logs.searchMatches)
			}
		})
	}
}

func TestDashboardWithoutPodsInvalidatesOldLogStream(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			m := dashDeployModel(160, 40, nil)
			var started []LogStartMsg
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg {
				started = append(started, req)
				return nil
			}
			pod := m.deploymentPods(m.deployments["dep-uid"])[0]
			opened, start := m.openDashboard(podRefFor(pod), pod.UID)
			m = opened.(Model)
			defer m.logs.cancel()
			oldSession := m.logs.session
			m.logs.searchTerm = "match"
			if running {
				start()
				updated, _ := m.Update(LogLinesMsg{Session: oldSession, Lines: []string{"match old"}})
				m = updated.(Model)
			}

			clear(m.pods)
			popped, _ := m.popDashboard()
			m = popped.(Model)
			if m.dashboard.logRef.Name != "" || m.logs.err != "no pod available to stream logs from" {
				t.Fatal("returning to the empty Deployment did not clear its log target")
			}
			if m.logs.session == oldSession || m.logs.streaming || m.logs.cancel != nil {
				t.Error("empty target did not invalidate and stop the old stream")
			}
			if running {
				if started[0].Context.Err() != context.Canceled {
					t.Error("running stream was not canceled")
				}
			} else {
				start()
				if len(started) != 0 {
					t.Error("queued stream started after its target disappeared")
				}
			}

			for _, msg := range []tea.Msg{
				LogLineMsg{Session: oldSession, Line: "match stale"},
				LogLinesMsg{Session: oldSession, Lines: []string{"match stale batch"}},
				LogErrorMsg{Session: oldSession, Err: "stale error"},
				LogReconnectingMsg{Session: oldSession},
				LogEOSMsg{Session: oldSession},
			} {
				updated, _ := m.Update(msg)
				got := updated.(Model).logs
				if len(got.lines) != 0 || len(got.searchMatches) != 0 || got.finished || got.reconnecting || got.err != "no pod available to stream logs from" {
					t.Errorf("stale %T mutated the cleared log state", msg)
				}
			}
		})
	}
}

func TestPausedLogsStayPinnedAtCapacity(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.logs.cap, m.logs.follow = 12, true
	for i := 0; i < 12; i++ {
		m.applyLogLine(fmt.Sprintf("line %02d", i))
	}
	m.logs.follow, m.logs.scroll = false, 3
	before := m.renderDashLogs(60, 3)
	m.applyLogLines([]string{"line 12", "line 13"})
	if after := m.renderDashLogs(60, 3); after != before {
		t.Fatalf("paused viewport moved after head eviction:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	m.applyLogLines(make([]string, 20))
	if m.logs.scroll >= len(m.logs.lines) || m.logs.follow {
		t.Fatal("expired pause anchor must stay paused within retained history")
	}
}

func TestLogSearchRenderAfterEviction(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.logs.cap, m.logs.follow, m.logs.searchTerm = 80, true, "match"
	for i := 0; i < 160; i++ {
		line := fmt.Sprintf("line %03d", i)
		if i%3 == 0 {
			line += " \x1b[31mMATCH\x1b[0m"
		}
		m.applyLogLine(line)
	}
	m.logs.searchIdx = 12
	for _, scroll := range []int{0, 25, 60} {
		m.logs.scroll = scroll
		reference := m
		reference.logs.lineBase = 0
		reference.logs.searchMatches = nil
		for i, line := range reference.logs.lines {
			if strings.Contains(strings.ToLower(line), "match") {
				reference.logs.searchMatches = append(reference.logs.searchMatches, i)
			}
		}
		if got, want := m.renderLogs(100, 20), reference.renderLogs(100, 20); got != want {
			t.Fatalf("scroll %d: highlighting differs from a fresh buffer:\ngot:\n%s\nwant:\n%s", scroll, got, want)
		}
	}
}
