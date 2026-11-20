package loadbalancer

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// sharedListenerHarness - two pools on the Gateway listener 192.168.0.13:8080,
// a-inference the older, both installed without a credential policy on a
// gateway that holds one policy per listener.
func sharedListenerHarness(t *testing.T) (*Manager, *fakeLoxiLB, cache.Indexer, *corev1.Service, *corev1.Service) {
	t.Helper()
	gw := authGateway(t)
	gw.onePolicyPerListener = true
	peer := gw.client(t)
	peer.IsAlive = true
	m, _ := addHarness(t, peer)

	a := poolService("a", "qwen", "192.168.0.13", 30001)
	a.CreationTimestamp = metav1.NewTime(listenerEpoch)
	b := poolService("b", "llama", "192.168.0.13", 30002)
	b.CreationTimestamp = metav1.NewTime(listenerEpoch.Add(time.Minute))
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range []*corev1.Service{a, b} {
		if _, err := m.kubeClient.CoreV1().Services("llm").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", svc.Name, err)
		}
		_ = indexer.Add(svc)
	}
	m.serviceLister = corelisters.NewServiceLister(indexer)

	for _, svc := range []*corev1.Service{a, b} {
		if err := m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: svc.Name}); err != nil {
			t.Fatalf("sync %s: %v", svc.Name, err)
		}
	}
	if got := policiesOn(gw); got != "llama=<nil>,qwen=<nil>" {
		t.Fatalf("rules on the gateway: %s, want both pools without a policy", got)
	}
	drainEvents(m.eventRecorder.(*record.FakeRecorder))
	return m, gw, indexer, a, b
}

// requiring - the Service with api_key_auth: required, as the InferencePool
// manager writes it.
func requiring(t *testing.T, indexer cache.Indexer, svc *corev1.Service) {
	t.Helper()
	svc = svc.DeepCopy()
	svc.Annotations[apiKeyAuthAnnotation] = api.APIKeyAuthRequired
	if err := indexer.Update(svc); err != nil {
		t.Fatal(err)
	}
}

