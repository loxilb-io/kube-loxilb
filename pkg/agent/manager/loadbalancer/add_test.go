package loadbalancer

import (
	"context"
	"net"
	"net/http"
	"testing"

	tk "github.com/loxilb-io/loxilib"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
)

const addTestClass = "loxilb.io/loxilb"

// addHarness - a Manager that can run addLoadBalancer end to end for one plain
// Service, llm/web on 8080/tcp, with one node as its endpoint and the given
// peers.
func addHarness(t *testing.T, peers ...*api.LoxiClient) (*Manager, *corev1.Service) {
	t.Helper()

	class := addTestClass
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "llm"},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &class,
			Ports: []corev1.ServicePort{{
				Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(80), NodePort: 30080,
			}},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.10.10.1"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	nodes := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodes.Add(node); err != nil {
		t.Fatalf("add node: %v", err)
	}

	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}

	clients := api.NewLoxiClientPool()
	for _, peer := range peers {
		clients.AddLoxiClient(peer)
	}

	return &Manager{
		kubeClient:  fake.NewSimpleClientset(svc, node),
		LoxiClients: clients,
		networkConfig: &config.NetworkConfig{
			LoxilbLoadBalancerClass: addTestClass,
			ExternalCIDRPoolDefs:    []string{"defaultPool=192.168.0.13/32"},
			NumZoneInst:             1,
		},
		nodeLister:      corelisters.NewNodeLister(nodes),
		lbCache:         LbCacheTable{},
		ipPoolTbl:       map[string]*ippool.IPPool{"defaultPool": pool},
		ip6PoolTbl:      map[string]*ippool.IPPool{},
		zoneInstRoleMap: map[string]*LoxiInstRole{},
		loxiInstAddrMap: map[string]net.IP{},
		eventRecorder:   record.NewFakeRecorder(100),
		queue:           testQueue(),
	}, svc
}

// testQueue - a work queue like the manager's, for code that queues Services.
func testQueue() workqueue.RateLimitingInterface {
	return workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "test")
}

func posts(f *fakeLoxiLB) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// current - the Service as the informer hands it over on the next sync, with
// the status the previous sync wrote.
func current(t *testing.T, m *Manager) *corev1.Service {
	t.Helper()
	svc, err := m.kubeClient.CoreV1().Services("llm").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	return svc
}

func ingressOf(t *testing.T, m *Manager) []corev1.LoadBalancerIngress {
	t.Helper()
	return current(t, m).Status.LoadBalancer.Ingress
}

// One loxilb that takes the rule: programmed and published as before, and a
// later sync with nothing changed asks nothing of the peer.
func TestAddSingleLoxilbSuccessIsUnchanged(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	peer.IsAlive = true
	m, svc := addHarness(t, peer)

	if err := m.addLoadBalancer(svc); err != nil {
		t.Fatalf("addLoadBalancer = %v", err)
	}
	if len(ingressOf(t, m)) == 0 {
		t.Error("the address was not published")
	}
	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("second sync = %v", err)
	}
	if n := posts(gw); n != 1 {
		t.Errorf("POSTed %d times over two syncs, want 1", n)
	}
}

// One loxilb that refuses the rule: the whole add fails as before - not
// cached, retried by the worker - and it is not dressed up as pending.
func TestAddSingleLoxilbFailureIsUnchanged(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	gw.createStatus = http.StatusBadRequest
	gw.createResult = "no"
	peer := gw.client(t)
	peer.IsAlive = true
	m, svc := addHarness(t, peer)

	err := m.addLoadBalancer(svc)
	if err == nil || errors.Is(err, errPeersPending) {
		t.Fatalf("addLoadBalancer = %v, want the failure itself", err)
	}
	if n := len(m.lbCache[GenKey("llm", "web")].LbServicePairs); n != 0 {
		t.Errorf("%d pairs cached for a rule no peer took", n)
	}
}

// Two loxilb, one refuses: the Service is served and published, the add asks
// to come back, and the next sync sends the rule to that peer only.
func TestAddPartialRefusalRetriesThatPeerOnly(t *testing.T) {
	has := newFakeLoxiLB(t, "")
	lacks := newFakeLoxiLB(t, "")
	lacks.createStatus = http.StatusBadRequest
	lacks.createResult = "jwt_auth_profile: required by this api_key_auth mode and must name a configured profile"
	hasPeer, lacksPeer := has.client(t), lacks.client(t)
	hasPeer.IsAlive, lacksPeer.IsAlive = true, true
	m, svc := addHarness(t, hasPeer, lacksPeer)

	if err := m.addLoadBalancer(svc); !errors.Is(err, errPeersPending) {
		t.Fatalf("addLoadBalancer = %v, want errPeersPending", err)
	}
	if len(ingressOf(t, m)) == 0 {
		t.Error("a Service one peer serves was not published")
	}

	lacks.mu.Lock()
	lacks.createStatus = 0
	lacks.mu.Unlock()

	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("retry = %v, want nil once the peer took the rule", err)
	}
	if h, l := posts(has), posts(lacks); h != 1 || l != 2 {
		t.Errorf("posts: %d to the peer that had the rule, %d to the one that refused; want 1 and 2", h, l)
	}
}

// A peer that is down fails the fan-out without keeping the Service retrying:
// the re-sync when it comes back puts the rule there.
func TestAddLeavesAPeerThatIsDownToItsReturn(t *testing.T) {
	up := newFakeLoxiLB(t, "")
	gone := newFakeLoxiLB(t, "")
	upPeer, gonePeer := up.client(t), gone.client(t)
	upPeer.IsAlive = true
	gone.srv.Close() // connection refused from now on; IsAlive stays false
	m, svc := addHarness(t, upPeer, gonePeer)

	if err := m.addLoadBalancer(svc); err != nil {
		t.Fatalf("addLoadBalancer = %v, want nil with the only failing peer down", err)
	}
	if st := m.lbCache[GenKey("llm", "web")].LbServicePairs; len(st) != 1 {
		t.Fatalf("pairs = %d, want 1", len(st))
	}
}
