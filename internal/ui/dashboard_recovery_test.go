package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

func TestDashboardRetriesInitialLogsWhenContainerStarts(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		lateError, batch, failRetry, fullHistory bool
	}{
		{name: "pending to running"},
		{name: "late startup error", lateError: true},
		{name: "batched pod update", batch: true},
		{name: "failed retry stays bounded", failRetry: true},
		{name: "full history survives retry", fullHistory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := dashDeployModel(160, 40, nil)
			target, _ := m.dashboard.target()
			m.pods = map[types.UID]podRow{}
			m.dashboard = dashboardState{}
			attempts := 0
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg {
				attempts++
				if attempts == 1 || tc.failRetry {
					return LogErrorMsg{Session: req.Session, Err: "startup failed"}
				}
				return nil
			}
			out, _ := m.openDashboard(target.Ref, target.UID)
			m = out.(Model)
			event := PodEventMsg{Context: "alpha", Kind: cluster.PodAdded, UID: "p", Name: "api-first", Namespace: "default", Phase: "Pending", Containers: []string{"api"}, Labels: map[string]string{"app": "payments"}, ContainerInfo: []cluster.ContainerInfo{{Name: "api", State: cluster.ContainerWaiting, Reason: "ContainerCreating"}}}
			out, cmd := m.Update(event)
			m = out.(Model)
			if cmd == nil {
				t.Fatal("missing first attempt")
			}
			if tc.fullHistory {
				cmd = m.beginLogStreamTail(m.logs.ref, m.logs.container, -1)
			}
			requestedTail := m.logs.tail
			oldSession := m.logs.session
			failure := cmd()
			if !tc.lateError {
				out, cmd = m.Update(failure)
				m = out.(Model)
				if cmd != nil {
					t.Fatal("retried while container still waiting")
				}
			}
			// Pod phase alone is insufficient; the selected container may still be waiting.
			event.Kind = cluster.PodUpdated
			event.Phase = "Running"
			out, cmd = m.Update(event)
			m = out.(Model)
			if cmd != nil {
				t.Fatal("retried before selected container started")
			}
			event.ContainerInfo[0].Running = true
			event.ContainerInfo[0].Reason = "" // Running, but readiness is still false.
			var msg tea.Msg = event
			if tc.batch {
				msg = ResourceBatchMsg{event}
			}
			out, cmd = m.Update(msg)
			m = out.(Model)
			if tc.lateError {
				if cmd != nil {
					t.Fatal("replaced request still in flight")
				}
				out, cmd = m.Update(failure)
				m = out.(Model)
			}
			if cmd == nil {
				t.Fatal("container started but request was not retried")
			}
			out, next := m.Update(cmd())
			m = out.(Model)
			if next != nil {
				t.Fatal("failed retry started another request")
			}
			t.Cleanup(m.logs.cancel)
			if attempts != 2 || m.logs.session == oldSession || m.logs.ref.UID != "p" || m.logs.tail != requestedTail {
				t.Fatalf("retry changed identity or count: %+v, %d", m.logs.ref, attempts)
			}
			if !tc.failRetry && m.logs.err != "" {
				t.Fatalf("error survived successful restart: %s", m.logs.err)
			}
			for i := 0; i < 3; i++ {
				out, cmd = m.Update(event)
				m = out.(Model)
				if cmd != nil {
					t.Fatal("unchanged pod restarted logs again")
				}
			}
			out, cmd = m.Update(LogErrorMsg{Session: oldSession, Err: "late old error", retryWhenStarted: true})
			m = out.(Model)
			if cmd != nil || m.logs.err == "late old error" {
				t.Fatal("accepted previous session's error")
			}
		})
	}
}

func TestDashboardDoesNotRetryEstablishedEmptyStream(t *testing.T) {
	m := dashDeployModel(160, 40, nil)
	p := m.pods["dash-uid"]
	p.ContainerInfo[0].Running = true
	m.pods[p.UID] = p
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
	m.dashboard.logRef = podRefFor(p)
	m.dashboard.containers = p.Containers
	cmd := m.startDashboardLogs()
	out, _ := m.Update(cmd())
	m = out.(Model)
	t.Cleanup(m.logs.cancel)
	out, cmd = m.Update(LogErrorMsg{Session: m.logs.session, Err: "established stream failed"})
	m = out.(Model)
	if cmd != nil || m.recoverDashboardLogs() != nil {
		t.Fatal("restarted an established stream with no buffered lines")
	}
}
