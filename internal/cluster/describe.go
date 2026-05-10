package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// DescribeRef points to a single resource we want to fetch + render.
// Kind-agnostic so future views (services, ingresses, CRDs) reuse it.
type DescribeRef struct {
	Group     string // empty for core
	Version   string
	Resource  string // plural, lowercase: "pods", "nodes", "deployments", "secrets"
	Kind      string // singular, capitalized: "Pod", "Node", "Deployment", "Secret"
	Namespace string // empty for cluster-scoped
	Name      string
}

// DescribeResult is what the UI renders. Context is the kubeconfig
// context the fetch ran against; UI receivers compare it to
// m.WatchedContext and drop stale results so a slow Describe that
// returns after the user Tabbed away can't overwrite the new view.
type DescribeResult struct {
	Context  string
	Ref      DescribeRef
	YAML     string
	Redacted bool
	Err      string
	At       time.Time
}

// Describe fetches the resource via the dynamic client and returns
// its YAML. Secret data values redact to `<redacted, N bytes>` unless
// reveal is true.
func (s *Supervisor) Describe(ctx context.Context, ctxName string, ref DescribeRef, reveal bool) DescribeResult {
	out := DescribeResult{Context: ctxName, Ref: ref, At: time.Now()}

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

	obj, err := resource.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		out.Err = err.Error()
		return out
	}

	stripManagedFields(obj.Object)

	if ref.Kind == "Secret" && !reveal {
		out.Redacted = redactSecretData(obj.Object)
	}

	b, err := yaml.Marshal(obj.Object)
	if err != nil {
		out.Err = "marshal: " + err.Error()
		return out
	}
	out.YAML = string(b)
	return out
}

// stripManagedFields removes metadata.managedFields, which can be 80%
// of a Pod YAML and is noise for human reading.
func stripManagedFields(o map[string]any) {
	meta, ok := o["metadata"].(map[string]any)
	if !ok {
		return
	}
	delete(meta, "managedFields")
}

// redactSecretData rewrites .data AND .stringData values to
// `<redacted, N bytes>`. Both fields are sensitive — when a watch
// cache hits before admission has normalised stringData into data,
// the apiserver returns stringData verbatim. Redacting only "data"
// would let those bytes through while the UI banner claims the
// secret is redacted. Returns true if any redaction happened.
func redactSecretData(o map[string]any) bool {
	changed := false
	// .data values are base64-encoded; size is the decoded byte count.
	if data, ok := o["data"].(map[string]any); ok {
		for k, v := range data {
			s, isStr := v.(string)
			if !isStr {
				continue
			}
			raw, _ := base64.StdEncoding.DecodeString(s)
			size := len(raw)
			if size == 0 {
				size = len(s)
			}
			data[k] = fmt.Sprintf("<redacted, %d bytes>", size)
			changed = true
		}
	}
	// .stringData values are plaintext; size is the string length.
	if sd, ok := o["stringData"].(map[string]any); ok {
		for k, v := range sd {
			s, isStr := v.(string)
			if !isStr {
				continue
			}
			sd[k] = fmt.Sprintf("<redacted, %d bytes>", len(s))
			changed = true
		}
	}
	return changed
}
