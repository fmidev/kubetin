package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// TestViewFitsCanvas asserts the invariant that View() output never
// exceeds (m.width × m.height) — no row scrolls off the top, no row
// wraps off the right edge. We check across a matrix of widths,
// heights, and view modes because every layout regression we've hit
// has come from one specific combination triggering an overflow that
// other combinations papered over.
func TestViewFitsCanvas(t *testing.T) {
	store := model.NewStore()
	contexts := []string{"alpha", "beta", "gamma"}

	cases := []struct {
		name   string
		width  int
		height int
		view   View
		setup  func(*Model)
	}{
		{"pods/wide-tall", 200, 50, ViewPods, nil},
		{"pods/narrow-short", 80, 24, ViewPods, nil},
		{"pods/very-narrow", 60, 20, ViewPods, nil},
		{"pods/tiny", 40, 12, ViewPods, nil},
		{"nodes/wide-tall", 200, 50, ViewNodes, nil},
		{"nodes/narrow", 80, 24, ViewNodes, nil},
		{"nodes/very-narrow", 60, 20, ViewNodes, nil},
		{"deploy/wide", 200, 50, ViewDeployments, nil},
		{"deploy/very-narrow", 60, 20, ViewDeployments, nil},
		// Events are an overlay now, not a view — exercised over each
		// of the underlying views rather than as one of them.
		{"events-lens/wide", 200, 50, ViewPods, eventsLens(nil)},
		{"events-lens/narrow", 60, 20, ViewPods, eventsLens(nil)},
		{"events-lens/tiny", 40, 12, ViewPods, eventsLens(nil)},
		{"events-lens/scoped", 120, 30, ViewPods, eventsLens(func(m *Model) {
			m.eventsLens.scope = &eventScopeRef{
				Kind: "Pod", Namespace: "default", Name: "payments-api-7f9c8-x2k4l",
			}
		})},
		{"events-lens/scoped-empty", 120, 30, ViewPods, eventsLens(func(m *Model) {
			m.eventsLens.scope = &eventScopeRef{Kind: "Pod", Namespace: "default", Name: "nothing-here"}
		})},
		{"events-lens/namespace-scoped", 120, 30, ViewPods, eventsLens(func(m *Model) {
			m.namespace = "kube-system"
		})},
		{"events-lens/cluster-empty", 120, 30, ViewPods, func(m *Model) {
			m.eventsLens.open = true
		}},
		{"events-lens/scrolled", 120, 24, ViewPods, eventsLens(func(m *Model) {
			m.eventsLens.scroll = 6
		})},
		{"events-lens/long-message", 120, 30, ViewPods, eventsLens(func(m *Model) {
			m.events["evt-long"] = eventRow{
				UID: "evt-long", Namespace: "default", Type: "Warning",
				Reason:  strings.Repeat("R", 60),
				Message: strings.Repeat("m", 400) + "\nsecond line",
				Count:   1, LastSeen: time.Now(),
				InvolvedKind: "Pod", InvolvedName: strings.Repeat("p", 90), InvolvedNs: "default",
			}
		})},
		{"events-lens/over-deployments", 120, 30, ViewDeployments, eventsLens(nil)},
		{"namespaces/wide", 200, 50, ViewNamespaces, nil},
		{"namespaces/narrow", 80, 24, ViewNamespaces, nil},
		{"overview/wide", 200, 50, ViewOverview, nil},
		{"overview/narrow", 80, 24, ViewOverview, nil},

		{"with-filter", 100, 30, ViewPods, func(m *Model) { m.filterText = "kube-system" }},
		{"with-filter-focused", 100, 30, ViewPods, func(m *Model) {
			m.filterFocused = true
			m.filterText = "kube"
		}},

		{"help-open", 120, 40, ViewPods, func(m *Model) { m.helpOpen = true }},
		{"rbac-open-empty", 120, 40, ViewPods, func(m *Model) { m.rbacOpen = true }},
		{"rbac-open-mixed", 120, 40, ViewPods, func(m *Model) {
			m.rbacOpen = true
			m.permissions = map[string]permState{
				"alpha|get||pods/log|":     {Allowed: true},
				"alpha|create||pods/exec|": {Allowed: false, Reason: "forbidden by RBAC"},
				"alpha|delete||pods|":      {Allowed: false, Err: "timeout"},
			}
			m.permissionsInFlight = map[string]struct{}{
				"alpha|list||events|": {},
			}
		}},
		{"rbac-open-with-ns", 120, 40, ViewPods, func(m *Model) {
			m.rbacOpen = true
			m.namespace = "kube-system"
			m.permissions = map[string]permState{
				"alpha|create||pods/exec|kube-system": {Allowed: true},
			}
		}},
		{"rbac-narrow", 70, 24, ViewPods, func(m *Model) { m.rbacOpen = true }},
		{"action-menu-denied", 120, 40, ViewPods, func(m *Model) {
			m.actionMenu.open = true
			m.actionMenu.ref.Kind = "Pod"
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
				{Action: ActLogs, Status: actionAllowed},
				{Action: ActExec, Status: actionDenied, Reason: "forbidden"},
				{Action: ActEvents, Status: actionPending},
				{Action: ActDelete, Status: actionDenied, Reason: "forbidden"},
			}
		}},
		{"action-menu-long-name", 120, 40, ViewPods, func(m *Model) {
			m.actionMenu.open = true
			m.actionMenu.ref = clusterRef("Pod", "default", strings.Repeat("x", 80))
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
				{Action: ActDelete, Status: actionAllowed},
			}
		}},
		{"action-menu-floating", 120, 40, ViewPods, func(m *Model) {
			// Seed a pod so the table behind the menu has visible
			// content. The layout test asserts dimensions; we test the
			// floating shape itself in TestActionMenuFloating below.
			m.pods["uid-1"] = podRow{UID: "uid-1", Namespace: "default", Name: "behind-menu"}
			m.actionMenu.open = true
			m.actionMenu.ref = clusterRef("Pod", "default", "behind-menu")
			m.actionMenu.options = []actionItem{
				{Action: ActDescribe, Status: actionAllowed},
			}
		}},
		{"describe-open", 120, 40, ViewPods, func(m *Model) {
			m.describe.open = true
			m.describe.result.YAML = strings.Repeat("yaml line\n", 60)
		}},
		{"action-menu", 120, 40, ViewPods, func(m *Model) { m.actionMenu.open = true }},
		{"exec-picker", 120, 40, ViewPods, func(m *Model) {
			m.exec.pickerOpen = true
			m.exec.containers = []string{"app", "sidecar", "istio-proxy"}
		}},
		{"drain-confirm", 120, 40, ViewNodes, func(m *Model) {
			m.drainConfirm.open = true
			m.drainConfirm.node = "node-1"
		}},
		{"drain-progress", 120, 40, ViewNodes, func(m *Model) {
			m.drainProgress.open = true
			m.drainProgress.node = "node-1"
			m.drainProgress.phase = "evicting"
			m.drainProgress.current = "kube-system/coredns-abc"
			m.drainProgress.done = 3
			m.drainProgress.total = 10
			m.drainProgress.blocked = []string{
				"kube-system/etcd-0 (PDB blocked after 5 retries)",
			}
		}},
		{"ns-picker", 120, 40, ViewPods, func(m *Model) {
			m.nsPickerOpen = true
			m.nsPickerOptions = []string{"(all namespaces)", "default", "kube-system"}
		}},
		{"logs-open", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"L1", "L2", "L3"}
			m.logs.cap = 1000
			m.logs.follow = true
			m.logs.container = "main"
		}},
		{"logs-narrow", 70, 20, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"L1"}
			m.logs.cap = 100
			m.logs.follow = true
		}},
		{"logs-search-active", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"hello world", "matchme", "noise", "matchme again"}
			m.logs.cap = 100
			m.logs.follow = false
			m.logs.searchTerm = "matchme"
			m.logs.searchMatches = []int{1, 3}
			m.logs.searchIdx = 0
		}},
		{"logs-search-focused", 120, 40, ViewPods, func(m *Model) {
			m.logs.open = true
			m.logs.lines = []string{"a", "b", "c"}
			m.logs.cap = 100
			m.logs.follow = false
			m.logs.searchTerm = "ab"
			m.logs.searchFocused = true
		}},
		{"delete-confirm", 120, 40, ViewPods, func(m *Model) {
			m.deleteConfirm.open = true
		}},
		{"scale-confirm", 120, 40, ViewDeployments, func(m *Model) {
			m.scaleConfirm.open = true
			m.scaleConfirm.ref.Name = "my-deploy"
			m.scaleConfirm.ref.Namespace = "default"
			m.scaleConfirm.current = 3
			m.scaleConfirm.typed = "5"
		}},
		{"dashboard/wide", 200, 50, ViewPods, dashSetup(nil)},
		{"dashboard/min-wide", 120, 20, ViewPods, dashSetup(nil)},
		{"dashboard/just-under-wide", 119, 40, ViewPods, dashSetup(nil)},
		{"dashboard/short", 160, 19, ViewPods, dashSetup(nil)},
		{"dashboard/narrow", 80, 24, ViewPods, dashSetup(nil)},
		{"dashboard/very-narrow", 60, 20, ViewPods, dashSetup(nil)},
		{"dashboard/tiny", 40, 12, ViewPods, dashSetup(nil)},
		{"dashboard/blocking-condition", 200, 50, ViewPods, dashSetup(func(m *Model) {
			r := m.pods["dash-uid"]
			r.Phase = "Pending"
			r.Conditions = []cluster.PodCondition{{
				Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
				Message: "0/5 nodes are available: 5 Insufficient cpu.\nsome trailing detail",
			}}
			m.pods["dash-uid"] = r
		})},
		{"dashboard/no-containers-reported", 200, 50, ViewPods, dashSetup(func(m *Model) {
			r := m.pods["dash-uid"]
			r.ContainerInfo = nil
			r.InitContainerInfo = nil
			m.pods["dash-uid"] = r
		})},
		{"dashboard/no-events", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.events = map[types.UID]eventRow{}
		})},
		{"dashboard/log-error", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.logs.lines = nil
			m.logs.err = "the server rejected our request: pods \"x\" not found"
		})},
		{"dashboard/long-everything", 200, 50, ViewPods, dashSetup(func(m *Model) {
			r := m.pods["dash-uid"]
			r.Name = strings.Repeat("x", 120)
			r.PodIP = "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
			r.ContainerInfo[0].Image = "registry.example.com/" + strings.Repeat("y", 90) + ":1.0"
			m.pods["dash-uid"] = r
			m.logs.lines = []string{strings.Repeat("L", 400)}
		})},
		{"dashboard/scrolled", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.dashboard.scroll[dashPaneEvents] = 3
			m.dashboard.scroll[dashPaneLogs] = 2
			m.dashboard.focus = dashPaneEvents
		})},
		{"dashboard/stacked-scrolled", 80, 24, ViewPods, dashSetup(func(m *Model) {
			m.dashboard.canvas = 7
		})},
		{"dashboard/target-gone", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.pods = map[types.UID]podRow{}
		})},
		{"dashboard/logs-over-dashboard", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.logs.open = true
		})},
		{"dashboard/menu-over-dashboard", 200, 50, ViewPods, dashSetup(func(m *Model) {
			m.actionMenu.open = true
			m.actionMenu.ref = clusterRef("Pod", "default", "dash-pod")
			m.actionMenu.options = []actionItem{{Action: ActDashboard, Status: actionAllowed}}
		})},

		{"dash-deploy/wide", 200, 50, ViewDeployments, dashDeploySetup(nil)},
		{"dash-deploy/min-wide", 120, 20, ViewDeployments, dashDeploySetup(nil)},
		{"dash-deploy/narrow", 80, 24, ViewDeployments, dashDeploySetup(nil)},
		{"dash-deploy/tiny", 40, 12, ViewDeployments, dashDeploySetup(nil)},
		{"dash-deploy/no-pods", 200, 50, ViewDeployments, dashDeploySetup(func(m *Model) {
			m.pods = map[types.UID]podRow{}
		})},
		{"dash-deploy/stalled-rollout", 200, 50, ViewDeployments, dashDeploySetup(func(m *Model) {
			d := m.deployments["dep-uid"]
			d.Ready, d.Available, d.Unavailable = 0, 0, 3
			d.Conditions = []cluster.DeployCondition{{
				Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded",
				Message: "ReplicaSet \"payments-api-7f9c8\" has timed out progressing.",
			}}
			m.deployments["dep-uid"] = d
		})},
		{"dash-deploy/cursor-at-end", 200, 50, ViewDeployments, dashDeploySetup(func(m *Model) {
			m.dashboard.focus = dashPaneMain
			m.dashboard.podCursor = 2
		})},
		{"dash-deploy/drilled-into-pod", 200, 50, ViewDeployments, dashDeploySetup(func(m *Model) {
			m.dashboard.stack = append(m.dashboard.stack, dashboardTarget{
				Ref: cluster.DescribeRef{Version: "v1", Resource: "pods", Kind: "Pod",
					Namespace: "default", Name: "dash-pod"},
				UID: "dash-uid",
			})
		})},

		// Single-context models render without the cluster rail, so the
		// main pane owns the full width — a different geometry from
		// every other case here, which uses three contexts.
		{"single-cluster/wide", 200, 50, ViewPods, singleContext(nil)},
		{"single-cluster/narrow", 80, 24, ViewPods, singleContext(nil)},
		{"single-cluster/tiny", 40, 12, ViewPods, singleContext(nil)},
		{"single-cluster/nodes", 120, 30, ViewNodes, singleContext(nil)},
		{"single-cluster/overview", 120, 30, ViewOverview, singleContext(nil)},
		{"single-cluster/with-filter", 120, 30, ViewPods, singleContext(func(m *Model) {
			m.filterFocused = true
			m.filterText = "kube"
		})},
		{"zero-contexts", 120, 30, ViewPods, func(m *Model) { m.Contexts = nil; m.HideSidebar = true }},
		// Rail hidden by choice on a fleet: multiple contexts, no rail.
		{"rail-hidden/fleet-wide", 200, 50, ViewPods, func(m *Model) { m.HideSidebar = true }},
		{"rail-hidden/fleet-narrow", 80, 24, ViewPods, func(m *Model) { m.HideSidebar = true }},
		// Rail forced on with a lone cluster, the inverse of the default.
		{"rail-shown/single-cluster", 120, 30, ViewPods, singleContext(func(m *Model) {
			m.HideSidebar = false
		})},

		{"services/wide", 200, 50, ViewServices, netFixture(nil)},
		{"services/mid", 120, 40, ViewServices, netFixture(nil)},
		{"services/narrow", 80, 24, ViewServices, netFixture(nil)},
		{"services/very-narrow", 60, 20, ViewServices, netFixture(nil)},
		{"services/tiny", 40, 12, ViewServices, netFixture(nil)},
		{"services/unsynced-empty", 120, 30, ViewServices, func(m *Model) {}},
		{"services/synced-empty", 120, 30, ViewServices, func(m *Model) { m.syncedServices = true }},
		{"services/long-names", 120, 30, ViewServices, netFixture(func(m *Model) {
			r := m.services["svc-lb"]
			r.Name = strings.Repeat("s", 90)
			r.Namespace = strings.Repeat("n", 40)
			r.ExternalIPs = []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334", "10.0.0.1", "10.0.0.2"}
			m.services["svc-lb"] = r
		})},
		{"services/filtered", 120, 30, ViewServices, netFixture(func(m *Model) {
			m.namespace = "staging"
			m.filterText = "orders"
		})},

		{"ingresses/wide", 200, 50, ViewIngresses, netFixture(nil)},
		{"ingresses/mid", 120, 40, ViewIngresses, netFixture(nil)},
		{"ingresses/narrow", 80, 24, ViewIngresses, netFixture(nil)},
		{"ingresses/very-narrow", 60, 20, ViewIngresses, netFixture(nil)},
		{"ingresses/tiny", 40, 12, ViewIngresses, netFixture(nil)},
		{"ingresses/unsynced-empty", 120, 30, ViewIngresses, func(m *Model) {}},
		{"ingresses/synced-empty", 120, 30, ViewIngresses, func(m *Model) { m.syncedIngresses = true }},
		{"ingresses/long-hosts", 120, 30, ViewIngresses, netFixture(func(m *Model) {
			r := m.ingresses["ing-multi"]
			r.Hosts = []string{strings.Repeat("h", 120) + ".example.com", "b.example.com"}
			r.Address = ""
			m.ingresses["ing-multi"] = r
		})},

		{"restart-confirm", 120, 40, ViewDeployments, func(m *Model) {
			m.restartConfirm.open = true
			m.restartConfirm.ref.Name = "my-deploy"
			m.restartConfirm.ref.Namespace = "default"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New("alpha", store, contexts)
			m.width, m.height = tc.width, tc.height
			m.view = tc.view
			if tc.setup != nil {
				tc.setup(&m)
			}

			out := m.View()

			gotH := lipgloss.Height(out)
			if gotH != tc.height {
				t.Errorf("height = %d, want %d", gotH, tc.height)
			}
			for i, line := range strings.Split(out, "\n") {
				w := lipgloss.Width(line)
				if w > tc.width {
					t.Errorf("line %d width = %d, want ≤ %d (%q)", i, w, tc.width, truncForErr(line))
					break
				}
			}
		})
	}
}

