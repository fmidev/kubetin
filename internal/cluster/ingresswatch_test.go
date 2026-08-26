package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func svcBackend(name string, port int32, portName string) *networkingv1.IngressBackend {
	b := &networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{Name: name},
	}
	if portName != "" {
		b.Service.Port.Name = portName
	} else {
		b.Service.Port.Number = port
	}
	return b
}

func collectIngress(t *testing.T, ing *networkingv1.Ingress) IngressEvent {
	t.Helper()
	w := NewIngressWatcher("test", 1)
	w.emit(IngAdded, ing)
	select {
	case ev := <-w.Out:
		return ev
	default:
		t.Fatal("watcher emitted nothing")
		return IngressEvent{}
	}
}

// Backends are flattened across every rule path plus the default
// backend, and deduplicated: an Ingress fanning twenty paths onto one
// service should read as one backend, not twenty.
func TestIngressBackendsFlattenAndDedupe(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			DefaultBackend: svcBackend("fallback", 8080, ""),
			Rules: []networkingv1.IngressRule{
				{
					Host: "a.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{Backend: *svcBackend("api", 80, "")},
								{Backend: *svcBackend("api", 80, "")}, // duplicate path, same backend
								{Backend: *svcBackend("ui", 0, "http")},
							},
						},
					},
				},
				{
					// Same host repeated across rules must appear once.
					Host: "a.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{Backend: *svcBackend("api", 80, "")},
							},
						},
					},
				},
				{Host: "b.example.com"}, // no HTTP block at all
			},
		},
	}

	ev := collectIngress(t, ing)

	wantHosts := []string{"a.example.com", "b.example.com"}
	if len(ev.Hosts) != len(wantHosts) {
		t.Fatalf("hosts = %v, want %v", ev.Hosts, wantHosts)
	}
	for i, h := range wantHosts {
		if ev.Hosts[i] != h {
			t.Errorf("host %d = %q, want %q", i, ev.Hosts[i], h)
		}
	}

	want := []IngressBackend{
		{Service: "fallback", Port: "8080"},
		{Service: "api", Port: "80"},
		{Service: "ui", Port: "http"},
	}
	if len(ev.Backends) != len(want) {
		t.Fatalf("backends = %v, want %v", ev.Backends, want)
	}
	for i, b := range want {
		if ev.Backends[i] != b {
			t.Errorf("backend %d = %+v, want %+v", i, ev.Backends[i], b)
		}
	}
}

// spec.ingressClassName is the modern field, but plenty of live
// Ingresses only carry the legacy annotation.
func TestIngressClassFallsBackToAnnotation(t *testing.T) {
	modern := "nginx"
	ev := collectIngress(t, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       networkingv1.IngressSpec{IngressClassName: &modern},
	})
	if ev.Class != "nginx" {
		t.Errorf("class = %q, want nginx from spec", ev.Class)
	}

	ev = collectIngress(t, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "b",
			Annotations: map[string]string{legacyIngressClassAnnotation: "traefik"},
		},
	})
	if ev.Class != "traefik" {
		t.Errorf("class = %q, want traefik from the legacy annotation", ev.Class)
	}

	if ev := collectIngress(t, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
	}); ev.Class != "" {
		t.Errorf("class = %q, want empty when neither is set", ev.Class)
	}
}

// The published address may arrive as an IP or a hostname depending on
// the cloud provider; both mean "admitted".
func TestIngressAddressFromIPOrHostname(t *testing.T) {
	withStatus := func(lb networkingv1.IngressLoadBalancerIngress) IngressEvent {
		return collectIngress(t, &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Status: networkingv1.IngressStatus{
				LoadBalancer: networkingv1.IngressLoadBalancerStatus{
					Ingress: []networkingv1.IngressLoadBalancerIngress{lb},
				},
			},
		})
	}
	if ev := withStatus(networkingv1.IngressLoadBalancerIngress{IP: "34.88.10.4"}); ev.Address != "34.88.10.4" {
		t.Errorf("address = %q, want the IP", ev.Address)
	}
	if ev := withStatus(networkingv1.IngressLoadBalancerIngress{Hostname: "lb.example.com"}); ev.Address != "lb.example.com" {
		t.Errorf("address = %q, want the hostname", ev.Address)
	}
	if ev := collectIngress(t, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
	}); ev.Address != "" {
		t.Errorf("address = %q, want empty for an un-admitted ingress", ev.Address)
	}
}

// A nil Ready condition means ready per the EndpointSlice API contract.
// Treating nil as not-ready would report 0/N on any cluster whose
// controller leaves the field unset.
func TestEndpointSliceNilReadyCountsAsReady(t *testing.T) {
	yes, no := true, false
	w := NewEndpointSliceWatcher("test", 1)
	w.emit(EndpointSliceAdded, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-abc", Namespace: "default",
			Labels: map[string]string{serviceNameLabel: "api"},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Conditions: discoveryv1.EndpointConditions{Ready: &yes}},
			{Conditions: discoveryv1.EndpointConditions{Ready: nil}},
			{Conditions: discoveryv1.EndpointConditions{Ready: &no}},
		},
	})

	ev := <-w.Out
	if ev.ServiceName != "api" {
		t.Errorf("service = %q, want api", ev.ServiceName)
	}
	if ev.Ready != 2 || ev.Total != 3 {
		t.Errorf("ready/total = %d/%d, want 2/3 (nil counts as ready)", ev.Ready, ev.Total)
	}
}

// Slices without the service-name label belong to no Service and have
// no row to attach to.
func TestEndpointSliceWithoutServiceLabelDropped(t *testing.T) {
	w := NewEndpointSliceWatcher("test", 1)
	w.emit(EndpointSliceAdded, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "default"},
	})
	select {
	case ev := <-w.Out:
		t.Errorf("expected the unlabelled slice to be dropped, got %+v", ev)
	default:
	}
}

// The external column merges spec.externalIPs with the addresses the
// load-balancer controller published — the table shows one "external"
// column and the user doesn't care which field an address came from.
func TestServiceExternalIPsMerged(t *testing.T) {
	w := NewServiceWatcher("test", 1)
	w.emit(SvcAdded, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "lb", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:        corev1.ServiceTypeLoadBalancer,
			ExternalIPs: []string{"192.0.2.1"},
			Ports: []corev1.ServicePort{
				{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP,
					TargetPort: intstr.FromInt32(8080)},
			},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{IP: "34.88.10.4"}, {Hostname: "lb.example.com"},
				},
			},
		},
	})

	ev := <-w.Out
	want := []string{"192.0.2.1", "34.88.10.4", "lb.example.com"}
	if len(ev.ExternalIPs) != len(want) {
		t.Fatalf("external = %v, want %v", ev.ExternalIPs, want)
	}
	for i, v := range want {
		if ev.ExternalIPs[i] != v {
			t.Errorf("external[%d] = %q, want %q", i, ev.ExternalIPs[i], v)
		}
	}
	if len(ev.Ports) != 1 || ev.Ports[0].NodePort != 30080 {
		t.Errorf("ports = %+v, want the node port carried through", ev.Ports)
	}
}
