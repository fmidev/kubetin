// Rancher Fleet (fleet.cattle.io) GitOps state, read from a Fleet
// manager cluster. Everything here is named for bundles, never
// "fleet": kubetin's own fleet is the F1 multi-cluster dashboard
// (FleetDetail and friends), which is unrelated.
package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

var (
	bundleSyncTimeout = 30 * time.Second
	bundleGateRetry   = 30 * time.Second
)

var (
	bundlesGVR           = schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundles"}
	bundleDeploymentsGVR = schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundledeployments"}
	gitReposGVR          = schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "gitrepos"}
	bundleClustersGVR    = schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "clusters"}
)

// BundleEventKind classifies a BundleWatcher event. The kinds apply to
// every object the watcher carries, not only Bundles.
type BundleEventKind uint8

const (
	BundleAdded BundleEventKind = iota
	BundleUpdated
	BundleDeleted
	// BundleSynced follows the initial list of every watched resource,
	// and supersedes an earlier BundleUnavailable for "bundles".
	BundleSynced
	// BundleUnavailable says one resource will not be delivered.
	BundleUnavailable
)

// BundleUnavailableReason says why a resource isn't being watched.
type BundleUnavailableReason uint8

const (
	// BundleNoAPI: not a Fleet manager — the normal case for most clusters.
	BundleNoAPI BundleUnavailableReason = iota
	BundleForbidden
	// BundleCheckFailed: discovery or the access review errored. Retried
	// for "bundles"; the other resources stay off for this focus.
	BundleCheckFailed
)

// BundleEvent carries one Fleet object, or a watch-level marker.
type BundleEvent struct {
	Kind    BundleEventKind
	Context string
	UID     types.UID

	// Exactly one is set on Added, Updated and Deleted.
	Bundle     *Bundle
	Deployment *BundleDeployment
	GitRepo    *GitRepo
	Cluster    *BundleCluster

	// Synced: the namespace the informers watch, "" for all.
	WatchNamespace string

	// Unavailable: the Fleet resource (plural, as in the API) and why.
	Resource string
	Reason   BundleUnavailableReason
	Detail   string
}

// BundleSummary is Fleet's count of BundleDeployments per state, as
// carried on Bundles, GitRepos and Fleet Clusters. Fleet omits zero
// counts, so a missing count is zero, but a missing summary is unknown.
type BundleSummary struct {
	Known        bool
	DesiredReady int
	Ready        int
	NotReady     int
	WaitApplied  int
	ErrApplied   int
	OutOfSync    int
	Modified     int
	Pending      int
	// NonReady is Fleet's own list of what isn't ready, which it may cap.
	NonReady []BundleNonReady
}

type BundleNonReady struct {
	Name    string
	State   string
	Message string
}

type BundleCondition struct {
	Type       string
	Status     string
	Reason     string
	Message    string
	LastUpdate time.Time
}

type Bundle struct {
	Namespace string
	Name      string
	UID       types.UID
	// Repo and Commit are empty on bundles no GitRepo produced, such as
	// the agent bundles Rancher creates.
	Repo   string
	Commit string
	Paused bool
	// ObservedGeneration below Generation: the status describes an older spec.
	Generation         int64
	ObservedGeneration int64
	// State is the worst BundleDeployment state. Fleet leaves it empty
	// when every deployment is ready.
	State      string
	Summary    BundleSummary
	Conditions []BundleCondition
	CreatedAt  time.Time
}

// BundleDeployment is one Bundle on one cluster.
type BundleDeployment struct {
	Namespace        string
	Name             string
	UID              types.UID
	BundleNamespace  string
	BundleName       string
	ClusterNamespace string
	ClusterName      string
	Commit           string
	Paused           bool
	State            string
	// Release is the Helm release Fleet installed, "namespace/name:revision".
	Release    string
	Resources  BundleResourceCounts
	NotReady   []BundleObjectStatus
	Modified   []BundleModifiedObject
	Conditions []BundleCondition
}

type BundleResourceCounts struct {
	Known        bool
	DesiredReady int
	Ready        int
	NotReady     int
	Modified     int
	Missing      int
	Orphaned     int
	Unknown      int
	WaitApplied  int
}