// TestClampCanvasContract — clampCanvas is the geometric backbone, so
// its contract gets its own test independent of View().
func TestClampCanvasContract(t *testing.T) {
	cases := []struct {
		name string
		s    string
		w, h int
	}{
		{"shorter", "a\nb", 10, 5},
		{"taller", "a\nb\nc\nd\ne\nf\ng", 10, 4},
		{"wider", strings.Repeat("X", 30), 10, 3},
		{"trailing-newline", "a\nb\nc\n", 10, 4},
		{"empty", "", 10, 5},
		{"styled", lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("hi"), 10, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := clampCanvas(tc.s, tc.w, tc.h)
			if h := lipgloss.Height(out); h != tc.h {
				t.Errorf("height = %d, want %d", h, tc.h)
			}
			for i, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w != tc.w {
					t.Errorf("row %d width = %d, want %d", i, w, tc.w)
					break
				}
			}
		})
	}
}

// dashSetup seeds a realistic dashboard: a two-container pod mid
// CrashLoopBackOff, a handful of events, and a live log buffer. extra
// runs afterwards so individual cases can distort one dimension
// without restating the whole fixture.
func dashSetup(extra func(*Model)) func(*Model) {
	return func(m *Model) {
		now := time.Now()
		m.pods["dash-uid"] = podRow{
			UID:            "dash-uid",
			Namespace:      "default",
			Name:           "dash-pod",
			Phase:          "Running",
			Restarts:       3,
			Node:           "worker-03.example.com",
			Containers:     []string{"api", "envoy"},
			CreatedAt:      now.Add(-4 * time.Hour),
			StartedAt:      now.Add(-4 * time.Hour),
			PodIP:          "10.42.3.17",
			HostIP:         "192.168.1.10",
			QOSClass:       "Burstable",
			ServiceAccount: "payments",
			CPUMilli:       142,
			MemBytes:       384 << 20,
			HasMetrics:     true,
			HasNetwork:     true,
			NetRXBps:       1200,
			NetTXBps:       840,
			ContainerInfo: []cluster.ContainerInfo{
				{Name: "api", Image: "ghcr.io/x/api:1.2", Ready: true, State: cluster.ContainerReady},
				{Name: "envoy", Image: "envoy:v1.29", State: cluster.ContainerError,
					Restarts: 3, Reason: "CrashLoopBackOff", ExitCode: 137},
			},
			InitContainerInfo: []cluster.ContainerInfo{
				{Name: "wait-db", Image: "busybox:1.36", State: cluster.ContainerTerminated, Reason: "Completed"},
			},
			Conditions: []cluster.PodCondition{{Type: "Ready", Status: "True"}},
		}
		m.cursor = "dash-uid"
		m.events = map[types.UID]eventRow{}
		for i := 0; i < 8; i++ {
			uid := types.UID("evt-" + string(rune('a'+i)))
			m.events[uid] = eventRow{
				UID: uid, Namespace: "default", Type: "Warning",
				Reason:       "BackOff",
				Message:      "Back-off restarting failed container envoy in pod dash-pod_default",
				Count:        int32(i + 1),
				LastSeen:     now.Add(-time.Duration(i) * time.Minute),
				InvolvedKind: "Pod", InvolvedName: "dash-pod", InvolvedNs: "default",
			}
		}
		m.logs.streaming = true
		m.logs.follow = true
		m.logs.container = "envoy"
		m.logs.cap = 1000
		for i := 0; i < 40; i++ {
			m.logs.lines = append(m.logs.lines, "12:04:11 GET /health 200 1.2ms")
		}
		m.dashboard.open = true
		m.dashboard.stack = []dashboardTarget{{
			Ref: cluster.DescribeRef{Version: "v1", Resource: "pods", Kind: "Pod",
				Namespace: "default", Name: "dash-pod"},
			UID: "dash-uid",
		}}
		m.dashboard.containers = []string{"api", "envoy"}
		if extra != nil {
			extra(m)
		}
	}
}

