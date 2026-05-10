// Mutating Deployment operations: Scale and Rollout-Restart.
//
// Both go through the typed clientset rather than the dynamic one so
// the apiserver does the strategic-merge work for us. Scale uses the
// /scale subresource for atomic semantics (won't fight a concurrent
// HPA edit); RolloutRestart matches kubectl's annotation patch on
// spec.template.metadata so existing observability stays accurate.
package cluster

import (
	"context"
	"fmt"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ScaleResult is the outcome of a Scale call. Context lets UI
// receivers drop a result that arrived after the user Tabbed away
// (see DeleteResult comment for the same pattern).
type ScaleResult struct {
	Context  string
	Ref      DescribeRef
	Replicas int32
	OK       bool
	Err      string
}

// RolloutResult is the outcome of a RolloutRestart call. Context as
// above.
type RolloutResult struct {
	Context string
	Ref     DescribeRef
	OK      bool
	Err     string
}

// Scale sets the deployment's replica count via the /scale subresource.
// Read-modify-write rather than a blind PATCH so we don't trample a
// concurrent HPA decision (its writer-side conflict will surface as a
// 409 the user can react to instead of silently overwriting).
func (s *Supervisor) Scale(ctx context.Context, ctxName string, ref DescribeRef, replicas int32) ScaleResult {
	out := ScaleResult{Context: ctxName, Ref: ref, Replicas: replicas}
	if ref.Kind != "Deployment" {
		out.Err = "Scale only supported on Deployment"
		return out
	}
	if replicas < 0 {
		out.Err = "replicas must be ≥ 0"
		return out
	}
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

	scaleObj, err := cs.AppsV1().Deployments(ref.Namespace).
		GetScale(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		out.Err = err.Error()
		return out
	}
	scaleObj.Spec.Replicas = replicas
	updated, err := cs.AppsV1().Deployments(ref.Namespace).
		UpdateScale(ctx, ref.Name, scaleObj, metav1.UpdateOptions{})
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.Replicas = updated.Spec.Replicas
	out.OK = true
	_ = autoscalingv1.SchemeGroupVersion // keep import alive in case we extend later
	return out
}

// RolloutRestart bumps a template annotation that forces a new
// ReplicaSet, matching `kubectl rollout restart`. We could pause the
// deployment first for safety but that's not what kubectl does and
// would change the user's expectations.
func (s *Supervisor) RolloutRestart(ctx context.Context, ctxName string, ref DescribeRef) RolloutResult {
	out := RolloutResult{Context: ctxName, Ref: ref}
	if ref.Kind != "Deployment" {
		out.Err = "Rollout restart only supported on Deployment"
		return out
	}
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

	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339)))
	_, err = cs.AppsV1().Deployments(ref.Namespace).
		Patch(ctx, ref.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.OK = true
	return out
}
