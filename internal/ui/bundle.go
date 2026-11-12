package ui

// Rancher Fleet's GitOps state on a Fleet manager, laid out for triage:
// what's broken, where, and why. Named for bundles, never "fleet":
// kubetin's fleet is the F1 dashboard.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fmidev/kubetin/internal/cluster"
)

// agentSilentAfter marks a Fleet agent silent: agents check in every
// 15 minutes by default. Fleet keeps showing a silent agent's bundles
// as they last were, Ready included, so silence is the only sign their
// state is stale.
const agentSilentAfter = time.Hour

// bundlesState is the focused cluster's Fleet objects, and what the
// watcher said about each resource it was asked for.
type bundlesState struct {
	items       map[types.UID]cluster.Bundle
	deployments map[types.UID]cluster.BundleDeployment
	repos       map[types.UID]cluster.GitRepo
	clusters    map[types.UID]cluster.BundleCluster
	// synced maps each resource whose initial list arrived to the
	// namespace it is watched in, "" for all.
	synced      map[string]string
	unavailable map[string]cluster.BundleEvent
}

func newBundlesState() bundlesState {
	return bundlesState{
		items:       make(map[types.UID]cluster.Bundle),
		deployments: make(map[types.UID]cluster.BundleDeployment),
		repos:       make(map[types.UID]cluster.GitRepo),
		clusters:    make(map[types.UID]cluster.BundleCluster),
		synced:      make(map[string]string),
		unavailable: make(map[string]cluster.BundleEvent),
	}
}

func (s bundlesState) apply(ev cluster.BundleEvent) {
	switch ev.Kind {
	case cluster.BundleSynced:
		s.synced[ev.Resource] = ev.WatchNamespace
		return
	case cluster.BundleUnavailable:
		s.unavailable[ev.Resource] = ev
		return
	}
	gone := ev.Kind == cluster.BundleDeleted
	switch {
	case ev.Bundle != nil:
		putBundleObject(s.items, ev.UID, *ev.Bundle, gone)
	case ev.Deployment != nil:
		putBundleObject(s.deployments, ev.UID, *ev.Deployment, gone)
	case ev.GitRepo != nil:
		putBundleObject(s.repos, ev.UID, *ev.GitRepo, gone)
	case ev.Cluster != nil:
		putBundleObject(s.clusters, ev.UID, *ev.Cluster, gone)
	}
}

func putBundleObject[T any](m map[types.UID]T, uid types.UID, v T, gone bool) {
	if gone {
		delete(m, uid)
		return
	}
	m[uid] = v
}

func agentSilent(c cluster.BundleCluster, now time.Time) bool {
	return c.AgentLastSeen.IsZero() || now.Sub(c.AgentLastSeen) > agentSilentAfter
}

// silentAgents counts silent agents. An agent goes silent without any
// event, so the table order depends on this count, not only on events.
func (s bundlesState) silentAgents(now time.Time) int {
	n := 0
	for _, c := range s.clusters {
		if agentSilent(c, now) {
			n++
		}
	}
	return n
}

type bundleKey struct{ namespace, name string }

type bundleIndex struct {
	targets  map[bundleKey][]cluster.BundleDeployment
	clusters map[bundleKey]cluster.BundleCluster
	// perCluster: BundleDeployments are synced cluster-wide, so targets
	// is complete. They live in per-cluster namespaces, so a namespaced
	// watch holds almost none of them.
	perCluster bool
}

func (m Model) bundleIndex() bundleIndex {
	idx := bundleIndex{
		targets:  make(map[bundleKey][]cluster.BundleDeployment),
		clusters: make(map[bundleKey]cluster.BundleCluster),
	}
	ns, ok := m.bundles.synced["bundledeployments"]
	idx.perCluster = ok && ns == ""
	for _, d := range m.bundles.deployments {
		k := bundleKey{d.BundleNamespace, d.BundleName}
		idx.targets[k] = append(idx.targets[k], d)
	}
	for _, c := range m.bundles.clusters {
		idx.clusters[bundleKey{c.Namespace, c.Name}] = c
	}
	return idx
}

