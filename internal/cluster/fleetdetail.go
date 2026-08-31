package cluster

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Bounded row caps for a fleet-detail fetch — the panel is a triage
// summary under one cluster's card, not a table view. Lists paginate
// up to fleetDetailPageCap pages before the worst rows are selected,
// so a disaster cluster's later pages can't hide its worst pods.
const (
	fleetDetailPodCap      = 15
	fleetDetailDeployCap   = 10
	fleetDetailEventCap    = 10
	fleetDetailEventWindow = time.Hour
	fleetDetailPageCap     = 4 // × Limit 500 = 2000 items per list
)

// FleetPodIssue is one non-green pod in a fleet-detail fetch.
type FleetPodIssue struct {
	Namespace string
	Name      string
	Phase     string
	Reason    string // container waiting/terminated reason when present
	Restarts  int32
}

// FleetDeployIssue is one deployment below its desired replica count.
type FleetDeployIssue struct {
	Namespace string
	Name      string
	Ready     int32
	Desired   int32
}

// FleetEventGroup aggregates recent Warning events sharing a
// (Reason, Message) pair, the same shape the events lens groups by.
type FleetEventGroup struct {
	Reason   string
	Message  string
	Count    int32
	LastSeen time.Time
}

// FleetDetailResult is the on-demand drill-down behind one cluster's
// card in the fleet dashboard. UI receivers must compare Context to
// the cluster they requested and drop mismatches.
type FleetDetailResult struct {
	Context string
	Pods    []FleetPodIssue
	Deploys []FleetDeployIssue
	Events  []FleetEventGroup
	Err     string
	At      time.Time
}

// FleetDetail fetches the problem detail for one cluster: non-green
// pods with their container reasons, degraded deployments, and
// grouped recent warning events. The three lists are the same ones
// the probe counts — kept as rows here rather than tallies. Partial
// failures return what succeeded plus Err.
func (s *Supervisor) FleetDetail(ctx context.Context, ctxName string) FleetDetailResult {
	out := FleetDetailResult{Context: ctxName, At: time.Now()}

	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		out.Err = "kubeconfig: " + err.Error()
		return out
	}
	restCfg.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		out.Err = "client: " + err.Error()
		return out
	}
	ns := s.ResolveScope(ctx, ctxName, cs)

	var mu sync.Mutex
	fail := func(err error) {
		mu.Lock()
		if out.Err == "" {
			out.Err = trimError(err)
			if out.Err == "" {
				// An error whose text trims away must still read as
				// a failure — an empty Err claims the lists succeeded.
				out.Err = "fetch failed"
			}
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		var pods []FleetPodIssue
		opts := metav1.ListOptions{
			FieldSelector: "status.phase!=Running,status.phase!=Succeeded",
			Limit:         500,
		}
		for page := 0; page < fleetDetailPageCap; page++ {
			list, err := cs.CoreV1().Pods(ns).List(ctx, opts)
			if err != nil {
				fail(err)
				return
			}
			for _, p := range list.Items {
				pods = append(pods, podIssueOf(p))
			}
			if list.Continue == "" {
				break
			}
			opts.Continue = list.Continue
		}
		sort.Slice(pods, func(i, j int) bool {
			pi, pj := podPhaseRank(pods[i].Phase), podPhaseRank(pods[j].Phase)
			if pi != pj {
				return pi < pj
			}
			if pods[i].Restarts != pods[j].Restarts {
				return pods[i].Restarts > pods[j].Restarts
			}
			return pods[i].Namespace+"/"+pods[i].Name < pods[j].Namespace+"/"+pods[j].Name
		})
		if len(pods) > fleetDetailPodCap {
			pods = pods[:fleetDetailPodCap]
		}
		mu.Lock()
		out.Pods = pods
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		var deps []FleetDeployIssue
		opts := metav1.ListOptions{Limit: 500}
		for page := 0; page < fleetDetailPageCap; page++ {
			list, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
			if err != nil {
				fail(err)
				return
			}
			for _, d := range list.Items {
				desired := int32(1)
				if d.Spec.Replicas != nil {
					desired = *d.Spec.Replicas
				}
				if desired == 0 || d.Status.ReadyReplicas >= desired {
					continue
				}
				deps = append(deps, FleetDeployIssue{
					Namespace: d.Namespace, Name: d.Name,
					Ready: d.Status.ReadyReplicas, Desired: desired,
				})
			}
			if list.Continue == "" {
				break
			}
			opts.Continue = list.Continue
		}
		sort.Slice(deps, func(i, j int) bool {
			ri := float64(deps[i].Ready) / float64(deps[i].Desired)
			rj := float64(deps[j].Ready) / float64(deps[j].Desired)
			if ri != rj {
				return ri < rj
			}
			return deps[i].Namespace+"/"+deps[i].Name < deps[j].Namespace+"/"+deps[j].Name
		})
		if len(deps) > fleetDetailDeployCap {
			deps = deps[:fleetDetailDeployCap]
		}
		mu.Lock()
		out.Deploys = deps
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		type groupKey struct{ reason, message string }
		groups := make(map[groupKey]*FleetEventGroup)
		cutoff := time.Now().Add(-fleetDetailEventWindow)
		var items []corev1.Event
		opts := metav1.ListOptions{FieldSelector: "type=Warning", Limit: 500}
		for page := 0; page < fleetDetailPageCap; page++ {
			list, err := cs.CoreV1().Events(ns).List(ctx, opts)
			if err != nil {
				fail(err)
				return
			}
			items = append(items, list.Items...)
			if list.Continue == "" {
				break
			}
			opts.Continue = list.Continue
		}
		for _, e := range items {
			last := e.LastTimestamp.Time
			if last.IsZero() {
				last = e.EventTime.Time
			}
			if last.IsZero() {
				last = e.CreationTimestamp.Time
			}
			if !last.After(cutoff) {
				continue
			}
			count := e.Count
			if count == 0 {
				count = 1
			}
			k := groupKey{e.Reason, e.Message}
			g := groups[k]
			if g == nil {
				g = &FleetEventGroup{Reason: e.Reason, Message: e.Message}
				groups[k] = g
			}
			g.Count += count
			if last.After(g.LastSeen) {
				g.LastSeen = last
			}
		}
		evs := make([]FleetEventGroup, 0, len(groups))
		for _, g := range groups {
			evs = append(evs, *g)
		}
		sort.Slice(evs, func(i, j int) bool {
			if evs[i].Count != evs[j].Count {
				return evs[i].Count > evs[j].Count
			}
			return evs[i].LastSeen.After(evs[j].LastSeen)
		})
		if len(evs) > fleetDetailEventCap {
			evs = evs[:fleetDetailEventCap]
		}
		mu.Lock()
		out.Events = evs
		mu.Unlock()
	}()
	wg.Wait()
	return out
}