// dashDeploySetup seeds a deployment dashboard: three replicas sharing
// the deployment's selector, one of which is the CrashLoopBackOff pod
// from dashSetup, plus deployment- and replicaset-level events.
func dashDeploySetup(extra func(*Model)) func(*Model) {
	return func(m *Model) {
		dashSetup(nil)(m)

		sel := map[string]string{"app": "payments"}
		now := time.Now()

		// The pod dashSetup created is one of this deployment's replicas.
		p := m.pods["dash-uid"]
		p.Name = "payments-api-7f9c8-x2k4l"
		p.Labels = sel
		m.pods["dash-uid"] = p
		for i, name := range []string{"payments-api-7f9c8-aaaaa", "payments-api-7f9c8-bbbbb"} {
			uid := types.UID("rep-" + name)
			m.pods[uid] = podRow{
				UID: uid, Namespace: "default", Name: name, Phase: "Running",
				Labels: sel, Containers: []string{"api", "envoy"},
				CreatedAt: now.Add(-time.Duration(i+1) * time.Hour),
				ContainerInfo: []cluster.ContainerInfo{
					{Name: "api", Ready: true, State: cluster.ContainerReady, Image: "ghcr.io/x/api:1.2"},
					{Name: "envoy", Ready: true, State: cluster.ContainerReady, Image: "envoy:v1.29"},
				},
			}
		}

		// A neighbouring deployment whose name shares the prefix — the
		// selector match must not pull its pods in.
		m.pods["foreign"] = podRow{
			UID: "foreign", Namespace: "default", Name: "payments-api-worker-1234-zzzzz",
			Phase: "Running", Labels: map[string]string{"app": "payments-worker"},
		}

		m.deployments["dep-uid"] = deploymentRow{
			UID: "dep-uid", Namespace: "default", Name: "payments-api",
			Replicas: 3, Ready: 2, UpToDate: 3, Available: 2, Unavailable: 1,
			CreatedAt:    now.Add(-12 * 24 * time.Hour),
			StrategyType: "RollingUpdate", MaxSurge: "25%", MaxUnavailable: "25%",
			Selector:   sel,
			Conditions: []cluster.DeployCondition{{Type: "Available", Status: "True"}},
		}
		m.events["dep-evt"] = eventRow{
			UID: "dep-evt", Namespace: "default", Type: "Normal", Reason: "ScalingReplicaSet",
			Message: "Scaled up replica set payments-api-7f9c8 to 3", Count: 1,
			LastSeen:     now.Add(-9 * time.Minute),
			InvolvedKind: "Deployment", InvolvedName: "payments-api", InvolvedNs: "default",
		}
		m.events["rs-evt"] = eventRow{
			UID: "rs-evt", Namespace: "default", Type: "Normal", Reason: "SuccessfulCreate",
			Message: "Created pod: payments-api-7f9c8-x2k4l", Count: 1,
			LastSeen:     now.Add(-8 * time.Minute),
			InvolvedKind: "ReplicaSet", InvolvedName: "payments-api-7f9c8", InvolvedNs: "default",
		}
		// The pod events dashSetup created point at "dash-pod"; repoint
		// them at the renamed replica so they belong to this deployment.
		for uid, e := range m.events {
			if e.InvolvedKind == "Pod" && e.InvolvedName == "dash-pod" {
				e.InvolvedName = "payments-api-7f9c8-x2k4l"
				m.events[uid] = e
			}
		}

		m.cursor = "dep-uid"
		m.dashboard.stack = []dashboardTarget{{
			Ref: cluster.DescribeRef{Group: "apps", Version: "v1", Resource: "deployments",
				Kind: "Deployment", Namespace: "default", Name: "payments-api"},
			UID: "dep-uid",
		}}
		m.dashboard.podCursor = 0
		if extra != nil {
			extra(m)
		}
	}
}