// clusterName turns Fleet's "namespace/name" cluster reference into the
// name Rancher shows.
func (idx bundleIndex) clusterName(ref string) string {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return ref
	}
	if c, found := idx.clusters[bundleKey{ns, name}]; found && c.DisplayName != "" {
		return c.DisplayName
	}
	return name
}

// bundleTarget is one cluster a bundle deploys to.
type bundleTarget struct {
	name       string
	deploy     cluster.BundleDeployment
	agent      cluster.BundleCluster
	agentKnown bool
}

func (t bundleTarget) silent(now time.Time) bool {
	return t.agentKnown && agentSilent(t.agent, now)
}

// bundleView is a Bundle as the table shows it.
type bundleView struct {
	state string
	rank  int
	ready string
	// where lists the clusters with a problem; when there are none, the
	// clusters targeted, if known.
	where   []string
	problem bool
	targets []bundleTarget
}

func (idx bundleIndex) view(b cluster.Bundle, now time.Time) bundleView {
	v := bundleView{ready: "—"}
	if b.Summary.Known {
		v.ready = fmt.Sprintf("%d/%d", b.Summary.Ready, b.Summary.DesiredReady)
	}
	silent := false
	if idx.perCluster {
		for _, d := range idx.targets[bundleKey{b.Namespace, b.Name}] {
			t := bundleTarget{name: d.ClusterName, deploy: d}
			t.agent, t.agentKnown = idx.clusters[bundleKey{d.ClusterNamespace, d.ClusterName}]
			if t.agentKnown && t.agent.DisplayName != "" {
				t.name = t.agent.DisplayName
			}
			v.targets = append(v.targets, t)
		}
		sort.Slice(v.targets, func(i, j int) bool {
			if v.targets[i].name != v.targets[j].name {
				return v.targets[i].name < v.targets[j].name
			}
			return v.targets[i].deploy.UID < v.targets[j].deploy.UID
		})
		for _, t := range v.targets {
			if t.silent(now) || t.deploy.State != "Ready" {
				v.where = append(v.where, t.name)
			}
			silent = silent || t.silent(now)
		}
	} else {
		for _, r := range b.Summary.NonReady {
			v.where = append(v.where, idx.clusterName(r.Name))
		}
	}
	v.problem = len(v.where) > 0
	if !v.problem {
		for _, t := range v.targets {
			v.where = append(v.where, t.name)
		}
	}
	v.state = bundleState(b, silent)
	v.rank = bundleRank(v.state)
	return v
}

// bundleState is Fleet's state for b, except where Fleet's own claim
// can't be trusted: a Ready computed for an older spec, or reported by
// an agent that has since gone silent.
func bundleState(b cluster.Bundle, silent bool) string {
	switch {
	case b.State != "":
		if b.State != "Ready" {
			return b.State
		}
	case !b.Summary.Known:
		return "Unknown"
	case b.Summary.DesiredReady == 0:
		return "No targets"
	case b.Summary.Ready < b.Summary.DesiredReady:
		return "NotReady"
	}
	switch {
	case b.ObservedGeneration < b.Generation:
		return "Updating"
	case silent:
		return "Stale"
	}
	return "Ready"
}

// bundleStateRanks orders the table worst first, following Fleet's own
// ranking of BundleDeployment states.
var bundleStateRanks = map[string]int{
	"ErrApplied":  10,
	"WaitApplied": 9,
	"Modified":    8,
	"OutOfSync":   7,
	"Pending":     6,
	"NotReady":    5,
	"Stale":       4,
	"Updating":    3,
	"Unknown":     2,
	"No targets":  1,
	"Ready":       0,
}

func bundleRank(state string) int {
	if r, ok := bundleStateRanks[state]; ok {
		return r
	}
	// A state this build doesn't know is not Ready.
	return bundleStateRanks["NotReady"]
}