// podIssueOf projects one non-green pod into a detail row. Init
// containers count too: a pod stuck in Init:CrashLoopBackOff often
// has no regular statuses beyond PodInitializing, which would hide
// the actual reason and restart count.
func podIssueOf(p corev1.Pod) FleetPodIssue {
	issue := FleetPodIssue{
		Namespace: p.Namespace,
		Name:      p.Name,
		Phase:     string(p.Status.Phase),
	}
	for _, cst := range p.Status.ContainerStatuses {
		issue.Restarts += cst.RestartCount
		if issue.Reason == "" {
			switch {
			case cst.State.Waiting != nil:
				issue.Reason = cst.State.Waiting.Reason
			case cst.State.Terminated != nil:
				issue.Reason = cst.State.Terminated.Reason
			}
		}
	}
	for _, cst := range p.Status.InitContainerStatuses {
		issue.Restarts += cst.RestartCount
		switch {
		case cst.State.Waiting != nil && cst.State.Waiting.Reason != "" &&
			cst.State.Waiting.Reason != "PodInitializing":
			issue.Reason = "Init:" + cst.State.Waiting.Reason
		case cst.State.Terminated != nil && cst.State.Terminated.ExitCode != 0 &&
			cst.State.Terminated.Reason != "":
			issue.Reason = "Init:" + cst.State.Terminated.Reason
		}
	}
	if issue.Reason == "" && p.Status.Reason != "" {
		issue.Reason = p.Status.Reason
	}
	return issue
}

// podPhaseRank orders detail rows worst-first: dead pods, then limbo,
// then merely waiting.
func podPhaseRank(phase string) int {
	switch phase {
	case string(corev1.PodFailed):
		return 0
	case string(corev1.PodUnknown):
		return 1
	case string(corev1.PodPending):
		return 2
	}
	return 3
}