// BundleObjectStatus is a deployed object Fleet reports as not ready.
type BundleObjectStatus struct {
	Kind          string
	Namespace     string
	Name          string
	State         string
	Message       string
	Error         bool
	Transitioning bool
}

// BundleModifiedObject is a deployed object that differs from the bundle.
type BundleModifiedObject struct {
	Kind      string
	Namespace string
	Name      string
	// Missing: should exist and doesn't. Delete: exists and shouldn't.
	Missing bool
	Delete  bool
	Patched bool
}

type GitRepo struct {
	Namespace string
	Name      string
	UID       types.UID
	Branch    string
	Revision  string
	// Commit is the commit Fleet last fetched.
	Commit             string
	Paused             bool
	Generation         int64
	ObservedGeneration int64
	Summary            BundleSummary
	Message            string
	Error              bool
	Conditions         []BundleCondition
	CreatedAt          time.Time
}

// BundleCluster is Fleet's Cluster resource: a cluster registered with
// the manager, and its agent.
type BundleCluster struct {
	Namespace string
	Name      string
	UID       types.UID
	// DisplayName is Rancher's name for the cluster; empty outside Rancher.
	DisplayName string
	State       string
	// AgentLastSeen is the agent's last check-in. Fleet keeps reporting
	// a silent agent's bundles as they last were, so this is the only
	// sign they may be stale. Zero when never reported.
	AgentLastSeen time.Time
	Summary       BundleSummary
	Conditions    []BundleCondition
}

type bundleResource struct {
	gvr     schema.GroupVersionResource
	keep    [][]string
	project func(*unstructured.Unstructured, *BundleEvent)
}

// bundleResources lists what the watcher carries. Bundles come first:
// they are the gate, the rest only enrich them.
var bundleResources = []bundleResource{
	{bundlesGVR, [][]string{
		{"spec", "paused"},
		{"status", "observedGeneration"}, {"status", "display"}, {"status", "summary"}, {"status", "conditions"},
	}, projectBundle},
	{bundleDeploymentsGVR, [][]string{
		{"spec", "paused"},
		{"status", "display"}, {"status", "release"}, {"status", "resourceCounts"},
		{"status", "nonReadyStatus"}, {"status", "modifiedStatus"}, {"status", "conditions"},
	}, projectBundleDeployment},
	{gitReposGVR, [][]string{
		{"spec", "branch"}, {"spec", "revision"}, {"spec", "paused"},
		{"status", "commit"}, {"status", "observedGeneration"}, {"status", "display"},
		{"status", "summary"}, {"status", "conditions"},
	}, projectGitRepo},
	{bundleClustersGVR, [][]string{
		{"status", "display"}, {"status", "agent", "lastSeen"}, {"status", "summary"}, {"status", "conditions"},
	}, projectBundleCluster},
}

var bundleMetaKeep = [][]string{
	{"metadata", "name"}, {"metadata", "namespace"}, {"metadata", "uid"},
	{"metadata", "resourceVersion"}, {"metadata", "generation"},
	{"metadata", "creationTimestamp"}, {"metadata", "labels"},
}

// BundleWatcher watches a Fleet manager's Bundles, BundleDeployments,
// GitRepos and Clusters. Safe to spawn on every cluster: anywhere
// without the Fleet API it reports BundleNoAPI and idles.
type BundleWatcher struct {
	Context string
	*eventDelivery[BundleEvent]
}

func NewBundleWatcher(ctxName string, cap int) *BundleWatcher {
	return &BundleWatcher{
		Context:       ctxName,
		eventDelivery: newEventDelivery[BundleEvent](cap),
	}
}