func (t Theme) bundleStateStyle(state string) lipgloss.Style {
	switch state {
	case "ErrApplied":
		return t.StatusBad
	case "Ready":
		return t.StatusOK
	case "Unknown", "No targets":
		return t.Dim
	}
	return t.StatusWrn
}

func bundleMatches(b cluster.Bundle, namespace, needle string) bool {
	if namespace != "" && b.Namespace != namespace {
		return false
	}
	return needle == "" ||
		strings.Contains(strings.ToLower(b.Namespace), needle) ||
		strings.Contains(strings.ToLower(b.Name), needle) ||
		strings.Contains(strings.ToLower(b.Repo), needle)
}

func (m Model) sortedBundleUIDs(needle string) []types.UID {
	idx, now := m.bundleIndex(), time.Now()
	type ranked struct {
		b    cluster.Bundle
		rank int
	}
	rows := make([]ranked, 0, len(m.bundles.items))
	for _, b := range m.bundles.items {
		if bundleMatches(b, m.namespace, needle) {
			rows = append(rows, ranked{b, idx.view(b, now).rank})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		if a.b.Namespace != b.b.Namespace {
			return a.b.Namespace < b.b.Namespace
		}
		if a.b.Name != b.b.Name {
			return a.b.Name < b.b.Name
		}
		return a.b.UID < b.b.UID
	})
	out := make([]types.UID, len(rows))
	for i, r := range rows {
		out[i] = r.b.UID
	}
	return out
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func bundleUnavailableText(ev cluster.BundleEvent) string {
	switch ev.Reason {
	case cluster.BundleNoAPI:
		return "not served"
	case cluster.BundleForbidden:
		return cleanDetail(ev.Detail)
	}
	return "check failed: " + cleanDetail(ev.Detail)
}

// repoTrouble is why a GitRepo isn't delivering, "" when it is. Bundle
// failures are the table's business; this is git and the GitRepo itself.
func repoTrouble(r cluster.GitRepo) string {
	for _, c := range r.Conditions {
		if (c.Type == "Stalled" && c.Status == "True") ||
			((c.Type == "GitPolling" || c.Type == "Accepted") && c.Status == "False") {
			return cleanDetail(c.Type + ": " + c.Message)
		}
	}
	if r.Error {
		if r.Message == "" {
			return "error"
		}
		return cleanDetail(r.Message)
	}
	return ""
}

// bundleStrip is what the table can't say per bundle: silent agents,
// failing GitRepos, and which parts of the picture are missing.
func (m Model) bundleStrip(now time.Time, width int) []string {
	th := m.Theme
	line := func(style lipgloss.Style, s string) string {
		return style.Render(truncate(" "+cleanDetail(s), width))
	}
	var out []string

	var silent []cluster.BundleCluster
	for _, c := range m.bundles.clusters {
		if agentSilent(c, now) {
			silent = append(silent, c)
		}
	}
	if len(silent) > 0 {
		sort.Slice(silent, func(i, j int) bool { return silent[i].Name < silent[j].Name })
		parts := make([]string, len(silent))
		for i, c := range silent {
			name := c.Name
			if c.DisplayName != "" {
				name = c.DisplayName
			}
			age := "never"
			if !c.AgentLastSeen.IsZero() {
				age = formatAge(c.AgentLastSeen)
			}
			parts[i] = name + " " + age
		}
		out = append(out, line(th.StatusWrn, "⚠ agents silent: "+strings.Join(parts, " · ")+
			" — their bundles show the state last reported"))
	}

	repos := make([]cluster.GitRepo, 0, len(m.bundles.repos))
	for _, r := range m.bundles.repos {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Namespace != repos[j].Namespace {
			return repos[i].Namespace < repos[j].Namespace
		}
		return repos[i].Name < repos[j].Name
	})
	var healthy []string
	for _, r := range repos {
		at := r.Name
		if r.Commit != "" {
			at += " @" + shortCommit(r.Commit)
		}
		if trouble := repoTrouble(r); trouble != "" {
			out = append(out, line(th.StatusBad, "✕ repo "+r.Namespace+"/"+at+": "+trouble))
			continue
		}
		healthy = append(healthy, at)
	}
	if len(healthy) > 0 {
		out = append(out, line(th.Dim, "repos "+strings.Join(healthy, " · ")))
	}

	var gaps []string
	if ns := m.bundles.synced["bundles"]; ns != "" {
		gaps = append(gaps, "bundles only in "+ns)
	}
	if ns, ok := m.bundles.synced["bundledeployments"]; ok && ns != "" {
		gaps = append(gaps, "per-cluster detail only in "+ns)
	} else if ev, ok := m.bundles.unavailable["bundledeployments"]; ok {
		gaps = append(gaps, "no per-cluster detail ("+bundleUnavailableText(ev)+")")
	}
	if ev, ok := m.bundles.unavailable["clusters"]; ok {
		gaps = append(gaps, "agent check-ins unknown ("+bundleUnavailableText(ev)+")")
	}
	if ev, ok := m.bundles.unavailable["gitrepos"]; ok {
		gaps = append(gaps, "repos unknown ("+bundleUnavailableText(ev)+")")
	}
	if len(gaps) > 0 {
		out = append(out, line(th.Dim, "partial: "+strings.Join(gaps, " · ")))
	}
	return out
}

