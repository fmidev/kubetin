package ui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

const fixtureCommit = "0123456789abcdef0123456789abcdef01234567"

func bundleSummary(desired, ready int, nonReady ...cluster.BundleNonReady) cluster.BundleSummary {
	return cluster.BundleSummary{Known: true, DesiredReady: desired, Ready: ready, NonReady: nonReady}
}

// bundlesFixture is a Fleet manager with every list synced: a failing
// bundle, a drifted one, one whose agent went silent while Fleet still
// says Ready, and healthy ones.
func bundlesFixture(edit func(*Model)) func(*Model) {
	return func(m *Model) {
		now := time.Now()
		apply := func(ev cluster.BundleEvent) {
			ev.Context = "alpha"
			m.bundles.apply(ev)
		}
		for _, r := range []string{"bundles", "bundledeployments", "gitrepos", "clusters"} {
			apply(cluster.BundleEvent{Kind: cluster.BundleSynced, Resource: r})
		}
		for _, c := range []cluster.BundleCluster{
			{Namespace: "fleet-default", Name: "c-edge-a", UID: "c-a", DisplayName: "edge-a", AgentLastSeen: now.Add(-5 * time.Minute)},
			{Namespace: "fleet-default", Name: "c-edge-b", UID: "c-b", DisplayName: "edge-b", AgentLastSeen: now.Add(-104 * 24 * time.Hour)},
			{Namespace: "fleet-local", Name: "local", UID: "c-l", AgentLastSeen: now.Add(-time.Minute)},
		} {
			apply(cluster.BundleEvent{UID: c.UID, Cluster: &c})
		}
		for _, b := range []cluster.Bundle{
			{Namespace: "fleet-default", Name: "platform-ingress", UID: "b-ingress", Repo: "platform", Commit: fixtureCommit,
				State: "ErrApplied", Summary: bundleSummary(2, 1, cluster.BundleNonReady{
					Name: "fleet-default/c-edge-a", State: "ErrApplied", Message: "install failed: timed out"}),
				Conditions: []cluster.BundleCondition{{Type: "Ready", Status: "False", Message: "ErrApplied(1) [Cluster fleet-default/c-edge-a]"}},
				CreatedAt:  now.Add(-48 * time.Hour)},
			{Namespace: "fleet-default", Name: "platform-dns", UID: "b-dns", Repo: "platform", Commit: fixtureCommit,
				Summary: bundleSummary(2, 2), CreatedAt: now.Add(-48 * time.Hour)},
			{Namespace: "fleet-default", Name: "fleet-agent-c-edge-a", UID: "b-agent", Summary: bundleSummary(1, 1)},
			{Namespace: "fleet-local", Name: "monitoring", UID: "b-mon", Repo: "infra", Commit: fixtureCommit,
				State: "Modified", Summary: bundleSummary(1, 0), Paused: true},
			{Namespace: "fleet-local", Name: "healthy", UID: "b-ok", Repo: "infra", Summary: bundleSummary(1, 1)},
		} {
			apply(cluster.BundleEvent{UID: b.UID, Bundle: &b})
		}
		deploy := func(uid, bundleNs, bundle, clusterNs, clusterName, state string) cluster.BundleDeployment {
			return cluster.BundleDeployment{UID: types.UID(uid), Namespace: "cluster-" + clusterName, Name: bundle,
				BundleNamespace: bundleNs, BundleName: bundle, ClusterNamespace: clusterNs, ClusterName: clusterName,
				State: state, Release: "apps/" + bundle + ":3",
				Resources: cluster.BundleResourceCounts{Known: true, DesiredReady: 2, Ready: 2}}
		}
		failed := deploy("d-1", "fleet-default", "platform-ingress", "fleet-default", "c-edge-a", "ErrApplied")
		failed.Conditions = []cluster.BundleCondition{{Type: "Deployed", Status: "False", Message: "install failed: timed out"}}
		failed.NotReady = []cluster.BundleObjectStatus{{Kind: "Deployment", Namespace: "ingress", Name: "controller", Message: "Available: 0/1"}}
		drifted := deploy("d-5", "fleet-local", "monitoring", "fleet-local", "local", "Modified")
		drifted.Modified = []cluster.BundleModifiedObject{{Kind: "ConfigMap", Namespace: "mon", Name: "rules", Patched: true}}
		for _, d := range []cluster.BundleDeployment{
			failed,
			deploy("d-2", "fleet-default", "platform-ingress", "fleet-default", "c-edge-b", "Ready"),
			deploy("d-3", "fleet-default", "platform-dns", "fleet-default", "c-edge-a", "Ready"),
			deploy("d-4", "fleet-default", "platform-dns", "fleet-default", "c-edge-b", "Ready"),
			drifted,
			deploy("d-6", "fleet-local", "healthy", "fleet-local", "local", "Ready"),
			deploy("d-7", "fleet-default", "fleet-agent-c-edge-a", "fleet-default", "c-edge-a", "Ready"),
		} {
			apply(cluster.BundleEvent{UID: d.UID, Deployment: &d})
		}
		for _, r := range []cluster.GitRepo{
			{Namespace: "fleet-default", Name: "platform", UID: "g-1", Commit: fixtureCommit},
			{Namespace: "fleet-local", Name: "infra", UID: "g-2", Commit: fixtureCommit,
				Conditions: []cluster.BundleCondition{{Type: "Stalled", Status: "True", Message: "fatal: could not read from remote repository"}}},
		} {
			apply(cluster.BundleEvent{UID: r.UID, GitRepo: &r})
		}
		m.cursor = "b-ingress"
		if edit != nil {
			edit(m)
		}
	}
}