func (w *BundleWatcher) Run(ctx context.Context, sup *Supervisor) error {
	ctx, stop := w.start(ctx)
	defer stop()
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	scope := sup.ResolveScope(ctx, w.Context, clientset)

	watched := w.gate(ctx, sup, restCfg, scope)
	if len(watched) == 0 {
		<-ctx.Done()
		return nil
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, scope, nil)
	synced := make([]cache.InformerSynced, 0, len(watched))
	for _, r := range watched {
		informer := factory.ForResource(r.gvr).Informer()
		keep := r.keep
		if err := informer.SetTransform(func(obj any) (any, error) {
			return trimBundleObject(obj, keep), nil
		}); err != nil {
			return fmt.Errorf("transform %s: %w", r.gvr.Resource, err)
		}
		project := r.project
		handler, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { w.emit(BundleAdded, obj, project) },
			UpdateFunc: func(_, obj any) { w.emit(BundleUpdated, obj, project) },
			DeleteFunc: func(obj any) {
				if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = d.Obj
				}
				w.emit(BundleDeleted, obj, project)
			},
		})
		if err != nil {
			return fmt.Errorf("add handler %s: %w", r.gvr.Resource, err)
		}
		synced = append(synced, handler.HasSynced)
	}

	klog.Infof("bundlewatch[%s]: starting %d informers", w.Context, len(watched))
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, bundleSyncTimeout)
	ok := cache.WaitForCacheSync(syncCtx.Done(), synced...)
	syncCancel()
	if !ok {
		if ctx.Err() != nil {
			return nil
		}
		klog.Warningf("bundlewatch[%s]: initial cache sync delayed; retrying", w.Context)
		if !cache.WaitForCacheSync(ctx.Done(), synced...) {
			return nil
		}
	}
	klog.Infof("bundlewatch[%s]: synced", w.Context)

	// The empty UID is reserved for this ordered cache-completion marker.
	w.publish("", BundleEvent{Kind: BundleSynced, Context: w.Context, WatchNamespace: scope}, false)

	<-ctx.Done()
	return nil
}

// gate picks the resources to watch, retrying while the Bundles check
// itself errors: a cluster briefly unreachable at focus time is not a
// cluster without Fleet.
func (w *BundleWatcher) gate(ctx context.Context, sup *Supervisor, restCfg *rest.Config, scope string) []bundleResource {
	for {
		watched, retry := w.check(ctx, sup, restCfg, scope)
		if !retry {
			return watched
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(bundleGateRetry):
		}
	}
}

func (w *BundleWatcher) check(ctx context.Context, sup *Supervisor, restCfg *rest.Config, scope string) ([]bundleResource, bool) {
	discCfg := rest.CopyConfig(restCfg)
	discCfg.Timeout = 10 * time.Second
	has, err := hasAPIResource(discCfg, bundlesGVR)
	if err != nil {
		w.unavailable(bundlesGVR.Resource, BundleCheckFailed, "discovery: "+err.Error())
		return nil, true
	}
	if !has {
		klog.Infof("bundlewatch[%s]: skipped (no fleet.cattle.io API)", w.Context)
		w.unavailable(bundlesGVR.Resource, BundleNoAPI, "")
		return nil, false
	}

	var watched []bundleResource
	for i, r := range bundleResources {
		res := sup.CanI(ctx, w.Context, "list", r.gvr.Group, r.gvr.Resource, scope)
		switch {
		case res.Err != "":
			w.unavailable(r.gvr.Resource, BundleCheckFailed, res.Err)
			if i == 0 {
				return nil, true
			}
		case !res.Allowed:
			w.unavailable(r.gvr.Resource, BundleForbidden, res.Reason)
			if i == 0 {
				return nil, false
			}
		default:
			watched = append(watched, r)
		}
	}
	return watched, false
}

func (w *BundleWatcher) unavailable(resource string, reason BundleUnavailableReason, detail string) {
	w.publish(types.UID("unavailable/"+resource), BundleEvent{
		Kind: BundleUnavailable, Context: w.Context,
		Resource: resource, Reason: reason, Detail: detail,
	}, false)
}

func (w *BundleWatcher) emit(kind BundleEventKind, obj any, project func(*unstructured.Unstructured, *BundleEvent)) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	ev := BundleEvent{Kind: kind, Context: w.Context, UID: u.GetUID()}
	project(u, &ev)
	w.publish(ev.UID, ev, kind == BundleDeleted)
}

