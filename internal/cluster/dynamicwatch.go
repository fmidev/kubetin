// Shared scaffolding for watchers that talk to optional APIs through
// the dynamic client. The concrete watchers (projectwatch.go for
// OpenShift, bundlewatch.go for Rancher Fleet) wire their own
// GroupVersionResource + event projection;
// the helpers here are just the discovery probe that lets each
// watcher cleanly skip when the API surface doesn't include the
// resource (rather than letting the dynamic informer 404-loop in
// debug.log every few seconds).
package cluster

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// hasAPIResource returns true when the focused cluster's discovery
// endpoint lists the given GVR — i.e. the resource is part of the
// API surface. Used as the first check in each dynamic watcher's
// Run loop; the second check is RBAC (Supervisor.CanI list verb).
//
// Discovery is cheap (single HTTP GET) and the result is naturally
// stable for the lifetime of a focused-cluster watcher group, so we
// query it once at Run start rather than caching.
func hasAPIResource(restCfg *rest.Config, gvr schema.GroupVersionResource) (bool, error) {
	names, err := apiResourceNames(restCfg, gvr.GroupVersion())
	return names[gvr.Resource], err
}

// apiResourceNames returns the resources discovery lists for gv. A 404
// means the group isn't registered: an empty set, not an error.
// Anything else is a failed check — an unreachable cluster mustn't read
// as one without the API.
func apiResourceNames(restCfg *rest.Config, gv schema.GroupVersion) (map[string]bool, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	list, err := disc.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	names := make(map[string]bool, len(list.APIResources))
	for _, r := range list.APIResources {
		names[r.Name] = true
	}
	return names, nil
}
