package loadbalancer

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

var listenerEpoch = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// onListener - a fullproxy Service on 192.168.0.13:8080/tcp, created minutes
// after the epoch, declaring the given policy.
func onListener(name string, minutes int, apiKeyAuth, profile string) *corev1.Service {
	class := addTestClass
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "llm",
			CreationTimestamp: metav1.NewTime(listenerEpoch.Add(time.Duration(minutes) * time.Minute)),
			Annotations:       map[string]string{lbModeAnnotation: "fullproxy"},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &class, LoadBalancerIP: "192.168.0.13",
			Ports: []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}
	if apiKeyAuth != "" {
		svc.Annotations[apiKeyAuthAnnotation] = apiKeyAuth
	}
	if profile != "" {
		svc.Annotations[jwtAuthProfileAnnotation] = profile
	}
	return svc
}

// listenerManager - a Manager whose lister holds services.
func listenerManager(t *testing.T, services ...*corev1.Service) (*Manager, *objectRecorder) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range services {
		if err := indexer.Add(svc); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &objectRecorder{}
	return &Manager{
		networkConfig: &config.NetworkConfig{LoxilbLoadBalancerClass: addTestClass},
		serviceLister: corelisters.NewServiceLister(indexer),
		lbCache:       LbCacheTable{},
		LoxiClients:   api.NewLoxiClientPool(),
		eventRecorder: recorder,
		queue:         testQueue(),
	}, recorder
}

func names(services []*corev1.Service) string {
	var out []string
	for _, svc := range services {
		out = append(out, svc.Name)
	}
	return strings.Join(out, ",")
}

func TestOlderPolicyHoldsTheListener(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, "", "")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo == nil || v.yieldTo.Name != "a" {
		t.Errorf("b yields to %v, want a", v.yieldTo)
	}
	if v := m.judgeListeners(a); v.yieldTo != nil || names(v.outranks) != "b" {
		t.Errorf("a: yieldTo %v, outranks %q; want nil and b", v.yieldTo, names(v.outranks))
	}
}

func TestAgreeingServicesShareTheListener(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, api.APIKeyAuthJWT, "kc")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo != nil {
		t.Errorf("b yields to %s although they agree", v.yieldTo.Name)
	}
	if v := m.judgeListeners(a); len(v.outranks) != 0 {
		t.Errorf("a outranks %q although they agree", names(v.outranks))
	}
}

// As on the gateway: "disabled" strips X-Api-Key and an unset policy does not.
func TestUnsetAndDisabledDisagree(t *testing.T) {
	a := onListener("a", 0, "", "")
	b := onListener("b", 1, api.APIKeyAuthDisabled, "")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo == nil {
		t.Error("an explicit disabled shares a listener with an unset policy")
	}
}

// The gateway holds only fullproxy rules to one policy per listener.
func TestOnlyFullProxyRulesShareAPolicy(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	a.Annotations[lbModeAnnotation] = "default"
	b := onListener("b", 1, "", "")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo != nil {
		t.Error("b yields to a rule that is not fullproxy")
	}
}

// A Service that agrees with the oldest stays, even next to one that does not.
func TestThePolicyOfTheOldestDecides(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, "", "")
	c := onListener("c", 2, api.APIKeyAuthJWT, "kc")
	m, _ := listenerManager(t, a, b, c)

	if v := m.judgeListeners(b); v.yieldTo == nil || v.yieldTo.Name != "a" {
		t.Errorf("b yields to %v, want a", v.yieldTo)
	}
	if v := m.judgeListeners(c); v.yieldTo != nil {
		t.Errorf("c yields to %s although it agrees with the oldest", v.yieldTo.Name)
	}
}

// A generated Service counts as old as its pool, so re-creating the Service
// does not hand the listener over.
func TestPoolAgeDecidesForGeneratedServices(t *testing.T) {
	recreated := onListener("old-pool-inference", 5, api.APIKeyAuthJWT, "kc")
	recreated.Labels = map[string]string{inferencePoolLabel: "old-pool"}
	other := onListener("b", 1, "", "")
	m, _ := listenerManager(t, recreated, other)
	m.SetInferencePoolLister(poolLister(t, &infv1.InferencePool{ObjectMeta: metav1.ObjectMeta{
		Name: "old-pool", Namespace: "llm", CreationTimestamp: metav1.NewTime(listenerEpoch),
	}}))

	if v := m.judgeListeners(recreated); v.yieldTo != nil {
		t.Errorf("the Service of the older pool yields to %s", v.yieldTo.Name)
	}
	if v := m.judgeListeners(other); v.yieldTo == nil {
		t.Error("b does not yield to the Service of the older pool")
	}
}

