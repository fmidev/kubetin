package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

func TestDashboardRegressionDescribe(t *testing.T) {
	for _, deployment := range []bool{false, true} {
		t.Run(fmt.Sprint(deployment), func(t *testing.T) {
			m := dashModel(160, 40, nil)
			if deployment {
				dashDeploySetup(nil)(&m)
			}
			m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
				ctx, cancel := context.WithTimeout(req.Context, 10*time.Second)
				defer cancel()
				if ctx.Err() != nil {
					t.Fatal("describe context is already canceled")
				}
				return DescribeResultMsg(cluster.DescribeResult{Context: focused})
			}
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("dashboard d panicked: %v", r)
				}
			}()
			out, cmd := m.Update(key("d"))
			if cmd == nil {
				t.Fatal("no describe command")
			}
			out, _ = out.Update(cmd())
			if out.(Model).describe.loading {
				t.Error("describe result not accepted")
			}
		})
	}
}

func TestDashboardRegressionDescribeResult(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
		return DescribeResultMsg(cluster.DescribeResult{Context: focused})
	}
	out, cmd := m.Update(key("d"))
	out, _ = out.Update(cmd())
	if out.(Model).describe.loading {
		t.Error("even without context panic, dashboard stays loading after describe completion")
	}
}

func TestDashboardRegressionStaleMetrics(t *testing.T) {
	m := dashModel(160, 40, nil)
	out, _ := m.Update(MetricsSnapshotMsg{Context: "alpha", OK: false, At: time.Now()})
	m = out.(Model)
	p := m.pods["dash-uid"]
	if p.HasMetrics {
		t.Fatal("fixture still has metrics")
	}
	body := m.renderDashPodStatus(p, 160, 3)
	if strings.Contains(body, formatCPU(p.CPUMilli)) || strings.Contains(body, formatMem(p.MemBytes)) {
		t.Errorf("stale metrics still rendered: %s", body)
	}
}

func TestDashboardRegressionSparseContainers(t *testing.T) {
	m := dashModel(160, 40, nil)
	for _, initOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(initOnly), func(t *testing.T) {
			p := podRow{Containers: []string{"api", "sidecar"}}
			if initOnly {
				p.InitContainerInfo = []cluster.ContainerInfo{{Name: "init-db", State: cluster.ContainerWaiting}}
			} else {
				p.ContainerInfo = []cluster.ContainerInfo{{Name: "api", Ready: true, State: cluster.ContainerReady}}
			}
			ready, total := containerReadyCount(p)
			if total != 2 {
				t.Errorf("sparse status reported ready %d/%d, want total 2", ready, total)
			}
			body := m.renderDashContainers(p, 100, 8, 0)
			if !strings.Contains(body, "sidecar") {
				t.Errorf("declared sidecar omitted: %s", body)
			}
		})
	}
}

func TestDashboardRegressionDeniedLogs(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
	m.logs.lines = []string{"ORIGINAL_POD_SECRET_MARKER"}
	m.logs.ref = podRefFor(m.pods["dash-uid"])
	out, _ := m.closeDashboard()
	m = out.(Model)
	p := m.pods["dash-uid"]
	p.UID = "other-uid"
	p.Name = "other-pod"
	p.Namespace = "other"
	m.pods[p.UID] = p
	m.permissions[cluster.PermissionKey("alpha", "get", "", "pods/log", "other")] = permState{Allowed: false}
	out, _ = m.openDashboard(podRefFor(p), p.UID)
	m = out.(Model)
	if strings.Contains(m.View(), "ORIGINAL_POD_SECRET_MARKER") {
		t.Errorf("new target %s shows logs from %s after denial", m.dashboard.logRef.Name, m.logs.ref.Name)
	}
}

func TestDashboardRegressionEventControls(t *testing.T) {
	for _, deployment := range []bool{false, true} {
		t.Run(fmt.Sprint(deployment), func(t *testing.T) {
			m := dashModel(200, 50, nil)
			if deployment {
				dashDeploySetup(nil)(&m)
			}
			for uid, e := range m.events {
				e.Message = "\x1b[2Jpayload\x07"
				m.events[uid] = e
			}
			body := m.View()
			if strings.Contains(body, "\x1b[2J") || strings.Contains(body, "\x07") {
				t.Error("terminal control bytes survived full dashboard View")
			}
		})
	}
}

