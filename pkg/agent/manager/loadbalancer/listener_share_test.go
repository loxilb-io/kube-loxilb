package loadbalancer

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	tk "github.com/loxilb-io/loxilib"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
)

// poolService - a Service generated from InferencePool pool, serving model on
// its Gateway listener gatewayIP:8080/tcp.
func poolService(pool, model, gatewayIP string, nodePort int32) *corev1.Service {
	class := addTestClass
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: pool + "-inference", Namespace: "llm",
			Labels:      map[string]string{inferencePoolLabel: pool},
			Annotations: map[string]string{lbModeAnnotation: "fullproxy"},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &class, LoadBalancerIP: gatewayIP,
			Ports: []corev1.ServicePort{{
				Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8000), NodePort: nodePort,
			}},
		},
	}
	if model != "" {
		svc.Annotations[modelNameAnnotation] = model
	}
	return svc
}

// cachePairs - put a Service's pairs in the cache, as a sync that installed
// them would.
func cachePairs(m *Manager, svc *corev1.Service, entry *LbCacheEntry, pairs []SvcPair) {
	for _, p := range pairs {
		entry.LbServicePairs[GenSPKey(p.IPString, uint16(p.Port), p.Protocol)] = &LbServicePairEntry{
			ExternalIP: p.IPString, Port: uint16(p.Port), Protocol: p.Protocol,
			InRange: p.InRange, StaticIP: p.StaticIP, IdentIPAM: p.IdentIPAM,
		}
	}
	m.lbCache[GenKey(svc.Namespace, svc.Name)] = entry
}

// newPoolEntry - the cache entry addLoadBalancer starts a pool's Service with.
func newPoolEntry(pool *ippool.IPPool, svc *corev1.Service) *LbCacheEntry {
	return &LbCacheEntry{IPPool: pool, LbServicePairs: map[string]*LbServicePairEntry{}, ListenerAddr: listenerAddrOf(svc)}
}

// The pools on one listener share its address and port: the first is given
// them, the others do not ask the address pool again.
func TestPoolsOnOneListenerShareItsPort(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	m := &Manager{networkConfig: &config.NetworkConfig{}, lbCache: LbCacheTable{}}

	a := poolService("a", "qwen", "192.168.0.13", 30001)
	entryA := newPoolEntry(pool, a)
	pairsA, err, _ := m.getIngressSvcPairs(a, entryA)
	if err != nil {
		t.Fatalf("first pool: %v", err)
	}
	if len(pairsA) != 1 || pairsA[0].IPString != "192.168.0.13" || pairsA[0].Port != 8080 || !pairsA[0].IPAllocd {
		t.Fatalf("first pool pairs = %+v, want 192.168.0.13:8080 allocated by it", pairsA)
	}
	cachePairs(m, a, entryA, pairsA)

	b := poolService("b", "llama", "192.168.0.13", 30002)
	pairsB, err, _ := m.getIngressSvcPairs(b, newPoolEntry(pool, b))
	if err != nil {
		t.Fatalf("second pool on the listener: %v", err)
	}
	if len(pairsB) != 1 || pairsB[0].IPString != "192.168.0.13" || pairsB[0].Port != 8080 ||
		pairsB[0].IPAllocd || pairsB[0].IdentIPAM != pairsA[0].IdentIPAM {
		t.Errorf("second pool pairs = %+v, want 192.168.0.13:8080 shared, not allocated again", pairsB)
	}
}

// After a restart the Services already carry the address; the pools after the
// first share it as well, rather than each counting it as theirs.
func TestRestoredPoolsShareTheirListener(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	m := &Manager{networkConfig: &config.NetworkConfig{}, lbCache: LbCacheTable{}}
	published := []corev1.LoadBalancerIngress{{IP: "192.168.0.13"}}

	a := poolService("a", "qwen", "192.168.0.13", 30001)
	a.Status.LoadBalancer.Ingress = published
	entryA := newPoolEntry(pool, a)
	pairsA, err, _ := m.getIngressSvcPairs(a, entryA)
	if err != nil || len(pairsA) != 1 {
		t.Fatalf("first pool: %+v, %v", pairsA, err)
	}
	cachePairs(m, a, entryA, pairsA)

	b := poolService("b", "llama", "192.168.0.13", 30002)
	b.Status.LoadBalancer.Ingress = published
	pairsB, err, _ := m.getIngressSvcPairs(b, newPoolEntry(pool, b))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	if len(pairsB) != 1 || pairsB[0].IPAllocd || pairsB[0].IdentIPAM != pairsA[0].IdentIPAM {
		t.Errorf("second pool pairs = %+v, want the first pool's shared", pairsB)
	}
}