// bundleColumns in display order. BUNDLE never drops, then STATE and
// WHERE: the view exists to say what's broken and where.
var bundleColumns = []column{
	{min: 10, max: 16, prio: 4}, // WORKSPACE
	{min: 16, max: 40, prio: 0}, // BUNDLE
	{min: 10, max: 20, prio: 1}, // STATE
	{min: 5, max: 7, prio: 5},   // READY
	{min: 12, max: 40, prio: 2}, // WHERE
	{min: 14, max: 30, prio: 3}, // REPO@COMMIT
	{min: 4, max: 5, prio: 6},   // AGE
}

func (m Model) renderBundlesView(height, width int) string {
	if _, ok := m.bundles.synced["bundles"]; !ok {
		return m.bundlesPending(width)
	}
	idx, now := m.bundleIndex(), time.Now()
	strip := m.bundleStrip(now, width)
	if limit := height / 4; len(strip) > limit {
		strip = strip[:limit]
	}
	rest := height - len(strip)
	var detail []string
	if b, ok := m.bundles.items[m.cursor]; ok && rest >= 14 {
		detail = m.renderBundleDetail(b, idx.view(b, now), idx, now, rest/3, width)
	}

	var out strings.Builder
	for _, l := range strip {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	table := m.renderBundleTable(idx, now, rest-len(detail), width)
	out.WriteString(table)
	if len(detail) > 0 {
		// The detail sits at the bottom, not wherever a short table ends.
		out.WriteString(strings.Repeat("\n", max(0, rest-len(detail)-strings.Count(table, "\n"))))
	}
	for _, l := range detail {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}

func (m Model) bundlesPending(width int) string {
	ev, ok := m.bundles.unavailable["bundles"]
	if !ok {
		return m.emptyPlaceholder(false, "bundles")
	}
	switch ev.Reason {
	case cluster.BundleNoAPI:
		return m.Theme.Dim.Render(truncate(" No Rancher Fleet here: this cluster doesn't serve fleet.cattle.io bundles.", width))
	case cluster.BundleForbidden:
		return m.Theme.StatusWrn.Render(truncate(" Can't watch Rancher Fleet bundles: "+cleanDetail(ev.Detail), width))
	}
	return m.Theme.StatusWrn.Render(truncate(" Couldn't check for Rancher Fleet, retrying: "+cleanDetail(ev.Detail), width))
}

// renderBundleTable fills height rows: a header, then the window of
// bundles around the cursor, worst first.
func (m Model) renderBundleTable(idx bundleIndex, now time.Time, height, width int) string {
	w := fitColumns(bundleColumns, width-1)
	hdr := m.Theme.Header
	var b strings.Builder
	b.WriteString(" " + joinCells(
		padCol("WORKSPACE", w[0], hdr),
		padCol("BUNDLE", w[1], hdr),
		padCol("STATE", w[2], hdr),
		padColRight("READY", w[3], hdr),
		padCol("WHERE", w[4], hdr),
		padCol("REPO@COMMIT", w[5], hdr),
		padColRight("AGE", w[6], hdr),
	))
	b.WriteByte('\n')
	if m.tableCount(ViewBundles) == 0 {
		b.WriteString(m.emptyPlaceholder(true, "bundles"))
		b.WriteByte('\n')
		return b.String()
	}
	for _, r := range rowsForUIDs(m.bundles.items, m.windowUIDs(ViewBundles, height)) {
		v := idx.view(r, now)
		state := v.state
		if r.Paused {
			state += " (paused)"
		}
		whereStyle := m.Theme.Dim
		if v.problem {
			whereStyle = m.Theme.bundleStateStyle(v.state)
			if v.state == "Ready" {
				whereStyle = m.Theme.StatusWrn
			}
		}
		repo := "—"
		if r.Repo != "" {
			repo = r.Repo
			if r.Commit != "" {
				repo += "@" + shortCommit(r.Commit)
			}
		}
		line := " " + joinCells(
			padCol(r.Namespace, w[0], m.Theme.Base),
			padCol(r.Name, w[1], m.Theme.Base),
			padCol(cleanDetail(state), w[2], m.Theme.bundleStateStyle(v.state)),
			padColRight(v.ready, w[3], m.Theme.Base),
			padCol(cleanDetail(collapseList(v.where)), w[4], whereStyle),
			padCol(repo, w[5], m.Theme.Dim),
			padColRight(formatAge(r.CreatedAt), w[6], m.Theme.Base),
		)
		if r.UID == m.cursor {
			line = renderSelected(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// bundleTargetColumns: the selected bundle, one row per cluster. WHY
// soaks up whatever width is left.
var bundleTargetColumns = []column{
	{min: 10, max: 20, prio: 0},  // CLUSTER
	{min: 10, max: 11, prio: 1},  // STATE
	{min: 7, max: 8, prio: 3},    // AGENT
	{min: 5, max: 7, prio: 4},    // READY
	{min: 12, max: 24, prio: 5},  // RELEASE
	{min: 10, max: 400, prio: 2}, // WHY
}

// renderBundleDetail explains the selected bundle cluster by cluster
// in at most limit lines.
func (m Model) renderBundleDetail(b cluster.Bundle, v bundleView, idx bundleIndex, now time.Time, limit, width int) []string {
	th := m.Theme
	rule := lipgloss.NewStyle().Foreground(lipgloss.Color("236")).Render(strings.Repeat("─", width))
	title := " " + th.Title.Render(truncate(b.Namespace+"/"+b.Name, width/2))
	if c := readyCondition(b.Conditions); c != "" && !idx.perCluster {
		title += th.Dim.Render(truncate("  "+c, width-lipgloss.Width(title)))
	}
	out := []string{rule, title}

	w := fitColumns(bundleTargetColumns, width-1)
	row := func(cells ...string) string { return " " + joinCells(cells...) }
	out = append(out, row(
		padCol("CLUSTER", w[0], th.Header),
		padCol("STATE", w[1], th.Header),
		padCol("AGENT", w[2], th.Header),
		padColRight("READY", w[3], th.Header),
		padCol("RELEASE", w[4], th.Header),
		padCol("WHY", w[5], th.Header),
	))

	var rows []string
	if idx.perCluster {
		for _, t := range v.targets {
			d := t.deploy
			state := cleanDetail(d.State)
			if state == "" {
				state = "—"
			}
			agent, agentStyle := th.agentCell(t.agent, t.agentKnown, now)
			ready := "—"
			if d.Resources.Known {
				ready = fmt.Sprintf("%d/%d", d.Resources.Ready, d.Resources.DesiredReady)
			}
			why, whyStyle := targetWhy(t, now), th.Base
			if why == "" {
				why, whyStyle = "—", th.Dim
			}
			release := cleanDetail(d.Release)
			if release == "" {
				release = "—"
			}
			rows = append(rows, row(
				padCol(cleanDetail(t.name), w[0], th.Base),
				padCol(state, w[1], th.bundleStateStyle(state)),
				padCol(agent, w[2], agentStyle),
				padColRight(ready, w[3], th.Base),
				padCol(release, w[4], th.Dim),
				padCol(why, w[5], whyStyle),
			))
		}
		if len(rows) == 0 {
			rows = append(rows, th.Dim.Render(truncate(" targets no clusters", width)))
		}
	} else {
		for _, r := range b.Summary.NonReady {
			ns, name, _ := strings.Cut(r.Name, "/")
			c, known := idx.clusters[bundleKey{ns, name}]
			agent, agentStyle := th.agentCell(c, known, now)
			rows = append(rows, row(
				padCol(cleanDetail(idx.clusterName(r.Name)), w[0], th.Base),
				padCol(cleanDetail(r.State), w[1], th.bundleStateStyle(r.State)),
				padCol(agent, w[2], agentStyle),
				padColRight("—", w[3], th.Dim),
				padCol("—", w[4], th.Dim),
				padCol(cleanDetail(r.Message), w[5], th.Base),
			))
		}
		if len(rows) == 0 {
			rows = append(rows, th.Dim.Render(truncate(" no per-cluster detail; Fleet lists no cluster as not ready", width)))
		}
	}

	room := limit - len(out)
	if room < 1 {
		return nil
	}
	if len(rows) > room {
		more := len(rows) - room + 1
		rows = append(rows[:room-1], th.Dim.Render(truncate(fmt.Sprintf(" +%d more clusters", more), width)))
	}
	return append(out, rows...)
}

// agentCell is when a cluster's agent last checked in, warning once it
// has gone silent.
func (t Theme) agentCell(c cluster.BundleCluster, known bool, now time.Time) (string, lipgloss.Style) {
	if !known {
		return "—", t.Dim
	}
	seen := "never"
	if !c.AgentLastSeen.IsZero() {
		seen = formatAge(c.AgentLastSeen) + " ago"
	}
	if agentSilent(c, now) {
		return seen, t.StatusWrn
	}
	return seen, t.Dim
}

func readyCondition(conds []cluster.BundleCondition) string {
	for _, c := range conds {
		if c.Type == "Ready" && c.Status == "False" && c.Message != "" {
			return cleanDetail(c.Message)
		}
	}
	return ""
}

// targetWhy says what's wrong with one cluster's deployment: a silent
// agent, then failed conditions, objects not ready, and drift.
func targetWhy(t bundleTarget, now time.Time) string {
	var why []string
	seen := map[string]bool{}
	add := func(s string) {
		if s = cleanDetail(s); s != "" && !seen[s] {
			seen[s] = true
			why = append(why, s)
		}
	}
	if t.silent(now) {
		if t.agent.AgentLastSeen.IsZero() {
			add("agent never checked in")
		} else {
			add("agent silent " + formatAge(t.agent.AgentLastSeen) + ", state as last reported")
		}
	}
	d := t.deploy
	for _, c := range d.Conditions {
		if c.Status == "False" && c.Message != "" && !seen[cleanDetail(c.Message)] {
			add(c.Type + ": " + c.Message)
			seen[cleanDetail(c.Message)] = true
		}
	}
	for _, o := range d.NotReady {
		s := o.Kind + " " + objectRef(o.Namespace, o.Name)
		switch {
		case o.Message != "":
			s += ": " + o.Message
		case o.State != "":
			s += ": " + o.State
		}
		add(s)
	}
	for _, o := range d.Modified {
		what := "modified"
		switch {
		case o.Missing:
			what = "missing"
		case o.Delete:
			what = "should not exist"
		}
		add(o.Kind + " " + objectRef(o.Namespace, o.Name) + " " + what)
	}
	return strings.Join(why, " · ")
}

func objectRef(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}