func TestDashboardRegressionForeignReplicaSet(t *testing.T) {
	m := dashModel(160, 40, nil)
	dashDeploySetup(nil)(&m)
	m.events["foreign-rs"] = eventRow{UID: "foreign-rs", Namespace: "default", InvolvedNs: "default", InvolvedKind: "ReplicaSet", InvolvedName: "payments-api-worker-abcdef", Message: "foreign rollout failure"}
	sub, _ := m.dashSubjectNow()
	for _, e := range m.dashDeployEvents(sub.Deploy, sub.Pods) {
		if e.UID == "foreign-rs" {
			t.Error("payments-api dashboard includes payments-api-worker ReplicaSet event")
		}
	}
}

func TestDashboardRegressionEmptyDeploymentRecovery(t *testing.T) {
	m := dashModel(160, 40, nil)
	dashDeploySetup(nil)(&m)
	m.pods = map[types.UID]podRow{}
	target, _ := m.dashboard.target()
	m.dashboard = dashboardState{}
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
	out, _ := m.openDashboard(target.Ref, target.UID)
	m = out.(Model)
	out, cmd := m.Update(PodEventMsg{Context: "alpha", Kind: cluster.PodAdded, UID: "new-pod", Name: "new-pod", Namespace: "default", Labels: map[string]string{"app": "payments"}, Phase: "Running", Containers: []string{"api"}})
	m = out.(Model)
	sub, _ := m.dashSubjectNow()
	if len(sub.Pods) != 1 {
		t.Fatal("pod update not applied")
	}
	if cmd == nil || m.dashboard.logRef.Name == "" || !m.logs.streaming || m.logs.err != "" {
		t.Errorf("pod arrived, log target still empty; error: %s", m.logs.err)
	}
	t.Cleanup(m.logs.cancel)
}

var dashboardBenchmarkSink string

func BenchmarkDeploymentDashboardView(b *testing.B) {
	for _, w := range []int{80, 160} {
		for _, n := range []int{10, 100, 1000} {
			b.Run(fmt.Sprintf("w%d/pods%d", w, n), func(b *testing.B) {
				m := dashModel(w, 40, nil)
				dashDeploySetup(nil)(&m)
				base := m.pods["dash-uid"]
				m.pods = make(map[types.UID]podRow, n)
				for i := 0; i < n; i++ {
					p := base
					p.UID = types.UID(fmt.Sprint(i))
					p.Name = fmt.Sprintf("payments-api-7f9c8-%05d", i)
					m.pods[p.UID] = p
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					dashboardBenchmarkSink = m.View()
				}
			})
		}
	}
}

func TestDashboardRegressionControlCases(t *testing.T) {
	t.Run("shared_describe_path", func(t *testing.T) {
		m := dashModel(160, 40, nil)
		m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
			ctx, cancel := context.WithTimeout(req.Context, time.Second)
			defer cancel()
			if ctx.Err() != nil {
				t.Fatal("describe context is already canceled")
			}
			return DescribeResultMsg(cluster.DescribeResult{Context: focused})
		}
		out, cmd := m.startDescribe(podRefFor(m.pods["dash-uid"]), false)
		out, _ = out.Update(cmd())
		if out.(Model).describe.loading {
			t.Fatal("shared describe path also broken")
		}
	})
	t.Run("allowed_log_target_clears_old_buffer", func(t *testing.T) {
		m := dashModel(160, 40, nil)
		m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
		m.logs.lines = []string{"ORIGINAL_POD_MARKER"}
		p := m.pods["dash-uid"]
		p.Name = "new-pod"
		p.UID = "new-uid"
		m.pods[p.UID] = p
		out, _ := m.closeDashboard()
		out, _ = out.(Model).openDashboard(podRefFor(p), p.UID)
		if len(out.(Model).logs.lines) != 0 {
			t.Fatal("allowed target also retains old logs")
		}
	})
	t.Run("complete_container_status", func(t *testing.T) {
		p := podRow{Containers: []string{"api", "sidecar"}, ContainerInfo: []cluster.ContainerInfo{{Name: "api", Ready: true}, {Name: "sidecar", Ready: false}}}
		ready, total := containerReadyCount(p)
		if ready != 1 || total != 2 {
			t.Fatal("complete status also broken")
		}
	})
}

