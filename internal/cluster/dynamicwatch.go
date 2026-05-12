// Shared scaffolding for watchers that talk to OpenShift-only APIs
// through the dynamic client. The concrete watchers
// (projectwatch.go, and later routewatch / buildconfigwatch for
// #21 / #22) wire their own GroupVersionResource + event projection;
// the helpers here are just the discovery probe that lets each
// watcher cleanly skip when the API surface doesn't include the
// resource (rather than letting the dynamic informer 404-loop in
// debug.log every few seconds).
package cluster

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// hasAPIResource returns true when the focused cluster's discovery
// endpoint lists the given GVR — i.e. the resource is part of the
// API surface. Used as the first check in each OpenShift watcher's
// Run loop; the second check is RBAC (Supervisor.CanI list verb).
//
// Discovery is cheap (single HTTP GET) and the result is naturally
// stable for the lifetime of a focused-cluster watcher group, so we
// query it once at Run start rather than caching.
func hasAPIResource(restCfg *rest.Config, gvr schema.GroupVersionResource) (bool, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return false, err
	}
	list, err := disc.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		// 404 from the apiserver → group not registered. That's a
		// negative answer, not an error from the caller's POV.
		return false, nil
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}
