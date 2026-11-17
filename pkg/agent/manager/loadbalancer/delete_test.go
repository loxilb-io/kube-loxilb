package loadbalancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// deleteManager - a Manager holding one cached rule for the Service llm/vllm,
// fanned out to peers in that order.
func deleteManager(peers ...*api.LoxiClient) *Manager {
	pool := api.NewLoxiClientPool()
	for _, peer := range peers {
		pool.AddLoxiClient(peer)
	}

	return &Manager{
		LoxiClients:   pool,
		networkConfig: &config.NetworkConfig{},
		queue:         testQueue(),
		lbCache: LbCacheTable{
			GenKey("llm", "vllm"): &LbCacheEntry{
				LbServicePairs: map[string]*LbServicePairEntry{
					"pair": {LbModelList: []api.LoadBalancerModel{plainModel()}},
				},
			},
		},
	}
}

// deleteGoroutines - goroutines still inside a deleteLoadBalancer fan-out.
func deleteGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*Manager).deleteLoadBalancer.func")
}

// A peer without the rule - one that refused it when it was added - answers
// 404. That is the state the delete asks for, so the Service's entry goes.
// It used to fail the delete, and every retry failed the same way.
func TestDeleteLoadBalancerTreatsMissingRuleAsDeleted(t *testing.T) {
	has := newFakeLoxiLB(t, "")
	refused := newFakeLoxiLB(t, "")
	refused.deleteStatus = http.StatusNotFound
	refused.deleteResult = "lbrule not-exists error"
	m := deleteManager(refused.client(t), has.client(t))

	if err := m.deleteLoadBalancer("llm", "vllm", true); err != nil {
		t.Fatalf("deleteLoadBalancer = %v, want nil", err)
	}
	if _, ok := m.lbCache[GenKey("llm", "vllm")]; ok {
		t.Error("the cache entry outlived a delete every peer settled")
	}
}

// With appended endpoints the delete is a detach POST, and a missing rule
// answers 404 there too.
func TestDeleteLoadBalancerDetachTreatsMissingRuleAsDeleted(t *testing.T) {
	refused := newFakeLoxiLB(t, "")
	refused.createStatus = http.StatusNotFound
	refused.createResult = "lbrule not-exists error"
	m := deleteManager(refused.client(t))
	m.networkConfig.AppendEPs = true

	if err := m.deleteLoadBalancer("llm", "vllm", true); err != nil {
		t.Fatalf("deleteLoadBalancer = %v, want nil", err)
	}
}

// The first attempt fails on one peer after the other has already deleted the
// rule; the retry then gets 404 from that other peer. It must still finish
// once the failing peer answers.
func TestDeleteLoadBalancerFinishesAfterAFailedAttempt(t *testing.T) {
	flaky := newFakeLoxiLB(t, "")
	flaky.deleteStatus = http.StatusInternalServerError
	flaky.deleteResult = "datapath busy"
	fine := newFakeLoxiLB(t, "")
	m := deleteManager(flaky.client(t), fine.client(t))

	if err := m.deleteLoadBalancer("llm", "vllm", true); err == nil {
		t.Fatal("a 500 was taken as deleted")
	}
	if _, ok := m.lbCache[GenKey("llm", "vllm")]; !ok {
		t.Fatal("the entry went although a peer still has the rule")
	}

	flaky.mu.Lock()
	flaky.deleteStatus = 0
	flaky.mu.Unlock()
	fine.mu.Lock()
	fine.deleteStatus = http.StatusNotFound
	fine.deleteResult = "lbrule not-exists error"
	fine.mu.Unlock()

	if err := m.deleteLoadBalancer("llm", "vllm", true); err != nil {
		t.Fatalf("retry = %v, want nil", err)
	}
	if _, ok := m.lbCache[GenKey("llm", "vllm")]; ok {
		t.Error("the entry outlived the retry")
	}
}

// A failure on the first peer read used to end the read loop, leaving the
// goroutine for every later peer blocked on its send for good - one more per
// retry, and a failing delete is retried forever.
func TestDeleteLoadBalancerReadsEveryAnswer(t *testing.T) {
	failing := newFakeLoxiLB(t, "")
	failing.deleteStatus = http.StatusInternalServerError
	failing.deleteResult = "datapath busy"
	fine := newFakeLoxiLB(t, "")
	m := deleteManager(failing.client(t), fine.client(t))

	for i := 0; i < 5; i++ {
		if err := m.deleteLoadBalancer("llm", "vllm", true); err == nil {
			t.Fatal("a 500 was taken as deleted")
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for deleteGoroutines() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := deleteGoroutines(); n != 0 {
		t.Errorf("%d delete goroutines still running after the calls returned", n)
	}

	fine.mu.Lock()
	defer fine.mu.Unlock()
	if fine.deletes != 5 {
		t.Errorf("the second peer was asked %d times, want 5", fine.deletes)
	}
}

// A peer that accepts the connection and never answers must not hold the
// worker: the REST client has no timeout of its own.
func TestDeleteLoadBalancerBoundsAHungPeer(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/netlox/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&api.VersionModel{Version: "test"})
			return
		}
		<-release
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) }) // before Close, which waits for handlers

	old := deleteRequestTimeout
	deleteRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { deleteRequestTimeout = old })

	m := deleteManager(clientFor(t, hung))
	done := make(chan error, 1)
	go func() { done <- m.deleteLoadBalancer("llm", "vllm", true) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a peer that never answered was taken as deleted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deleteLoadBalancer is still waiting on a peer that never answers")
	}
}