func bundlesModel(edit func(*Model)) Model {
	m := New("alpha", model.NewStore(), []string{"alpha", "beta"})
	m.width, m.height = 160, 40
	m.view = ViewBundles
	bundlesFixture(edit)(&m)
	return m
}

func plainView(m Model) string { return ansiRE.ReplaceAllString(m.View(), "") }

func TestBundlesSortWorstFirst(t *testing.T) {
	m := bundlesModel(nil)
	want := []types.UID{"b-ingress", "b-mon", "b-dns", "b-agent", "b-ok"}
	if got := m.visibleUIDs(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Fleet still calls a bundle Ready when the agent that reported it went
// silent months ago. The view must not.
func TestBundleReadyFromASilentAgentIsStale(t *testing.T) {
	m := bundlesModel(nil)
	idx := m.bundleIndex()
	v := idx.view(m.bundles.items["b-dns"], time.Now())
	if v.state != "Stale" || !v.problem || !slices.Equal(v.where, []string{"edge-b"}) {
		t.Fatalf("got %+v", v)
	}
	m.cursor = "b-dns"
	out := plainView(m)
	for _, want := range []string{"agents silent: edge-b 14w", "agent silent 14w, state as last reported"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBundleStateDerivation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		b      cluster.Bundle
		silent bool
		want   string
	}{
		{"fleet state wins", cluster.Bundle{State: "OutOfSync", Summary: bundleSummary(1, 1)}, true, "OutOfSync"},
		{"no status yet", cluster.Bundle{}, false, "Unknown"},
		{"targets nothing", cluster.Bundle{Summary: bundleSummary(0, 0)}, false, "No targets"},
		{"short of ready without a state", cluster.Bundle{Summary: bundleSummary(2, 1)}, false, "NotReady"},
		{"ready", cluster.Bundle{Summary: bundleSummary(2, 2)}, false, "Ready"},
		{"ready for an older spec", cluster.Bundle{Summary: bundleSummary(1, 1), Generation: 3, ObservedGeneration: 2}, false, "Updating"},
		{"ready from a silent agent", cluster.Bundle{State: "Ready", Summary: bundleSummary(1, 1)}, true, "Stale"},
	} {
		if got := bundleState(tc.b, tc.silent); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if bundleRank("SomethingNew") <= bundleRank("Ready") {
		t.Error("an unknown Fleet state ranked as healthy")
	}
}

func TestBundleDetailSaysWhereAndWhy(t *testing.T) {
	out := plainView(bundlesModel(func(m *Model) { m.width = 240 }))
	for _, want := range []string{
		"fleet-default/platform-ingress",
		"Deployed: install failed: timed out · Deployment ingress/controller: Available: 0/1",
		"apps/platform-ingress:3",
		"✕ repo fleet-local/infra @0123456: Stalled: fatal: could not read from remote repository",
		"repos platform @0123456",
		"Modified (paused)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Without BundleDeployments the bundle's own summary still says where,
// and the view says what it can't show.
func TestBundleWhereWithoutDeployments(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Model)
		gap  string
	}{
		{"forbidden", func(m *Model) {
			delete(m.bundles.synced, "bundledeployments")
			m.bundles.apply(cluster.BundleEvent{Kind: cluster.BundleUnavailable, Resource: "bundledeployments",
				Reason: cluster.BundleForbidden, Detail: "list bundledeployments denied"})
		}, "no per-cluster detail (list bundledeployments denied)"},
		{"watched in one namespace", func(m *Model) {
			m.bundles.synced["bundledeployments"] = "fleet-default"
		}, "per-cluster detail only in fleet-default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bundlesModel(tc.edit)
			v := m.bundleIndex().view(m.bundles.items["b-ingress"], time.Now())
			if !v.problem || !slices.Equal(v.where, []string{"edge-a"}) || len(v.targets) != 0 {
				t.Fatalf("got %+v", v)
			}
			out := plainView(m)
			for _, want := range []string{tc.gap, "install failed: timed out", "ErrApplied(1) [Cluster fleet-default/c-edge-a]", "5m ago"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
		})
	}
}

func TestBundlesViewExplainsAMissingWatch(t *testing.T) {
	for _, tc := range []struct {
		reason cluster.BundleUnavailableReason
		detail string
		want   string
	}{
		{cluster.BundleNoAPI, "", "this cluster doesn't serve fleet.cattle.io bundles"},
		{cluster.BundleForbidden, "watch bundles denied", "Can't watch Rancher Fleet bundles: watch bundles denied"},
		{cluster.BundleCheckFailed, "discovery: connection refused", "Couldn't check for Rancher Fleet, retrying: discovery: connection refused"},
	} {
		m := New("alpha", model.NewStore(), []string{"alpha"})
		m.width, m.height, m.view = 160, 30, ViewBundles
		m = updateTableModel(m, BundleEventMsg{Context: "alpha", Kind: cluster.BundleUnavailable,
			Resource: "bundles", Reason: tc.reason, Detail: tc.detail})
		if out := plainView(m); !strings.Contains(out, tc.want) {
			t.Errorf("missing %q in:\n%s", tc.want, out)
		}
	}

	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height, m.view = 160, 30, ViewBundles
	if out := plainView(m); !strings.Contains(out, "syncing bundles") {
		t.Errorf("unsynced view doesn't say so:\n%s", out)
	}
	// A check that failed and then recovered.
	m = updateTableModel(m, BundleEventMsg{Context: "alpha", Kind: cluster.BundleUnavailable,
		Resource: "bundles", Reason: cluster.BundleCheckFailed, Detail: "timeout"})
	m = updateTableModel(m, BundleEventMsg{Context: "alpha", Kind: cluster.BundleSynced, Resource: "bundles"})
	if out := plainView(m); strings.Contains(out, "retrying") || !strings.Contains(out, "no bundles in this cluster") {
		t.Errorf("synced didn't supersede the failure:\n%s", out)
	}
}

func TestBundleTextCannotReachTheTerminal(t *testing.T) {
	m := bundlesModel(func(m *Model) {
		d := m.bundles.deployments["d-1"]
		d.Conditions = []cluster.BundleCondition{{Type: "Deployed", Status: "False", Message: "boom\x1b]0;pwned\x07\x1b[2J\nsecond line"}}
		d.Release = "apps/x:1\x1b[31m"
		d.State = "ErrApplied\x1b[5m"
		m.bundles.deployments["d-1"] = d
		b := m.bundles.items["b-ingress"]
		b.State = "ErrApplied\x1b]0;t\x07"
		m.bundles.items["b-ingress"] = b
		r := m.bundles.repos["g-2"]
		r.Conditions[0].Message = "fatal\x1b]8;;http://x\x07"
		m.bundles.repos["g-2"] = r
	})
	out := m.View()
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[31m", "\x1b[5m"} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q", bad)
		}
	}
	target := m.bundleIndex().view(m.bundles.items["b-ingress"], time.Now()).targets[0]
	if got := targetWhy(target, time.Now()); got != "Deployed: boom ]0;pwned [2J second line · Deployment ingress/controller: Available: 0/1" {
		t.Errorf("why = %q", got)
	}
}

func TestBundleKeysAndSelection(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 160, 40
	bundlesFixture(func(m *Model) { m.cursor = "" })(&m)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("7")})
	m = updated.(Model)
	if m.view != ViewBundles || m.cursor != "b-ingress" {
		t.Fatalf("7: view %v cursor %q", m.view, m.cursor)
	}
	sortKey := m.sortKey
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if m = updated.(Model); m.sortKey != sortKey {
		t.Fatal("s cycled the pod sort from the bundles view")
	}
	ref, ok := m.refForCursor()
	if !ok || ref.Group != "fleet.cattle.io" || ref.Resource != "bundles" || ref.Kind != "Bundle" ||
		ref.Namespace != "fleet-default" || ref.Name != "platform-ingress" {
		t.Fatalf("describe ref %+v", ref)
	}
	if !slices.Equal(actionsFor("Bundle"), []Action{ActDescribe, ActEvents}) {
		t.Fatal("bundles gained an action")
	}

	// A bundle arriving while nothing is selected is selected, so the
	// detail pane has something to explain.
	m.cursor = ""
	m = updateTableModel(m, BundleEventMsg{Context: "alpha", UID: "b-new", Bundle: &cluster.Bundle{UID: "b-new", Name: "new"}})
	if m.cursor == "" {
		t.Fatal("no bundle selected")
	}
	m = updateTableModel(m, BundleEventMsg{Context: "beta", UID: "b-foreign", Bundle: &cluster.Bundle{UID: "b-foreign"}})
	if _, ok := m.bundles.items["b-foreign"]; ok {
		t.Fatal("applied another cluster's bundle")
	}
	m.focusContext("beta")
	if len(m.bundles.items)+len(m.bundles.deployments)+len(m.bundles.synced) != 0 {
		t.Fatal("focus change kept the previous cluster's bundles")
	}
}

