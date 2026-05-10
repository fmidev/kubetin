// Shared informer factory helper.
//
// All watchers (pod, deploy, event) build their factory through this
// helper so the namespace-scoping decision lives in one place. When
// the kubeconfig context has a namespace pinned (e.g. OpenShift after
// `oc project foo`), we scope the informer there — cluster-wide LIST
// of pods/events/deployments 403s for users without cluster-scope
// roles. Empty ns falls back to a cluster-wide factory.
//
// The Node watcher is intentionally NOT routed through this helper:
// nodes are cluster-scoped, so there's no namespaced fallback. That
// watcher handles RBAC denial in its own Run loop.
package cluster

import (
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

func newScopedFactory(cs *kubernetes.Clientset, namespace string) informers.SharedInformerFactory {
	if namespace == "" {
		return informers.NewSharedInformerFactory(cs, 0)
	}
	return informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithNamespace(namespace),
	)
}
