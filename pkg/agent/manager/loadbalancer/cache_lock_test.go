package loadbalancer

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A Service the worker deletes while its rule is being put back on a peer
// must not end up back on that peer: the delete has to wait for the re-sync
// of that Service, not slip in before its POST is answered.
func TestReinstallNeverPutsBackADeletedService(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	postStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePost := func() { releaseOnce.Do(func() { close(release) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/netlox/v1/version":
			_ = json.NewEncoder(w).Encode(&api.VersionModel{Version: "test"})
		case r.Method == http.MethodPost:
			record("post")
			postStarted <- struct{}{}
			<-release
			record("post-done")
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		case r.Method == http.MethodDelete:
			record("delete")
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(releasePost) // before Close, which waits for handlers

	peer := clientFor(t, srv)
	m := deleteManager(peer)
	m.serviceLister = emptyServiceLister()

	reinstalled := make(chan struct{})
	go func() {
		m.reinstallLoxiLbRules(peer)
		close(reinstalled)
	}()
	<-postStarted

	synced := make(chan error, 1)
	go func() { synced <- m.syncLoadBalancer(LbCacheKey{Namespace: "llm", Name: "vllm"}) }()
	time.Sleep(100 * time.Millisecond) // room for the delete to overtake, if it can
	releasePost()

	<-reinstalled
	if err := <-synced; err != nil {
		t.Fatalf("sync: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ","); got != "post,post-done,delete" {
		t.Errorf("requests reached the peer as %s, want post,post-done,delete", got)
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