func TestBundleDetailOverflowCountsTheRest(t *testing.T) {
	m := bundlesModel(func(m *Model) {
		for i := range 30 {
			uid := types.UID(fmt.Sprintf("d-many-%d", i))
			m.bundles.apply(cluster.BundleEvent{UID: uid, Deployment: &cluster.BundleDeployment{UID: uid,
				BundleNamespace: "fleet-default", BundleName: "platform-ingress",
				ClusterNamespace: "fleet-default", ClusterName: fmt.Sprintf("c-%02d", i), State: "Ready"}})
		}
	})
	if out := plainView(m); !regexp.MustCompile(`\+\d+ more clusters`).MatchString(out) {
		t.Errorf("no overflow line:\n%s", out)
	}
}

// An agent goes silent with no event at all. The order must follow.
func TestBundleOrderFollowsAnAgentGoingSilent(t *testing.T) {
	m := bundlesModel(func(m *Model) {
		c := m.bundles.clusters["c-b"]
		c.AgentLastSeen = time.Now()
		m.bundles.clusters["c-b"] = c
	})
	if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"b-ingress", "b-mon", "b-agent", "b-dns", "b-ok"}) {
		t.Fatalf("order = %v", got)
	}
	c := m.bundles.clusters["c-b"]
	c.AgentLastSeen = time.Now().Add(-2 * agentSilentAfter)
	m.bundles.clusters["c-b"] = c
	if got := m.visibleUIDs(); !slices.Equal(got, []types.UID{"b-ingress", "b-mon", "b-dns", "b-agent", "b-ok"}) {
		t.Fatalf("order after silence = %v", got)
	}
}
