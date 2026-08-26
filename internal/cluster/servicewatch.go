package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// SvcEventKind classifies a Service cache event.
type SvcEventKind uint8

const (
	SvcAdded SvcEventKind = iota
	SvcUpdated
	SvcDeleted
)

// ServicePort is one entry of spec.ports, projected to what the table
// prints. NodePort is zero for anything but NodePort/LoadBalancer.
type ServicePort struct {
	Port     int32
	NodePort int32
	Protocol string
}

// ServiceEvent is the thin UI projection of a Service.
type ServiceEvent struct {
	Kind      SvcEventKind
	Context   string
	Namespace string
	Name      string
	UID       types.UID
	Type      string // ClusterIP | NodePort | LoadBalancer | ExternalName
	ClusterIP string
	// ExternalIPs merges spec.externalIPs with the LoadBalancer
	// ingress addresses the controller assigned, because the table
	// shows one "external" column and the user doesn't care which
	// field an address arrived in. Empty while a LoadBalancer is still
	// pending, which the UI renders distinctly from "not applicable".
	ExternalIPs []string
	// ExternalName carries spec.externalName for type=ExternalName,
	// where there is no cluster IP to show at all.
	ExternalName string
	Ports        []ServicePort
	Selector     map[string]string
	CreatedAt    time.Time
}

// ServiceWatcher mirrors DeployWatcher: a namespaced informer feeding a
// bounded channel, dropping on consumer backpressure.
type ServiceWatcher struct {
	Context       string
	Out           chan ServiceEvent
	DroppedEvents atomic.Uint64
}

func NewServiceWatcher(ctxName string, cap int) *ServiceWatcher {
	return &ServiceWatcher{
		Context: ctxName,
		Out:     make(chan ServiceEvent, cap),
	}
}

func (w *ServiceWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Core().V1().Services().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(SvcAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(SvcUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(SvcDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("servicewatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("servicewatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("service cache sync timed out (30s)")
	}
	klog.Infof("servicewatch[%s]: synced, %d initial services", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

func (w *ServiceWatcher) emit(kind SvcEventKind, obj any) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return
	}

	ports := make([]ServicePort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, ServicePort{
			Port:     p.Port,
			NodePort: p.NodePort,
			Protocol: string(p.Protocol),
		})
	}

	external := append([]string(nil), svc.Spec.ExternalIPs...)
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		switch {
		case ing.IP != "":
			external = append(external, ing.IP)
		case ing.Hostname != "":
			external = append(external, ing.Hostname)
		}
	}

	ev := ServiceEvent{
		Kind:         kind,
		Context:      w.Context,
		Namespace:    svc.Namespace,
		Name:         svc.Name,
		UID:          svc.UID,
		Type:         string(svc.Spec.Type),
		ClusterIP:    svc.Spec.ClusterIP,
		ExternalIPs:  external,
		ExternalName: svc.Spec.ExternalName,
		Ports:        ports,
		Selector:     copyLabels(svc.Spec.Selector),
		CreatedAt:    svc.CreationTimestamp.Time,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
