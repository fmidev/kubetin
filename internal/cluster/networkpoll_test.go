package cluster

import (
	"testing"
	"time"
)

// parseCAdvisor must sum across interfaces and skip the system-cgroup
// rows that don't have pod labels.
func TestParseCAdvisor_PodAggregation(t *testing.T) {
	body := []byte(`# HELP container_network_receive_bytes_total foo
# TYPE container_network_receive_bytes_total counter
container_network_receive_bytes_total{namespace="default",pod="api",interface="eth0",image="x",name="abc"} 1000
container_network_receive_bytes_total{namespace="default",pod="api",interface="cni0",image="x",name="abc"} 500
container_network_receive_bytes_total{namespace="kube-system",pod="dns",interface="eth0",image="y",name="def"} 200
container_network_receive_bytes_total{interface="eth0",image="z",name="cgroup"} 99999
container_network_transmit_bytes_total{namespace="default",pod="api",interface="eth0",image="x",name="abc"} 800
`)
	now := time.Now()
	out := map[podKey]counterSample{}
	parseCAdvisor(body, now, out)

	api := out[podKey{ns: "default", name: "api"}]
	if api.rx != 1500 {
		t.Errorf("api.rx = %d, want 1500 (sum of two interfaces)", api.rx)
	}
	if api.tx != 800 {
		t.Errorf("api.tx = %d, want 800", api.tx)
	}
	dns := out[podKey{ns: "kube-system", name: "dns"}]
	if dns.rx != 200 {
		t.Errorf("dns.rx = %d, want 200", dns.rx)
	}
	if _, ok := out[podKey{ns: "", name: ""}]; ok {
		t.Error("system cgroup line should be skipped (no pod label)")
	}
}

// Counter resets (pod restart) must not produce huge negative or
// over-flow rates. clampRate returns zero on a negative delta.
func TestClampRate_CounterReset(t *testing.T) {
	if r := clampRate(100, 1_000_000, 15); r != 0 {
		t.Errorf("clamp on negative delta = %d, want 0", r)
	}
	if r := clampRate(1_000_000, 100, 10); r != 99990 {
		t.Errorf("normal rate = %d, want 99990", r)
	}
	if r := clampRate(100, 100, 10); r != 0 {
		t.Errorf("zero delta = %d, want 0", r)
	}
}

// Lines with optional timestamps after the value must still parse.
func TestParseCAdvisor_WithTimestamp(t *testing.T) {
	body := []byte(`container_network_receive_bytes_total{namespace="ns",pod="p",interface="eth0"} 42 1700000000000`)
	out := map[podKey]counterSample{}
	parseCAdvisor(body, time.Now(), out)
	if got := out[podKey{ns: "ns", name: "p"}].rx; got != 42 {
		t.Errorf("rx = %d, want 42 (timestamp suffix should be stripped)", got)
	}
}