// singleContext trims the model down to one configured cluster, which
// is what New would have defaulted HideSidebar from.
func singleContext(extra func(*Model)) func(*Model) {
	return func(m *Model) {
		m.Contexts = []string{"alpha"}
		m.HideSidebar = true
		if extra != nil {
			extra(m)
		}
	}
}

// netFixture seeds the Services and Ingresses views with the states
// worth rendering: a plain ClusterIP, a LoadBalancer with an address, a
// LoadBalancer still pending one, a Service selecting nothing, and a
// Service whose endpoints are split across two slices.
func netFixture(extra func(*Model)) func(*Model) {
	return func(m *Model) {
		now := time.Now()
		m.services["svc-api"] = serviceRow{
			UID: "svc-api", Namespace: "default", Name: "payments-api",
			Type: "ClusterIP", ClusterIP: "10.43.12.8",
			Selector:  map[string]string{"app": "payments"},
			Ports:     []cluster.ServicePort{{Port: 80, Protocol: "TCP"}},
			CreatedAt: now.Add(-12 * 24 * time.Hour),
		}
		m.services["svc-lb"] = serviceRow{
			UID: "svc-lb", Namespace: "default", Name: "payments-lb",
			Type: "LoadBalancer", ClusterIP: "10.43.9.101",
			ExternalIPs: []string{"34.88.10.4"},
			Selector:    map[string]string{"app": "payments"},
			Ports:       []cluster.ServicePort{{Port: 80, NodePort: 30821, Protocol: "TCP"}},
			CreatedAt:   now.Add(-4 * 24 * time.Hour),
		}
		m.services["svc-pending"] = serviceRow{
			UID: "svc-pending", Namespace: "prod", Name: "web-lb",
			Type: "LoadBalancer", ClusterIP: "10.43.7.7",
			Selector:  map[string]string{"app": "web"},
			Ports:     []cluster.ServicePort{{Port: 443, NodePort: 31000, Protocol: "TCP"}},
			CreatedAt: now.Add(-9 * time.Minute),
		}
		m.services["svc-orphan"] = serviceRow{
			UID: "svc-orphan", Namespace: "staging", Name: "orders-api",
			Type: "ClusterIP", ClusterIP: "10.43.44.2",
			Selector:  map[string]string{"app": "orders"},
			Ports:     []cluster.ServicePort{{Port: 8080, Protocol: "TCP"}},
			CreatedAt: now.Add(-3 * time.Hour),
		}
		m.services["svc-headless"] = serviceRow{
			UID: "svc-headless", Namespace: "default", Name: "cassandra",
			Type: "ClusterIP", ClusterIP: "None",
			CreatedAt: now.Add(-30 * 24 * time.Hour),
		}
		m.syncedServices = true

		// payments-api's endpoints arrive split across two slices, the
		// shape the controller produces past 100 endpoints or on a
		// dual-stack cluster.
		m.endpointSlices["es-api-1"] = endpointSliceRow{Namespace: "default", ServiceName: "payments-api", Ready: 2, Total: 3}
		m.endpointSlices["es-api-2"] = endpointSliceRow{Namespace: "default", ServiceName: "payments-api", Ready: 1, Total: 1}
		m.endpointSlices["es-lb"] = endpointSliceRow{Namespace: "default", ServiceName: "payments-lb", Ready: 2, Total: 2}
		m.endpointSlices["es-orphan"] = endpointSliceRow{Namespace: "staging", ServiceName: "orders-api", Ready: 0, Total: 0}
		m.endpointSlices["es-web"] = endpointSliceRow{Namespace: "prod", ServiceName: "web-lb", Ready: 3, Total: 3}

		m.ingresses["ing-pay"] = ingressRow{
			UID: "ing-pay", Namespace: "default", Name: "payments", Class: "nginx",
			Hosts: []string{"pay.example.com"}, Address: "34.88.10.4", TLSHosts: 1,
			Backends:  []cluster.IngressBackend{{Service: "payments-api", Port: "80"}},
			CreatedAt: now.Add(-12 * 24 * time.Hour),
		}
		m.ingresses["ing-multi"] = ingressRow{
			UID: "ing-multi", Namespace: "default", Name: "orders", Class: "nginx",
			Hosts:   []string{"orders.example.com", "o2.example.com", "o3.example.com"},
			Address: "34.88.10.4", TLSHosts: 3,
			Backends:  []cluster.IngressBackend{{Service: "orders-api", Port: "8080"}, {Service: "orders-ui", Port: "80"}},
			CreatedAt: now.Add(-6 * 24 * time.Hour),
		}
		m.ingresses["ing-pending"] = ingressRow{
			UID: "ing-pending", Namespace: "staging", Name: "internal",
			Backends:  []cluster.IngressBackend{{Service: "web", Port: "80"}},
			CreatedAt: now.Add(-3 * time.Hour),
		}
		m.syncedIngresses = true

		if extra != nil {
			extra(m)
		}
	}
}