// trimBundleObject rebuilds obj from the fields the projections read. A
// Bundle carries its manifests gzipped in spec.resources (hundreds of
// KB for a chart), a GitRepo lists every deployed object in its status,
// and Rancher stamps an applied-object annotation on both; the cache
// would otherwise hold all of it. Idempotent, as client-go requires.
func trimBundleObject(obj any, keep [][]string) any {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj
	}
	out := map[string]any{"apiVersion": u.GetAPIVersion(), "kind": u.GetKind()}
	for _, paths := range [][][]string{bundleMetaKeep, keep} {
		for _, p := range paths {
			if v, found, _ := unstructured.NestedFieldNoCopy(u.Object, p...); found {
				setNested(out, v, p)
			}
		}
	}
	return &unstructured.Unstructured{Object: out}
}

func setNested(obj map[string]any, v any, path []string) {
	for _, k := range path[:len(path)-1] {
		next, ok := obj[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			obj[k] = next
		}
		obj = next
	}
	obj[path[len(path)-1]] = v
}

func projectBundle(u *unstructured.Unstructured, ev *BundleEvent) {
	labels := u.GetLabels()
	status := nestedObj(u.Object, "status")
	ev.Bundle = &Bundle{
		Namespace:          u.GetNamespace(),
		Name:               u.GetName(),
		UID:                u.GetUID(),
		Repo:               labels["fleet.cattle.io/repo-name"],
		Commit:             labels["fleet.cattle.io/commit"],
		Paused:             nestedBool(u.Object, "spec", "paused"),
		Generation:         u.GetGeneration(),
		ObservedGeneration: nestedInt(status, "observedGeneration"),
		State:              nestedStr(status, "display", "state"),
		Summary:            bundleSummaryOf(nestedObj(status, "summary")),
		Conditions:         bundleConditionsOf(status),
		CreatedAt:          u.GetCreationTimestamp().Time,
	}
}

func projectBundleDeployment(u *unstructured.Unstructured, ev *BundleEvent) {
	labels := u.GetLabels()
	status := nestedObj(u.Object, "status")
	d := &BundleDeployment{
		Namespace:        u.GetNamespace(),
		Name:             u.GetName(),
		UID:              u.GetUID(),
		BundleNamespace:  labels["fleet.cattle.io/bundle-namespace"],
		BundleName:       labels["fleet.cattle.io/bundle-name"],
		ClusterNamespace: labels["fleet.cattle.io/cluster-namespace"],
		ClusterName:      labels["fleet.cattle.io/cluster"],
		Commit:           labels["fleet.cattle.io/commit"],
		Paused:           nestedBool(u.Object, "spec", "paused"),
		State:            nestedStr(status, "display", "state"),
		Release:          nestedStr(status, "release"),
		Conditions:       bundleConditionsOf(status),
	}
	if rc := nestedObj(status, "resourceCounts"); rc != nil {
		d.Resources = BundleResourceCounts{
			Known:        true,
			DesiredReady: int(nestedInt(rc, "desiredReady")),
			Ready:        int(nestedInt(rc, "ready")),
			NotReady:     int(nestedInt(rc, "notReady")),
			Modified:     int(nestedInt(rc, "modified")),
			Missing:      int(nestedInt(rc, "missing")),
			Orphaned:     int(nestedInt(rc, "orphaned")),
			Unknown:      int(nestedInt(rc, "unknown")),
			WaitApplied:  int(nestedInt(rc, "waitApplied")),
		}
	}
	for _, o := range nestedObjs(status, "nonReadyStatus") {
		sum := nestedObj(o, "summary")
		var msgs []string
		for _, m := range nestedSlice(sum, "message") {
			if s, ok := m.(string); ok && s != "" {
				msgs = append(msgs, s)
			}
		}
		d.NotReady = append(d.NotReady, BundleObjectStatus{
			Kind:          nestedStr(o, "kind"),
			Namespace:     nestedStr(o, "namespace"),
			Name:          nestedStr(o, "name"),
			State:         nestedStr(sum, "state"),
			Message:       strings.Join(msgs, "; "),
			Error:         nestedBool(sum, "error"),
			Transitioning: nestedBool(sum, "transitioning"),
		})
	}
	for _, o := range nestedObjs(status, "modifiedStatus") {
		d.Modified = append(d.Modified, BundleModifiedObject{
			Kind:      nestedStr(o, "kind"),
			Namespace: nestedStr(o, "namespace"),
			Name:      nestedStr(o, "name"),
			Missing:   nestedBool(o, "missing"),
			Delete:    nestedBool(o, "delete"),
			Patched:   nestedStr(o, "patch") != "",
		})
	}
	ev.Deployment = d
}

