package loadbalancer

import (
	"testing"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// fakeProfiles - JWTProfiles with the profiles that exist and the peers each
// one is on.
type fakeProfiles map[string]map[string]bool

func (f fakeProfiles) Programmed(name, host string) (bool, bool) {
	hosts, ok := f[name]
	return ok, ok && hosts[host]
}

func jwtRule() api.LoadBalancerModel {
	return authLoadBalancerModel(api.APIKeyAuthJWT, "kc")
}

// Without JWTAuthProfiles managed - the CRD is not installed - a rule goes as
// it is, and the gateway answers for the profile.
func TestRuleGoesAsItIsWithoutManagedProfiles(t *testing.T) {
	gw := authGateway(t)
	m := &Manager{}

	if err := m.installLB(gw.client(t), jwtRule(), false); err != nil {
		t.Fatalf("installLB: %v", err)
	}
	if posts, _, _ := gw.counts(); posts != 1 {
		t.Errorf("posted %d times, want 1", posts)
	}
}

// With them managed, a rule waits until its profile is on the peer.
func TestRuleWaitsForItsProfile(t *testing.T) {
	cases := []struct {
		name     string
		profiles fakeProfiles
		want     error
	}{
		{"no JWTAuthProfile", fakeProfiles{}, ErrProfileNotFound},
		{"not on this peer yet", fakeProfiles{"kc": {}}, ErrProfileNotProgrammed},
	}
	for _, tt := range cases {
		gw := authGateway(t)
		m := &Manager{}
		m.SetJWTProfiles(tt.profiles)

		err := m.installLB(gw.client(t), jwtRule(), false)
		if !errors.Is(err, tt.want) || !isPeerRefusal(err) {
			t.Errorf("%s: installLB = %v, want %v", tt.name, err, tt.want)
		}
		if posts, _, _ := gw.counts(); posts != 0 {
			t.Errorf("%s: the rule was sent before its profile", tt.name)
		}
	}

	gw := authGateway(t)
	peer := gw.client(t)
	m := &Manager{}
	m.SetJWTProfiles(fakeProfiles{"kc": {peer.Host: true}})
	if err := m.installLB(peer, jwtRule(), false); err != nil {
		t.Errorf("installLB with the profile on the peer: %v", err)
	}
}

// A rule naming no profile does not depend on them.
func TestRuleWithoutAProfileIsNotHeld(t *testing.T) {
	gw := authGateway(t)
	m := &Manager{}
	m.SetJWTProfiles(fakeProfiles{})

	if err := m.installLB(gw.client(t), authLoadBalancerModel(api.APIKeyAuthRequired, ""), false); err != nil {
		t.Errorf("installLB: %v", err)
	}
}

// The profile reached one peer: the rule goes there and waits for the other,
// and goes there too once the profile does.
func TestRuleFollowsItsProfilePeerByPeer(t *testing.T) {
	ready, waiting := authGateway(t), authGateway(t)
	readyPeer, waitingPeer := ready.client(t), waiting.client(t)
	readyPeer.IsAlive, waitingPeer.IsAlive = true, true
	m, recorder := resyncManager(readyPeer, map[string]api.LoadBalancerModel{"svc": jwtRule()})
	m.LoxiClients.AddLoxiClient(waitingPeer)
	profiles := fakeProfiles{"kc": {readyPeer.Host: true}}
	m.SetJWTProfiles(profiles)

	if m.retryPeers(GenKey("llm", "svc")) {
		t.Error("a peer waiting for its profile counted as pending")
	}
	if posts, _, _ := ready.counts(); posts != 1 {
		t.Errorf("the peer with the profile got %d posts, want 1", posts)
	}
	if posts, _, _ := waiting.counts(); posts != 0 {
		t.Errorf("the peer without the profile got %d posts", posts)
	}
	if events := drainEvents(recorder); len(events) != 1 || events[0][:len("Warning "+ReasonProfileNotProgrammed)] != "Warning "+ReasonProfileNotProgrammed {
		t.Errorf("events = %q, want one %s", events, ReasonProfileNotProgrammed)
	}

	profiles["kc"][waitingPeer.Host] = true
	m.retryPeers(GenKey("llm", "svc"))
	if posts, _, _ := waiting.counts(); posts != 1 {
		t.Errorf("the peer that got the profile got %d posts, want 1", posts)
	}
}

func TestQueueServicesUsingProfile(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for name, profile := range map[string]string{"a": "kc", "b": "kc", "c": "other", "d": ""} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm",
			Annotations: map[string]string{jwtAuthProfileAnnotation: profile}}}
		_ = indexer.Add(svc)
	}
	m := &Manager{serviceLister: corelisters.NewServiceLister(indexer), queue: testQueue()}

	m.QueueServicesUsingProfile("kc")

	if m.queue.Len() != 2 {
		t.Errorf("%d queued, want the two naming kc", m.queue.Len())
	}
}

// What a returning peer's rules need goes first.
func TestPeerBackCallsTheHook(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	m, _ := resyncManager(peer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	var called *api.LoxiClient
	m.SetPeerBackHook(func(c *api.LoxiClient) { called = c })

	m.reinstallLoxiLbRules(peer)

	if called != peer {
		t.Error("the peer-back hook was not called with the returning peer")
	}
}
