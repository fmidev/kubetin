package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func networkTestClient(t *testing.T, nodes []corev1.Node, scrape func(http.ResponseWriter, *http.Request, string)) *kubernetes.Clientset {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/nodes" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{Kind: "NodeList", APIVersion: "v1"}, Items: nodes})
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 8 && parts[4] != "" {
			scrape(w, r, parts[4])
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func readyNetworkNode(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
}

func writeNetworkCounters(w http.ResponseWriter, pod string) {
	fmt.Fprintf(w, "container_network_receive_bytes_total{namespace=\"ns\",pod=\"%s\"} 1000\n", pod)
}

func TestNetworkTickSlowNodeDoesNotStarvePeers(t *testing.T) {
	for _, slow := range []string{"a", "b"} {
		t.Run(slow, func(t *testing.T) {
			cs := networkTestClient(t, []corev1.Node{readyNetworkNode("a"), readyNetworkNode("b"), readyNetworkNode("c")}, func(w http.ResponseWriter, r *http.Request, node string) {
				if node == slow {
					<-r.Context().Done()
					return
				}
				writeNetworkCounters(w, node)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			p := NewNetworkPoller("test", 1)
			p.tick(ctx, cs)
			snap := <-p.Out
			if snap.OK || snap.NodesScraped != 2 || snap.NodesTotal != 3 || snap.Error == "" {
				t.Fatalf("slow node should yield explicit 2/3 coverage: %+v", snap)
			}
			seen := map[string]bool{}
			for _, pod := range snap.Pods {
				seen[pod.Name] = true
			}
			for _, node := range []string{"a", "b", "c"} {
				if seen[node] != (node != slow) {
					t.Errorf("node %s sampled=%t, slow=%s", node, seen[node], slow)
				}
			}
		})
	}
}

func TestNetworkTickCoverage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nodes       []corev1.Node
		fail        bool
		wantOK      bool
		wantScraped int
	}{
		{"empty", nil, false, true, 0},
		{"complete", []corev1.Node{readyNetworkNode("a"), readyNetworkNode("b")}, false, true, 2},
		{"failed", []corev1.Node{readyNetworkNode("a")}, true, false, 0},
		{"not-ready", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "down"}}}, false, false, 0},
		{"some-not-ready", []corev1.Node{readyNetworkNode("a"), {ObjectMeta: metav1.ObjectMeta{Name: "down"}}}, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := networkTestClient(t, tc.nodes, func(w http.ResponseWriter, r *http.Request, node string) {
				if node == "down" {
					t.Error("scraped a NotReady node")
				}
				if tc.fail {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				writeNetworkCounters(w, node)
			})
			p := NewNetworkPoller("test", 1)
			p.tick(context.Background(), cs)
			snap := <-p.Out
			if snap.OK != tc.wantOK || snap.NodesTotal != len(tc.nodes) || snap.NodesScraped != tc.wantScraped || (snap.Error == "") != tc.wantOK {
				t.Fatalf("unexpected coverage: %+v", snap)
			}
		})
	}
}

func TestNetworkScrapeIndividualDeadlines(t *testing.T) {
	var names []string
	for i := 0; i < networkScrapeWorkers*2; i++ {
		names = append(names, fmt.Sprint(i))
	}
	cs := networkTestClient(t, nil, func(w http.ResponseWriter, r *http.Request, node string) {
		if slices.Contains(names[:networkScrapeWorkers], node) {
			<-r.Context().Done()
			return
		}
		writeNetworkCounters(w, node)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := NewNetworkPoller("test", 1)
	_, scraped := p.scrapeNodes(ctx, cs, names, 100*time.Millisecond)
	if ctx.Err() != nil || scraped != networkScrapeWorkers {
		t.Fatalf("individual deadlines did not free workers for later nodes: scraped=%d, parent=%v", scraped, ctx.Err())
	}
}

func TestNetworkScrapeConcurrencyBoundAndCancellation(t *testing.T) {
	started := make(chan string, networkScrapeWorkers*3)
	cs := networkTestClient(t, nil, func(w http.ResponseWriter, r *http.Request, node string) {
		started <- node
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var names []string
	for i := 0; i < networkScrapeWorkers*3; i++ {
		names = append(names, fmt.Sprint(i))
	}
	done := make(chan struct{})
	go func() {
		NewNetworkPoller("test", 1).scrapeNodes(ctx, cs, names, time.Minute)
		close(done)
	}()
	for range networkScrapeWorkers {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("workers did not start concurrently")
		}
	}
	select {
	case <-started:
		t.Fatal("exceeded scrape concurrency limit")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop scrapes")
	}
	if len(started) != 0 {
		t.Fatal("queued scrapes started after cancellation")
	}
}

func TestNetworkScrapeUsesNodeCompletionTimes(t *testing.T) {
	var mu sync.Mutex
	var released time.Time
	cs := networkTestClient(t, nil, func(w http.ResponseWriter, r *http.Request, node string) {
		if node == "slow" {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			released = time.Now()
			mu.Unlock()
		}
		writeNetworkCounters(w, node)
	})
	p := NewNetworkPoller("test", 1)
	cur, scraped := p.scrapeNodes(context.Background(), cs, []string{"fast", "slow"}, time.Second)
	if scraped != 2 {
		t.Fatal("expected both nodes to be sampled")
	}
	mu.Lock()
	defer mu.Unlock()
	fast := cur[nodePodKey{node: "fast", podKey: podKey{"ns", "fast"}}]
	slow := cur[nodePodKey{node: "slow", podKey: podKey{"ns", "slow"}}]
	if slow.at.Before(released) || !fast.at.Before(slow.at) {
		t.Fatalf("sample times do not reflect node completion: fast=%s slow=%s release=%s", fast.at, slow.at, released)
	}
}

func TestNetworkRatesUseNodeBaselines(t *testing.T) {
	at := time.Unix(100, 0)
	a := nodePodKey{node: "a", podKey: podKey{"ns", "pod"}}
	b := nodePodKey{node: "b", podKey: a.podKey}
	prev := map[nodePodKey]counterSample{a: {rx: 100, tx: 400, at: at}, b: {rx: 1000, tx: 2000, at: at}}
	cur := map[nodePodKey]counterSample{a: {rx: 300, tx: 100, at: at.Add(2 * time.Second)}, b: {rx: 1200, tx: 2400, at: at.Add(4 * time.Second)}}
	pods, total := networkRates(cur, prev)
	if len(pods) != 1 || pods[0].RXBytesPerSec != 150 || pods[0].TXBytesPerSec != 100 || total.RXBytesPerSec != 150 || total.TXBytesPerSec != 100 {
		t.Fatalf("rates must use per-node intervals and clamp resets before summing: pods=%+v total=%+v", pods, total)
	}
	delete(prev, b)
	_, total = networkRates(cur, prev)
	if total.RXBytesPerSec != 100 || total.TXBytesPerSec != 0 {
		t.Fatalf("new node reused another node's baseline: %+v", total)
	}
}
