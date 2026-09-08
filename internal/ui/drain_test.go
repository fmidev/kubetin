package ui

import (
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fmidev/kubetin/internal/model"
)

func TestDrainErrorsStripTerminalControls(t *testing.T) {
	errText := "denied\nsecond line\r\t\x1b[31mred\x1b[0m\x1b]0;title\x07\u009b done"
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"start", DrainStartMsg{Context: "alpha", Node: "worker", Err: errText}},
		{"done", DrainDoneMsg{Context: "alpha", Node: "worker", Err: errText}},
		{"blocked", DrainProgressMsg{Context: "alpha", Node: "worker", Phase: "blocked", Pod: "default/pod", Err: errText}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.drainProgress = drainProgressState{open: true, context: "alpha", node: "worker"}
			updated, _ := m.Update(tc.msg)
			m = updated.(Model)
			got := m.toast
			if tc.name == "blocked" {
				if len(m.drainProgress.blocked) != 1 {
					t.Fatalf("expected one blocked pod, got %+v", m.drainProgress.blocked)
				}
				got = m.drainProgress.blocked[0]
			}
			if strings.ContainsFunc(got, unicode.IsControl) {
				t.Fatalf("terminal control survived in UI state: %q", got)
			}
			if !strings.Contains(got, "denied second line") || !strings.Contains(got, "done") {
				t.Fatalf("error detail lost: %q", got)
			}
		})
	}
}

func TestDrainBlockedLabelsPreserveCause(t *testing.T) {
	for _, cause := range []string{"UID precondition failed", "Forbidden", "PDB blocked after 5 retries"} {
		t.Run(cause, func(t *testing.T) {
			m := New("alpha", model.NewStore(), []string{"alpha"})
			m.drainProgress = drainProgressState{open: true, context: "alpha", node: "worker"}
			updated, _ := m.applyDrainProgress(DrainProgressMsg{
				Context: "alpha", Node: "worker", Phase: "blocked", Pod: "default/pod", Total: 1, Err: cause,
			})
			m = updated.(Model)
			view := m.renderDrainProgress(120, 40)
			if !strings.Contains(view, "1 pod(s) blocked:") || !strings.Contains(view, cause) || strings.Contains(view, "blocked by PDB") {
				t.Fatalf("progress misrepresents cause %q: %s", cause, view)
			}
			updated, _ = m.applyDrainDone(DrainDoneMsg{
				Context: "alpha", Node: "worker", Total: 1, Blocked: []string{"default/pod (" + cause + ")"}, Remaining: []string{"default/pod"},
			})
			m = updated.(Model)
			if !strings.Contains(m.toast, "(1 blocked)") || strings.Contains(m.toast, "PDB") {
				t.Fatalf("summary misrepresents cause %q: %s", cause, m.toast)
			}
			if view := m.renderDrainProgress(120, 40); !strings.Contains(view, cause) {
				t.Fatalf("incomplete result lost cause %q: %s", cause, view)
			}
		})
	}
}

func TestDrainWaitingDoesNotReportCompletion(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.drainProgress = drainProgressState{open: true, context: "alpha", node: "worker"}
	updated, _ := m.applyDrainProgress(DrainProgressMsg{
		Context: "alpha", Node: "worker", Phase: "waiting", Done: 0, Total: 1,
	})
	m = updated.(Model)
	view := m.renderDrainProgress(120, 40)
	if !m.drainProgress.open || !strings.Contains(view, "0 / 1 terminated") || !strings.Contains(view, "waiting for pod termination") {
		t.Fatalf("accepted eviction reported incorrectly: %s", view)
	}
}

func TestDrainIncompleteResultRemainsVisible(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.drainProgress = drainProgressState{open: true, context: "alpha", node: "worker"}
	updated, _ := m.applyDrainDone(DrainDoneMsg{
		Context: "alpha", Node: "worker", Done: 1, Total: 3,
		Err: "context deadline exceeded\n\x1b[31m", Remaining: []string{"default/terminating", "default/unattempted"},
	})
	m = updated.(Model)
	view := m.renderDrainProgress(120, 40)
	for _, want := range []string{"Drain incomplete: 1 / 3 terminated", "context deadline exceeded", "default/terminating", "default/unattempted", "esc to close"} {
		if !strings.Contains(view, want) {
			t.Errorf("incomplete result missing %q: %s", want, view)
		}
	}
	if !m.drainProgress.open || strings.ContainsFunc(m.drainProgress.err, unicode.IsControl) || strings.Contains(m.toast, "✓") {
		t.Fatalf("incorrect incomplete state: %+v toast=%q", m.drainProgress, m.toast)
	}
	updated, _ = m.handleDrainProgressKey(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(Model).drainProgress.open {
		t.Fatal("escape did not dismiss incomplete result")
	}
}

func TestDrainSuccessClosesProgress(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.drainProgress = drainProgressState{open: true, context: "alpha", node: "worker"}
	updated, _ := m.applyDrainDone(DrainDoneMsg{Context: "alpha", Node: "worker", Done: 2, Total: 2})
	m = updated.(Model)
	if m.drainProgress.open || !strings.Contains(m.toast, "✓ Drained worker: 2/2") {
		t.Fatalf("successful drain did not complete: open=%v toast=%q", m.drainProgress.open, m.toast)
	}
}
