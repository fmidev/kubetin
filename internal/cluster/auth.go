package cluster

import (
	"context"
	"fmt"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// CanIResult reports whether a given verb is permitted on a resource.
type CanIResult struct {
	Allowed bool
	Reason  string // populated when not Allowed (denial reason from SSAR)
	Err     string // transport / RBAC error
}

// CanI runs a SelfSubjectAccessReview against the cluster. Same
// mechanism as `kubectl auth can-i`. The check is cheap (a single
// API call, no resource list) and is the right way to gate UI
// actions on RBAC.
//
// The `resource` argument may carry an embedded subresource (e.g.
// "pods/log", "deployments/scale"). Built-in RBAC's resourceMatches
// happens to accept the combined form, but stricter authorizers
// (webhook, OPA Gatekeeper, OpenShift, etc.) demand the split. We
// always split on "/" and populate the structured fields.
func (s *Supervisor) CanI(ctx context.Context, ctxName, verb, group, resource, namespace string) CanIResult {
	out := CanIResult{}
	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		out.Err = "kubeconfig: " + err.Error()
		return out
	}
	restCfg.Timeout = 5 * time.Second

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		out.Err = "client: " + err.Error()
		return out
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res2, sub2 := splitResourceSubresource(resource)
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Verb:        verb,
				Group:       group,
				Resource:    res2,
				Subresource: sub2,
				Namespace:   namespace,
			},
		},
	}
	res, err := clientset.AuthorizationV1().
		SelfSubjectAccessReviews().
		Create(probeCtx, review, metav1.CreateOptions{})
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.Allowed = res.Status.Allowed
	out.Reason = res.Status.Reason
	return out
}

// DeleteResult is the outcome of a Delete call. Context is the
// kubeconfig context the call was issued against; UI handlers compare
// it to m.WatchedContext and drop stale results so a refocus during
// an in-flight delete can't show a toast for the wrong cluster.
type DeleteResult struct {
	Context string
	Ref     DescribeRef
	OK      bool
	Err     string
}

// Delete removes a resource via the dynamic client. We do NOT pass
// foreground/background propagation here yet — the apiserver default
// (background) is fine for pod deletes (the controller respawns).
func (s *Supervisor) Delete(ctx context.Context, ctxName string, ref DescribeRef) DeleteResult {
	out := DeleteResult{Context: ctxName, Ref: ref}
	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		out.Err = "kubeconfig: " + err.Error()
		return out
	}
	restCfg.Timeout = 10 * time.Second

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		out.Err = "dynamic client: " + err.Error()
		return out
	}

	gvr := schema.GroupVersionResource{Group: ref.Group, Version: ref.Version, Resource: ref.Resource}
	var resource dynamic.ResourceInterface = dyn.Resource(gvr)
	if ref.Namespace != "" {
		resource = dyn.Resource(gvr).Namespace(ref.Namespace)
	}

	delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := resource.Delete(delCtx, ref.Name, metav1.DeleteOptions{}); err != nil {
		out.Err = err.Error()
		return out
	}
	out.OK = true
	return out
}

// VerbForAction returns the canonical kube verb string for an action
// label, used both as the SSAR verb and as the cache key.
func VerbForAction(action string) string {
	switch action {
	case "delete":
		return "delete"
	case "logs":
		return "get" // GET on the pods/log subresource
	case "describe":
		return "get"
	}
	return action
}

// VerbDescriptor pairs a verb with the GVR it applies to. Helps the
// UI build SSAR keys consistently.
type VerbDescriptor struct {
	Verb     string
	Group    string
	Resource string
}

// PermissionKey forms the deduplicating cache key.
func PermissionKey(ctxName, verb, group, resource, namespace string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", ctxName, verb, group, resource, namespace)
}

// splitResourceSubresource accepts the combined-form Resource string
// kubetin's UI uses ("pods/log", "deployments/scale") and returns the
// structured (Resource, Subresource) the SSAR ResourceAttributes
// schema expects. A bare resource without a slash returns
// (resource, "").
func splitResourceSubresource(s string) (string, string) {
	if i := indexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