// A Service that yields takes back what it has on the peers, once, and says
// so once.
func TestYieldingTakesBackItsRulesOnce(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, "", "")
	m, recorder := listenerManager(t, a, b)
	m.LoxiClients.AddLoxiClient(peer)
	m.lbCache[GenKey("llm", "b")] = &LbCacheEntry{LbServicePairs: map[string]*LbServicePairEntry{
		"pair": {ExternalIP: "192.168.0.13", Port: 8080, Protocol: "tcp",
			LbModelList: []api.LoadBalancerModel{plainModel()}, peers: map[string]peerState{peer.Host: {phase: peerInstalled}}},
	}}

	for i := 0; i < 2; i++ {
		yields, err := m.settleListeners(b, GenKey("llm", "b"))
		if !yields || err != nil {
			t.Fatalf("settleListeners = %v, %v; want it to yield", yields, err)
		}
	}

	gw.mu.Lock()
	deletes := gw.deletes
	gw.mu.Unlock()
	if deletes != 1 {
		t.Errorf("deleted %d times, want once", deletes)
	}
	if st := m.lbCache[GenKey("llm", "b")].LbServicePairs["pair"].peers[peer.Host]; st.phase != peerHeld {
		t.Errorf("peer phase %d, want held", st.phase)
	}
	if len(recorder.events) != 1 || recorder.events[0].reason != ReasonListenerAuthConflict ||
		!strings.Contains(recorder.events[0].message, "llm/a") {
		t.Errorf("events = %+v, want one %s naming llm/a", recorder.events, ReasonListenerAuthConflict)
	}
}

// The older Service queues the one it outranks until that one has yielded.
func TestWinnerQueuesTheServiceItOutranks(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, "", "")
	m, _ := listenerManager(t, a, b)

	if yields, _ := m.settleListeners(a, GenKey("llm", "a")); yields {
		t.Fatal("the older Service yields")
	}
	if m.queue.Len() != 1 {
		t.Fatalf("%d queued, want b", m.queue.Len())
	}
	item, _ := m.queue.Get()
	m.queue.Done(item)
	m.queue.Forget(item)

	if yields, _ := m.settleListeners(b, GenKey("llm", "b")); !yields {
		t.Fatal("b does not yield")
	}
	m.settleListeners(a, GenKey("llm", "a"))
	if m.queue.Len() != 0 {
		t.Error("b was queued again although it already yields")
	}
}

// When the Service it yielded to goes, or comes to agree, a yielding Service is
// judged again.
func TestYielderIsQueuedWhenItsRivalGoesOrAgrees(t *testing.T) {
	a := onListener("a", 0, api.APIKeyAuthJWT, "kc")
	b := onListener("b", 1, "", "")

	m, _ := listenerManager(t, a, b)
	m.settleListeners(b, GenKey("llm", "b"))
	m.releaseYielders(GenKey("llm", "a"))
	if m.queue.Len() != 1 {
		t.Errorf("%d queued after the rival went, want b", m.queue.Len())
	}

	agreeing := onListener("a", 0, "", "")
	m2, _ := listenerManager(t, agreeing, b)
	m2.yielding = map[string]string{GenKey("llm", "b"): GenKey("llm", "a")}
	m2.settleListeners(agreeing, GenKey("llm", "a"))
	if m2.queue.Len() != 1 {
		t.Errorf("%d queued after the rival came to agree, want b", m2.queue.Len())
	}
}