// The shared port goes back to the address pool with the last Service on it,
// not the first one deleted.
func TestSharedPortGoesBackWithTheLastPool(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	m := &Manager{networkConfig: &config.NetworkConfig{}, lbCache: LbCacheTable{}, LoxiClients: api.NewLoxiClientPool()}
	for _, svc := range []*corev1.Service{
		poolService("a", "qwen", "192.168.0.13", 30001),
		poolService("b", "llama", "192.168.0.13", 30002),
	} {
		entry := newPoolEntry(pool, svc)
		pairs, err, _ := m.getIngressSvcPairs(svc, entry)
		if err != nil {
			t.Fatalf("%s: %v", svc.Name, err)
		}
		cachePairs(m, svc, entry, pairs)
	}
	plainTakes := func() error {
		_, err, _ := m.getIngressSvcPairs(portService("plain", 8080), &LbCacheEntry{IPPool: pool, LbServicePairs: map[string]*LbServicePairEntry{}})
		return err
	}

	if err := m.deleteLoadBalancer("llm", "a-inference", true); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if err := plainTakes(); !errors.Is(err, ErrExternalIPUnavailable) {
		t.Errorf("the port was released while b still uses it: %v", err)
	}

	if err := m.deleteLoadBalancer("llm", "b-inference", true); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if err := plainTakes(); err != nil {
		t.Errorf("the port did not go back with the last pool: %v", err)
	}
}

// A port held by a Service that is not a pool's - the Gateway's ingress
// service on its way out - is waited for, not shared.
func TestAPoolWaitsForAPortHeldByAnotherService(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	heldIP, _ := pool.GetNewIPAddr("llm/inference-gw-ingress-service", 8080, "tcp")
	m := &Manager{networkConfig: &config.NetworkConfig{}, lbCache: LbCacheTable{
		"llm/inference-gw-ingress-service": {LbServicePairs: map[string]*LbServicePairEntry{
			GenSPKey(heldIP.String(), 8080, "tcp"): {ExternalIP: heldIP.String(), Port: 8080, Protocol: "tcp", InRange: true},
		}},
	}}

	svc := poolService("a", "qwen", "192.168.0.13", 30001)
	_, err, _ = m.getIngressSvcPairs(svc, newPoolEntry(pool, svc))
	if !errors.Is(err, ErrExternalIPUnavailable) || !strings.Contains(err.Error(), "llm/inference-gw-ingress-service") {
		t.Errorf("err = %v, want ErrExternalIPUnavailable naming the ingress service", err)
	}
}

// End to end: two pools on one Gateway listener, each with its own model, are
// both installed on the gateway, on the listener's address and port.
func TestTwoPoolsOnOneListenerAreBothInstalled(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	peer := gw.client(t)
	peer.IsAlive = true
	m, _ := addHarness(t, peer)

	a := poolService("a", "qwen", "192.168.0.13", 30001)
	b := poolService("b", "llama", "192.168.0.13", 30002)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range []*corev1.Service{a, b} {
		if _, err := m.kubeClient.CoreV1().Services("llm").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", svc.Name, err)
		}
		_ = indexer.Add(svc)
	}
	m.serviceLister = corelisters.NewServiceLister(indexer)

	for _, svc := range []*corev1.Service{a, b} {
		if err := m.addLoadBalancer(svc); err != nil {
			t.Fatalf("addLoadBalancer(%s) = %v", svc.Name, err)
		}
	}

	var models []string
	gw.mu.Lock()
	for _, body := range gw.bodies {
		var rule api.LoadBalancerModel
		if err := json.Unmarshal([]byte(body), &rule); err != nil {
			t.Fatalf("decode rule: %v", err)
		}
		if rule.Service.ExternalIP != "192.168.0.13" || rule.Service.Port != 8080 {
			t.Errorf("rule %s on %s:%d, want the listener 192.168.0.13:8080",
				rule.Service.Name, rule.Service.ExternalIP, rule.Service.Port)
		}
		models = append(models, rule.Service.ModelName)
	}
	gw.mu.Unlock()
	sort.Strings(models)
	if strings.Join(models, ",") != "llama,qwen" {
		t.Errorf("models installed = %v, want both pools'", models)
	}
}
