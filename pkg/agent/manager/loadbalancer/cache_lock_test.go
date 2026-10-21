package loadbalancer

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// emptyServiceLister - a lister with no Services, so every sync takes the
// delete path.
func emptyServiceLister() corelisters.ServiceLister {
	return corelisters.NewServiceLister(cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}))
}

// The re-sync for a peer that comes back up runs on the lifecycle goroutine
// while the worker adds and deletes cache entries. Run under -race; without
// the cache lock the runtime also ends the process with "concurrent map
// iteration and map write".
func TestReinstallAndWorkerShareTheCache(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	m := deleteManager(peer)
	m.serviceLister = emptyServiceLister()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.reinstallLoxiLbRules(peer)
			}
		}
	}()

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("svc%d", i)
		m.lbCacheMu.Lock()
		m.lbCache[GenKey("llm", name)] = &LbCacheEntry{
			LbServicePairs: map[string]*LbServicePairEntry{
				"pair": {LbModelList: []api.LoadBalancerModel{plainModel()}},
			},
		}
		m.lbCacheMu.Unlock()

		if err := m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: name}); err != nil {
			t.Errorf("sync %s: %v", name, err)
		}
	}

	close(stop)
	wg.Wait()
}

// A peer coming back no longer posts anything from the lifecycle goroutine:
// it forgets that peer's standing and queues every Service, and the worker
// puts the rules back. That is also why a Service deleted meanwhile cannot be
// put back after its delete - the worker does both, one after the other.
func TestPeerBackLeavesInstallingToTheWorker(t *testing.T) {
	back := newFakeLoxiLB(t, "")
	other := newFakeLoxiLB(t, "")
	backPeer, otherPeer := back.client(t), other.client(t)
	m := deleteManager(backPeer, otherPeer)
	m.lbCache[GenKey("llm", "other")] = &LbCacheEntry{
		LbServicePairs: map[string]*LbServicePairEntry{"pair": {LbModelList: []api.LoadBalancerModel{plainModel()}}},
	}
	for _, entry := range m.lbCache {
		for _, sp := range entry.LbServicePairs {
			sp.peers = map[string]peerState{
				backPeer.Host:  {phase: peerInstalled},
				otherPeer.Host: {phase: peerInstalled},
			}
		}
	}

	m.reinstallLoxiLbRules(backPeer)

	back.mu.Lock()
	posted := len(back.bodies)
	back.mu.Unlock()
	if posted != 0 {
		t.Errorf("the lifecycle goroutine posted %d rules itself", posted)
	}
	for key, entry := range m.lbCache {
		for _, sp := range entry.LbServicePairs {
			if _, ok := sp.peers[backPeer.Host]; ok {
				t.Errorf("%s: the returning peer's standing was kept", key)
			}
			if sp.peers[otherPeer.Host].phase != peerInstalled {
				t.Errorf("%s: another peer's standing was touched", key)
			}
		}
	}
	if n := m.queue.Len(); n != 2 {
		t.Errorf("%d Services queued, want both", n)
	}
}

// AddLoxiInstAddr holds m.mtx, which the worker takes too, and the lifecycle
// loop that reads the signal can be waiting on the cache lock the worker
// holds. A send that waited for the loop closed that circle; it must not wait.
func TestInstAddrSignalNeverWaitsForTheLoop(t *testing.T) {
	m := &Manager{
		instAddrApplyCh: make(chan struct{}, 1),
		loxiInstAddrMap: map[string]net.IP{},
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			_ = m.AddLoxiInstAddr(fmt.Sprintf("inst%d", i), net.ParseIP("10.0.0.1"))
		}
		_ = m.DeleteLoxiInstAddr("inst0")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AddLoxiInstAddr is waiting for a reader")
	}
	if len(m.instAddrApplyCh) != 1 {
		t.Errorf("%d signals pending, want the changes folded into one", len(m.instAddrApplyCh))
	}
}