func TestDashboardRegressionDeniedQueuedLines(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }
	m.logs.lines = nil
	m.logs.session = 42
	out, _ := m.closeDashboard()
	m = out.(Model)
	p := m.pods["dash-uid"]
	p.Name = "denied-pod"
	p.UID = "denied-uid"
	p.Namespace = "denied"
	m.pods[p.UID] = p
	m.permissions[cluster.PermissionKey("alpha", "get", "", "pods/log", "denied")] = permState{Allowed: false}
	out, _ = m.openDashboard(podRefFor(p), p.UID)
	out, _ = out.Update(LogLinesMsg{Session: 42, Lines: []string{"QUEUED_FROM_PREVIOUS_POD"}})
	if strings.Contains(out.(Model).View(), "QUEUED_FROM_PREVIOUS_POD") {
		t.Error("denied target accepted previous session's queued log lines")
	}
}

func TestDashboardReplicaSetOwnership(t *testing.T) {
	m := dashDeployModel(160, 40, nil)
	m.pods = nil // FailedCreate can happen before a ReplicaSet has any pods.
	m.events = map[types.UID]eventRow{}
	for _, tc := range []struct {
		name      string
		owner     types.UID
		namespace string
	}{
		{"unusual-name", "dep-uid", "default"},
		{"payments-api-worker-123", "other-deployment", "default"},
		{"payments-api-orphan", "", "default"},
		{"foreign-namespace", "dep-uid", "other"},
	} {
		uid := types.UID(tc.name)
		out, _ := m.Update(ReplicaSetEventMsg{Context: "alpha", UID: uid, Name: tc.name, Namespace: tc.namespace, DeploymentUID: tc.owner})
		m = out.(Model)
		out, _ = m.Update(EvtEventMsg{Context: "alpha", UID: uid, InvolvedKind: "ReplicaSet", InvolvedName: tc.name, InvolvedNs: tc.namespace, InvolvedUID: uid})
		m = out.(Model)
	}
	stale := m.events["unusual-name"]
	stale.UID = "stale"
	stale.InvolvedUID = "replaced-rs"
	m.events[stale.UID] = stale
	d := m.deployments["dep-uid"]
	got := m.dashDeployEvents(d, nil)
	if len(got) != 1 || got[0].InvolvedName != "unusual-name" {
		t.Fatalf("ownership filtering returned %+v", got)
	}
	out, _ := m.Update(ReplicaSetEventMsg{Context: "alpha", Kind: cluster.ReplicaSetDeleted, UID: "unusual-name"})
	m = out.(Model)
	if len(m.dashDeployEvents(d, nil)) != 0 {
		t.Fatal("deleted ReplicaSet retained")
	}
	old := m.Focus()
	m.focusContext("beta")
	m.focusContext("alpha")
	if len(m.replicaSets) != 0 {
		t.Fatal("ownership survived focus change")
	}
	ev := ReplicaSetEventMsg{Context: "alpha", UID: "late", DeploymentUID: "dep-uid"}
	for _, msg := range []tea.Msg{FocusedMsg{Focus: old, Msg: ev}, ev} {
		out, _ = m.Update(msg)
		m = out.(Model)
		if len(m.replicaSets) != 0 {
			t.Fatal("accepted ownership from a previous focus visit")
		}
	}
}