// eventsLens opens the events overlay over a seeded event set.
func eventsLens(extra func(*Model)) func(*Model) {
	return func(m *Model) {
		now := time.Now()
		for i := 0; i < 6; i++ {
			uid := types.UID("lens-evt-" + string(rune('a'+i)))
			ns := "default"
			if i%2 == 1 {
				ns = "kube-system"
			}
			m.events[uid] = eventRow{
				UID: uid, Namespace: ns, Type: "Warning", Reason: "BackOff",
				Message:      "Back-off restarting failed container envoy in pod payments-api",
				Count:        int32(i + 1),
				LastSeen:     now.Add(-time.Duration(i) * time.Minute),
				InvolvedKind: "Pod", InvolvedName: "payments-api-7f9c8-x2k4l", InvolvedNs: ns,
			}
		}
		m.syncedEvents = true
		m.eventsLens.open = true
		if extra != nil {
			extra(m)
		}
	}
}

// clusterRef is a tiny test helper so the action-menu cases above
// can construct a DescribeRef inline without dragging cluster types
// into every case literal.
func clusterRef(kind, namespace, name string) cluster.DescribeRef {
	return cluster.DescribeRef{Kind: kind, Namespace: namespace, Name: name}
}

// TestActionMenuFloating asserts the action menu does NOT blank out
// the underlying table — at least one row outside the menu's column
// band still carries the pod name behind it. Pins the "non-blocking
// overlay" behaviour so a regression to the old full-canvas modal
// trips the test instead of just looking wrong.
func TestActionMenuFloating(t *testing.T) {
	store := model.NewStore()
	m := New("alpha", store, []string{"alpha"})
	m.width, m.height = 200, 50
	m.pods["uid-1"] = podRow{UID: "uid-1", Namespace: "default", Name: "pod-behind-menu-zzz"}
	m.actionMenu.open = true
	m.actionMenu.ref = clusterRef("Pod", "default", "pod-behind-menu-zzz")
	m.actionMenu.options = []actionItem{
		{Action: ActDescribe, Status: actionAllowed},
	}

	out := m.View()

	// The pod's name should still appear somewhere in the rendered
	// canvas — the floating modal sits centred, leaving the body
	// columns to the left/right intact.
	if !strings.Contains(out, "pod-behind-menu-zzz") {
		t.Errorf("expected pod name visible behind floating menu, but not present in render")
	}
}

func truncForErr(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
