package loadbalancer

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// resyncManager - a Manager holding one cached rule per entry of models,
// keyed by Service name in namespace llm, with those Services in its lister.
func resyncManager(peer *api.LoxiClient, models map[string]api.LoadBalancerModel) (*Manager, *record.FakeRecorder) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	table := LbCacheTable{}
	for name, model := range models {
		_ = indexer.Add(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"}})
		table[GenKey("llm", name)] = &LbCacheEntry{
			LbServicePairs: map[string]*LbServicePairEntry{
				"pair": {LbModelList: []api.LoadBalancerModel{model}},
			},
		}
	}

	recorder := record.NewFakeRecorder(16)
	pool := api.NewLoxiClientPool()
	pool.AddLoxiClient(peer)
	return &Manager{
		LoxiClients:   pool,
		lbCache:       table,
		serviceLister: corelisters.NewServiceLister(indexer),
		eventRecorder: recorder,
		queue:         testQueue(),
	}, recorder
}

// peerBack - what follows a peer coming back up: its standing is forgotten
// and the worker syncs every Service, which puts the rules back through
// retryPeers. Reports whether a rule is still pending somewhere.
func peerBack(m *Manager, peer *api.LoxiClient) bool {
	peer.IsAlive = true
	m.reinstallLoxiLbRules(peer)
	return syncAll(m)
}

// syncAll - the retry part of one worker sync of every cached Service.
func syncAll(m *Manager) bool {
	m.lbCacheMu.Lock()
	defer m.lbCacheMu.Unlock()

	pending := false
	for cacheKey := range m.lbCache {
		if m.retryPeers(cacheKey) {
			pending = true
		}
	}
	return pending
}

func plainModel() api.LoadBalancerModel {
	return api.LoadBalancerModel{Service: api.LoadBalancerService{
		ExternalIP: "10.0.0.1", Port: 8090, Protocol: "tcp", Mode: api.LBModeFullProxy,
	}}
}

// A peer that comes back on a build without Endpoint Picker support refuses
// the FailClose rule and takes the FailOpen one without the picker. Both have
// to show on their Service, as they do when the rule is first programmed.
func TestPeerBackReportsWhatAPeerWithoutThePickerDoes(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // no features
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failclose": eppLoadBalancerModel(api.EppFailureModeFailClose),
		"failopen":  eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":     plainModel(),
	})

	if peerBack(m, peer) {
		t.Error("a refusal kube-loxilb makes itself was counted as pending")
	}

	events := drainEvents(recorder)
	var refused, downgraded int
	for _, e := range events {
		if !strings.HasPrefix(e, "Warning "+ReasonEndpointPickerUnsupported+" ") {
			t.Errorf("unexpected event %q", e)
			continue
		}
		switch {
		case strings.Contains(e, "FailClose forbids"):
			refused++
		case strings.Contains(e, "FailOpen rule") && strings.Contains(e, peer.Host) && strings.Contains(e, "10.43.12.7:9002"):
			downgraded++
		default:
			t.Errorf("event with an unexpected message: %q", e)
		}
	}
	if refused != 1 || downgraded != 1 {
		t.Errorf("got %d refusal and %d downgrade warnings, want one each; events: %q", refused, downgraded, events)
	}

	// The held peer is looked at again on the next sync, without repeating
	// the warning.
	syncAll(m)
	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("the next sync repeated warnings: %q", events)
	}
}

// Back on a build that has the picker, everything goes on and nothing is said.
func TestPeerBackQuietOnAPeerWithThePicker(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.features = []string{api.FeatureEndpointPicker}
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failclose": eppLoadBalancerModel(api.EppFailureModeFailClose),
		"failopen":  eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":     plainModel(),
	})

	if peerBack(m, peer) {
		t.Error("rules still pending on a peer that took them all")
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events on a peer that supports the picker: %q", events)
	}
}

// A gateway that comes back on a build without the credential features is not
// sent the rule, and the Service says why.
func TestPeerBackReportsCredentialPolicyRefusal(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // no features
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"jwt": authLoadBalancerModel(api.APIKeyAuthJWT, "kc"),
	})

	peerBack(m, peer)

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonAuthNotSupported+" ") {
		t.Errorf("events = %q, want one %s warning", events, ReasonAuthNotSupported)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 0 {
		t.Errorf("a refused rule was still POSTed:\n%s", gw.bodies[0])
	}
}

// A peer that answers 4xx - here a JWT auth profile it does not have yet - is
// reported once on the Service and tried again on later syncs until it takes
// the rule. Nothing restarts kube-loxilb any more; the re-sync used to retry
// such a rule five times and then end the process.
func TestPeerBackRetriesARejectedRule(t *testing.T) {
	const reason = "jwt_auth_profile: required by this api_key_auth mode and must name a configured profile"
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.features = []string{api.FeatureAPIKeyAuth, api.FeatureJWTAuthProfile}
	gw.createStatus = http.StatusBadRequest
	gw.createResult = reason
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"jwt": authLoadBalancerModel(api.APIKeyAuthJWT, "kc"),
	})

	if !peerBack(m, peer) {
		t.Error("a rule the peer refused is not pending")
	}
	events := drainEvents(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonLoxiLBRejected+" ") ||
		!strings.Contains(events[0], peer.Host) || !strings.Contains(events[0], reason) {
		t.Errorf("events = %q, want one %s warning naming the peer and loxilb's reason", events, ReasonLoxiLBRejected)
	}

	if !syncAll(m) {
		t.Error("still refused, but no longer pending")
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("the same refusal was reported again: %q", events)
	}

	// The profile arrives on the peer.
	gw.mu.Lock()
	gw.createStatus = 0
	gw.mu.Unlock()
	if syncAll(m) {
		t.Error("still pending after the peer took the rule")
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 3 {
		t.Errorf("POSTed %d times, want 3: one per sync until the peer takes it", len(gw.bodies))
	}
}

// The other refusals reach their Service the same way.
func TestPeerBackReportsPlainPeerRefusal(t *testing.T) {
	plain := newFakeLoxiLB(t, "")
	peer := plain.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failopen": eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":    plainModel(),
	})

	peerBack(m, peer)

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonInferenceGatewayRequired+" ") {
		t.Errorf("events = %q, want one %s warning", events, ReasonInferenceGatewayRequired)
	}
}