// policiesOn - the rules the gateway holds, as model=api_key_auth.
func policiesOn(gw *fakeLoxiLB) string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	var out []string
	for _, rule := range gw.rules {
		policy, ok := rule["api_key_auth"]
		if !ok {
			policy = "<nil>"
		}
		out = append(out, rule["model_name"].(string)+"="+policy.(string))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// rejections - the LoxiLBRejected events recorded since the last call.
func rejections(m *Manager) []string {
	var out []string
	for _, e := range drainEvents(m.eventRecorder.(*record.FakeRecorder)) {
		if strings.Contains(e, ReasonLoxiLBRejected) {
			out = append(out, e)
		}
	}
	return out
}

// Both pools on a listener take a credential policy, the InferencePool
// manager updating their Services in turn. The gateway refuses the older
// pool's rule while the younger one's still declares none; the older pool is
// sent again as soon as the younger one has had its sync and taken its rule
// off, not after the backoff, and the refusal is not reported.
func TestAPolicyChangeOnASharedListenerIsSentAgainOnceTheOthersHaveHadTheirSync(t *testing.T) {
	m, gw, indexer, a, b := sharedListenerHarness(t)
	// next - sync the one Service queued, which is due now: the worker
	// would otherwise wait for the backoff.
	next := func(what string) {
		t.Helper()
		if n := m.queue.Len(); n != 1 {
			t.Fatalf("%s: %d Services due, want 1", what, n)
		}
		m.processNextWorkItem()
	}

	requiring(t, indexer, a)
	m.queue.Add(LbCacheKey{Namespace: "llm", Name: a.Name})
	next("a's Service changed")
	if got := policiesOn(gw); got != "llama=<nil>" {
		t.Errorf("after a's sync the gateway holds %s, want b's rule alone", got)
	}
	if e := rejections(m); len(e) != 0 {
		t.Errorf("the refusal of a's rule was reported: %v", e)
	}

	next("b queued, outranked by a") // b, not changed yet, yields and takes its rule off
	next("a waits for b")
	if got := policiesOn(gw); got != "qwen=required" {
		t.Errorf("after b yielded the gateway holds %s, want a's rule with its new policy", got)
	}

	requiring(t, indexer, b)
	m.queue.Add(LbCacheKey{Namespace: "llm", Name: b.Name})
	next("b's Service changed")
	if got := policiesOn(gw); got != "llama=required,qwen=required" {
		t.Errorf("the gateway holds %s, want both pools with the new policy", got)
	}
	if e := rejections(m); len(e) != 0 {
		t.Errorf("rejections reported: %v", e)
	}
	if len(m.listenerWaits) != 0 {
		t.Errorf("waits left: %v", m.listenerWaits)
	}
}

// A Service waited for that has had its sync and still holds the listener
// with the old policy is not about to change: the refusal is reported then.
func TestARefusalIsReportedOnceTheServiceWaitedForHasHadItsSync(t *testing.T) {
	m, _, indexer, a, _ := sharedListenerHarness(t)
	key := LbCacheKey{Namespace: "llm", Name: a.Name}

	requiring(t, indexer, a)
	if err := m.syncLoadBalancer(key); !errors.Is(err, errListenerWait) {
		t.Fatalf("first refusal: %v, want errListenerWait", err)
	}
	if e := rejections(m); len(e) != 0 {
		t.Errorf("the first refusal was reported: %v", e)
	}

	m.wakeListenerWaiters("llm/b-inference") // b had a sync that left its rule as it was
	err := m.syncLoadBalancer(key)
	if err == nil || errors.Is(err, errListenerWait) {
		t.Fatalf("second refusal: %v, want an error that does not wait", err)
	}
	if e := rejections(m); len(e) != 1 || !strings.Contains(e[0], "api_key_auth and jwt_auth_profile") {
		t.Errorf("rejections = %v, want the gateway's refusal reported once", e)
	}
}

// A refusal on a listener no other rule declares another policy on is not
// waited out.
func TestARefusalWithNoOtherPolicyOnTheListenerIsReported(t *testing.T) {
	m, gw, indexer, a, _ := sharedListenerHarness(t)
	gw.mu.Lock()
	gw.createStatus, gw.createResult = 400, "model_name is not valid"
	gw.mu.Unlock()

	svc := a.DeepCopy()
	svc.Annotations[modelNameAnnotation] = "qwen2"
	if err := indexer.Update(svc); err != nil {
		t.Fatal(err)
	}
	err := m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: a.Name})
	if err == nil || errors.Is(err, errListenerWait) {
		t.Fatalf("sync = %v, want an error that does not wait", err)
	}
	if e := rejections(m); len(e) != 1 {
		t.Errorf("rejections = %v, want one", e)
	}
	if len(m.listenerWaits) != 0 {
		t.Errorf("waits recorded: %v", m.listenerWaits)
	}
}

// A sync that leaves the Service served ends its wait, however it got there -
// a retry of the peers that refused it does not go through the install - so
// the Services it waited for no longer queue it on each of their syncs.
func TestAServedServiceWaitsForNothing(t *testing.T) {
	m, gw, indexer, a, b := sharedListenerHarness(t)
	// The Services as the informer hands them over now, with the status the
	// syncs wrote: a sync of either changes nothing.
	for _, svc := range []*corev1.Service{a, b} {
		cur, err := m.kubeClient.CoreV1().Services("llm").Get(context.Background(), svc.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := indexer.Update(cur); err != nil {
			t.Fatal(err)
		}
	}
	gw.mu.Lock()
	gw.ruleStatus = &api.LoadBalancerStatusModel{OperatingStatus: api.OperatingStatusOnline}
	gw.mu.Unlock()
	m.keepListenerWait("llm/"+a.Name, listenerWait{"llm/" + b.Name: false})

	before := posts(gw)
	if err := m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: a.Name}); err != nil {
		t.Fatalf("sync a: %v", err)
	}
	if n := posts(gw) - before; n != 0 {
		t.Fatalf("a's sync sent %d rules, want a sync that changes nothing", n)
	}
	if len(m.listenerWaits) != 0 {
		t.Errorf("waits left after a was synced and served: %v", m.listenerWaits)
	}
	if err := m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: b.Name}); err != nil {
		t.Fatalf("sync b: %v", err)
	}
	if n := m.queue.Len(); n != 0 {
		t.Errorf("%d Services queued by b's sync, want none", n)
	}
}
