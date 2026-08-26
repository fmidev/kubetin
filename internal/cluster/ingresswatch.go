package cluster

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// IngEventKind classifies an Ingress cache event.
type IngEventKind uint8

const (
	IngAdded IngEventKind = iota
	IngUpdated
	IngDeleted
)

// IngressBackend is one service an Ingress routes to, already reduced
// to the "name:port" the table prints. Port is the service port name
// when the rule addresses it by name, otherwise the number.
type IngressBackend struct {
	Service string
	Port    string
}

// IngressEvent is the thin UI projection of an Ingress.
type IngressEvent struct {
	Kind      IngEventKind
	Context   string
	Namespace string
	Name      string
	UID       types.UID
	// Class is spec.ingressClassName, falling back to the legacy
	// kubernetes.io/ingress.class annotation that older controllers
	// still write and plenty of live clusters still carry.
	Class string
	Hosts []string
	// Backends is flattened across every rule path plus the default
	// backend, deduplicated — an Ingress with twenty paths onto one
	// service should read as one backend, not twenty.
	Backends []IngressBackend
	// Address is the load-balancer address the controller published.
	// Empty means "not admitted yet", which is the single most common
	// reason an Ingress silently doesn't answer.
	Address string
	// TLSHosts counts hosts covered by a spec.tls entry. Zero renders
	// as no TLS rather than as an error.
	TLSHosts  int
	CreatedAt time.Time
}

// IngressWatcher mirrors ServiceWatcher.
type IngressWatcher struct {
	Context       string
	Out           chan IngressEvent
	DroppedEvents atomic.Uint64
}

func NewIngressWatcher(ctxName string, cap int) *IngressWatcher {
	return &IngressWatcher{
		Context: ctxName,
		Out:     make(chan IngressEvent, cap),
	}
}

func (w *IngressWatcher) Run(ctx context.Context, sup *Supervisor) error {
	restCfg, err := sup.RestConfigFor(w.Context)
	if err != nil {
		return fmt.Errorf("rest config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	factory := newScopedFactory(clientset, sup.ResolveScope(ctx, w.Context, clientset))
	informer := factory.Networking().V1().Ingresses().Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(IngAdded, obj) },
		UpdateFunc: func(_, obj any) { w.emit(IngUpdated, obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.emit(IngDeleted, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("add handler: %w", err)
	}

	klog.Infof("ingresswatch[%s]: starting", w.Context)
	factory.Start(ctx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		klog.Errorf("ingresswatch[%s]: cache sync timed out after 30s", w.Context)
		return fmt.Errorf("ingress cache sync timed out (30s)")
	}
	klog.Infof("ingresswatch[%s]: synced, %d initial ingresses", w.Context, len(informer.GetStore().List()))

	<-ctx.Done()
	return nil
}

// legacyIngressClassAnnotation predates spec.ingressClassName and is
// still the only class marker on plenty of running Ingresses.
const legacyIngressClassAnnotation = "kubernetes.io/ingress.class"

func (w *IngressWatcher) emit(kind IngEventKind, obj any) {
	ing, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return
	}

	class := ""
	if ing.Spec.IngressClassName != nil {
		class = *ing.Spec.IngressClassName
	}
	if class == "" {
		class = ing.Annotations[legacyIngressClassAnnotation]
	}

	hosts := make([]string, 0, len(ing.Spec.Rules))
	seenHost := make(map[string]struct{}, len(ing.Spec.Rules))
	backends := make([]IngressBackend, 0, 4)
	seenBackend := make(map[IngressBackend]struct{}, 4)

	addBackend := func(b *networkingv1.IngressBackend) {
		if b == nil || b.Service == nil {
			return
		}
		port := b.Service.Port.Name
		if port == "" && b.Service.Port.Number != 0 {
			port = itoaPort(b.Service.Port.Number)
		}
		be := IngressBackend{Service: b.Service.Name, Port: port}
		if _, dup := seenBackend[be]; dup {
			return
		}
		seenBackend[be] = struct{}{}
		backends = append(backends, be)
	}

	addBackend(ing.Spec.DefaultBackend)
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			if _, dup := seenHost[rule.Host]; !dup {
				seenHost[rule.Host] = struct{}{}
				hosts = append(hosts, rule.Host)
			}
		}
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			addBackend(&rule.HTTP.Paths[i].Backend)
		}
	}

	address := ""
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		switch {
		case lb.IP != "":
			address = lb.IP
		case lb.Hostname != "":
			address = lb.Hostname
		}
		if address != "" {
			break
		}
	}

	tlsHosts := 0
	for _, t := range ing.Spec.TLS {
		tlsHosts += len(t.Hosts)
	}

	ev := IngressEvent{
		Kind:      kind,
		Context:   w.Context,
		Namespace: ing.Namespace,
		Name:      ing.Name,
		UID:       ing.UID,
		Class:     class,
		Hosts:     hosts,
		Backends:  backends,
		Address:   address,
		TLSHosts:  tlsHosts,
		CreatedAt: ing.CreationTimestamp.Time,
	}
	select {
	case w.Out <- ev:
	default:
		w.DroppedEvents.Add(1)
	}
}