// End to end: a Service that disagrees with an older one on its listener is
// not sent anywhere; once that one is gone, it is installed.
func TestAddHoldsAServiceThatDisagreesOnItsListener(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	peer.IsAlive = true
	m, svc := addHarness(t, peer)
	svc.Annotations = map[string]string{lbModeAnnotation: "fullproxy"}
	svc.Spec.LoadBalancerIP = "192.168.0.13"
	svc.CreationTimestamp = metav1.NewTime(listenerEpoch.Add(time.Minute))
	older := onListener("older", 0, api.APIKeyAuthJWT, "kc")

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_ = indexer.Add(svc)
	_ = indexer.Add(older)
	m.serviceLister = corelisters.NewServiceLister(indexer)
	if _, err := m.kubeClient.CoreV1().Services("llm").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update service: %v", err)
	}

	if err := m.addLoadBalancer(svc); err != nil {
		t.Fatalf("addLoadBalancer = %v", err)
	}
	if n := posts(gw); n != 0 {
		t.Fatalf("POSTed %d rules for a Service that yields its listener", n)
	}

	_ = indexer.Delete(older)
	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("addLoadBalancer after the older Service went = %v", err)
	}
	if n := posts(gw); n != 1 {
		t.Errorf("POSTed %d rules once the listener was free, want 1", n)
	}
}

// poolOnListener - onListener for the Service of a pool serving model.
func poolOnListener(name string, minutes int, model string) *corev1.Service {
	svc := onListener(name, minutes, "", "")
	svc.Labels = map[string]string{inferencePoolLabel: name}
	if model != "" {
		svc.Annotations[modelNameAnnotation] = model
	}
	return svc
}

// Pools on one listener are told apart by model. Of those serving the same
// one, the oldest is served - even when it is not the oldest on the listener.
func TestPoolsServingTheSameModelDoNotShareAListener(t *testing.T) {
	a := poolOnListener("a", 0, "qwen")
	b := poolOnListener("b", 1, "llama")
	c := poolOnListener("c", 2, "llama")
	m, _ := listenerManager(t, a, b, c)

	if v := m.judgeListeners(b); v.yieldTo != nil {
		t.Errorf("b yields to %s although no older pool serves llama", v.yieldTo.Name)
	}
	if v := m.judgeListeners(c); v.yieldTo == nil || v.yieldTo.Name != "b" || v.reason != ReasonListenerRouteConflict {
		t.Errorf("c: yieldTo %v (%s), want b for %s", v.yieldTo, v.reason, ReasonListenerRouteConflict)
	}
	if v := m.judgeListeners(b); names(v.outranks) != "c" {
		t.Errorf("b outranks %q, want c", names(v.outranks))
	}
	if v := m.judgeListeners(a); len(v.outranks) != 0 {
		t.Errorf("a outranks %q although no other pool serves qwen", names(v.outranks))
	}
}

// Two pools naming no model are both the listener's catch-all: one rule.
func TestPoolsWithoutAModelConflict(t *testing.T) {
	a := poolOnListener("a", 0, "")
	b := poolOnListener("b", 1, "")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo == nil || v.reason != ReasonListenerRouteConflict {
		t.Errorf("b: yieldTo %v (%s), want a for %s", v.yieldTo, v.reason, ReasonListenerRouteConflict)
	}
}

// Services not generated from a pool are judged on their policy alone, as
// before.
func TestOtherServicesAreNotJudgedOnModel(t *testing.T) {
	a := onListener("a", 0, "", "")
	b := onListener("b", 1, "", "")
	m, _ := listenerManager(t, a, b)

	if v := m.judgeListeners(b); v.yieldTo != nil {
		t.Errorf("b yields to %s on model", v.yieldTo.Name)
	}
}

// A pool that yields says why, naming the older pool and the model.
func TestRouteConflictIsReported(t *testing.T) {
	a := poolOnListener("a", 0, "qwen")
	b := poolOnListener("b", 1, "qwen")
	m, recorder := listenerManager(t, a, b)

	if yields, err := m.settleListeners(b, GenKey("llm", "b")); !yields || err != nil {
		t.Fatalf("settleListeners = %v, %v; want it to yield", yields, err)
	}
	if len(recorder.events) != 1 || recorder.events[0].reason != ReasonListenerRouteConflict ||
		!strings.Contains(recorder.events[0].message, "llm/a") || !strings.Contains(recorder.events[0].message, "qwen") {
		t.Errorf("events = %+v, want one %s naming llm/a and qwen", recorder.events, ReasonListenerRouteConflict)
	}
}