func TestDashboardLogRecoveryKeepsSelectedStream(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			m := dashDeployModel(160, 40, nil)
			target, _ := m.dashboard.target()
			m.pods = map[types.UID]podRow{}
			m.dashboard = dashboardState{}
			var requests []LogStartMsg
			m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { requests = append(requests, req); return nil }
			out, _ := m.openDashboard(target.Ref, target.UID)
			m = out.(Model)
			event := PodEventMsg{Context: "alpha", Kind: cluster.PodAdded, UID: "first", Name: "first", Namespace: "default", Phase: "Running", Containers: []string{"api"}, Labels: map[string]string{"app": "payments"}}
			var msg tea.Msg = event
			if batch {
				msg = ResourceBatchMsg{event}
			}
			out, cmd := m.Update(msg)
			m = out.(Model)
			if cmd == nil {
				t.Fatal("arrival did not start logs")
			}
			cmd()
			if len(requests) != 1 || requests[0].Ref.Name != "first" {
				t.Fatalf("requests: %+v", requests)
			}
			t.Cleanup(m.logs.cancel)
			session := m.logs.session
			event.UID = "newer"
			event.Name = "newer"
			event.CreatedAt = time.Now()
			out, cmd = m.Update(event)
			m = out.(Model)
			if cmd != nil || m.logs.session != session || m.dashboard.logRef.Name != "first" {
				t.Fatal("new replica interrupted selected logs")
			}
		})
	}
}

func TestDashboardDescribeRejectsClosedRequest(t *testing.T) {
	m := dashModel(160, 40, nil)
	m.OnDescribe = func(req DescribeRequestMsg, focused string) tea.Msg {
		return DescribeResultMsg(cluster.DescribeResult{Context: focused})
	}
	out, cmd := m.Update(key("d"))
	m = out.(Model)
	old := cmd()
	request := m.describe.request
	out, _ = m.Update(key("esc"))
	m = out.(Model)
	if request.ctx.Err() == nil {
		t.Fatal("closing describe did not cancel its request")
	}
	out, newCmd := m.Update(key("d"))
	m = out.(Model)
	out, _ = m.Update(old)
	m = out.(Model)
	if !m.describe.loading {
		t.Fatal("old request completed reopened overlay")
	}
	out, _ = m.Update(newCmd())
	m = out.(Model)
	if m.describe.loading {
		t.Fatal("current request was rejected")
	}
}

func TestDashboardDeniedTargetCancelsStream(t *testing.T) {
	m := dashModel(160, 40, nil)
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.logs.cancel = cancel
	m.logs.session = 10
	m.logs.lines = []string{"old log"}
	m.dashboard.logRef = podRefFor(m.pods["dash-uid"])
	m.permissions[cluster.PermissionKey("alpha", "get", "", "pods/log", "default")] = permState{Allowed: false}
	if cmd := m.startDashboardLogs(); cmd != nil {
		t.Fatal("denied target started logs")
	}
	if streamCtx.Err() == nil || m.logs.session == 10 || len(m.logs.lines) != 0 || m.logs.err == "" {
		t.Fatal("denial did not cancel and clear the previous stream")
	}
}

func TestDashboardSanitizesConditionsAndContainerReasons(t *testing.T) {
	m := dashModel(160, 40, nil)
	p := m.pods["dash-uid"]
	p.Conditions = []cluster.PodCondition{{Type: "Ready", Status: "False", Reason: "\x1b[2Jreason", Message: "\x1b]52;c;payload\a"}}
	p.ContainerInfo[0].State = cluster.ContainerError
	p.ContainerInfo[0].Reason = "\x1b[2Jblocked\a"
	m.pods["dash-uid"] = p
	if body := m.View(); strings.Contains(body, "\x1b[2J") || strings.Contains(body, "\x1b]52") || strings.Contains(body, "\a") {
		t.Fatal("pod detail controls reached the terminal")
	}
	dashDeploySetup(nil)(&m)
	d := m.deployments["dep-uid"]
	d.Conditions = []cluster.DeployCondition{{Type: "Available", Status: "False", Reason: "\x1b[2Jreason", Message: "\a message"}}
	m.deployments["dep-uid"] = d
	if body := m.View(); strings.Contains(body, "\x1b[2J") || strings.Contains(body, "\a") {
		t.Fatal("deployment condition controls reached the terminal")
	}
}