func projectGitRepo(u *unstructured.Unstructured, ev *BundleEvent) {
	status := nestedObj(u.Object, "status")
	ev.GitRepo = &GitRepo{
		Namespace:          u.GetNamespace(),
		Name:               u.GetName(),
		UID:                u.GetUID(),
		Branch:             nestedStr(u.Object, "spec", "branch"),
		Revision:           nestedStr(u.Object, "spec", "revision"),
		Commit:             nestedStr(status, "commit"),
		Paused:             nestedBool(u.Object, "spec", "paused"),
		Generation:         u.GetGeneration(),
		ObservedGeneration: nestedInt(status, "observedGeneration"),
		Summary:            bundleSummaryOf(nestedObj(status, "summary")),
		Message:            nestedStr(status, "display", "message"),
		Error:              nestedBool(status, "display", "error"),
		Conditions:         bundleConditionsOf(status),
		CreatedAt:          u.GetCreationTimestamp().Time,
	}
}

func projectBundleCluster(u *unstructured.Unstructured, ev *BundleEvent) {
	status := nestedObj(u.Object, "status")
	ev.Cluster = &BundleCluster{
		Namespace:     u.GetNamespace(),
		Name:          u.GetName(),
		UID:           u.GetUID(),
		DisplayName:   u.GetLabels()["management.cattle.io/cluster-display-name"],
		State:         nestedStr(status, "display", "state"),
		AgentLastSeen: parseBundleTime(nestedStr(status, "agent", "lastSeen")),
		Summary:       bundleSummaryOf(nestedObj(status, "summary")),
		Conditions:    bundleConditionsOf(status),
	}
}

func bundleSummaryOf(m map[string]any) BundleSummary {
	if m == nil {
		return BundleSummary{}
	}
	s := BundleSummary{
		Known:        true,
		DesiredReady: int(nestedInt(m, "desiredReady")),
		Ready:        int(nestedInt(m, "ready")),
		NotReady:     int(nestedInt(m, "notReady")),
		WaitApplied:  int(nestedInt(m, "waitApplied")),
		ErrApplied:   int(nestedInt(m, "errApplied")),
		OutOfSync:    int(nestedInt(m, "outOfSync")),
		Modified:     int(nestedInt(m, "modified")),
		Pending:      int(nestedInt(m, "pending")),
	}
	for _, r := range nestedObjs(m, "nonReadyResources") {
		s.NonReady = append(s.NonReady, BundleNonReady{
			Name:    nestedStr(r, "name"),
			State:   nestedStr(r, "bundleState"),
			Message: nestedStr(r, "message"),
		})
	}
	return s
}

func bundleConditionsOf(status map[string]any) []BundleCondition {
	var out []BundleCondition
	for _, c := range nestedObjs(status, "conditions") {
		at := parseBundleTime(nestedStr(c, "lastUpdateTime"))
		if at.IsZero() {
			at = parseBundleTime(nestedStr(c, "lastTransitionTime"))
		}
		out = append(out, BundleCondition{
			Type:       nestedStr(c, "type"),
			Status:     nestedStr(c, "status"),
			Reason:     nestedStr(c, "reason"),
			Message:    nestedStr(c, "message"),
			LastUpdate: at,
		})
	}
	return out
}

func parseBundleTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nestedObj(m map[string]any, path ...string) map[string]any {
	v, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	obj, _ := v.(map[string]any)
	return obj
}

func nestedSlice(m map[string]any, path ...string) []any {
	v, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	s, _ := v.([]any)
	return s
}

func nestedObjs(m map[string]any, path ...string) []map[string]any {
	var out []map[string]any
	for _, v := range nestedSlice(m, path...) {
		if obj, ok := v.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

func nestedStr(m map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	s, _ := v.(string)
	return s
}

func nestedBool(m map[string]any, path ...string) bool {
	v, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	b, _ := v.(bool)
	return b
}

// nestedInt accepts both decodings of a JSON number: int64 from the
// API machinery, float64 from encoding/json.
func nestedInt(m map[string]any, path ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(m, path...)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}
