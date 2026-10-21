package loadbalancer

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func TestPeerStateOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want peerPhase
	}{
		{"accepted", nil, peerInstalled},
		{"refused before sending", errors.Wrap(ErrAuthUnsupported, "loxilb-lb(10.0.0.11)"), peerHeld},
		{"refused by the peer", &api.APIError{StatusCode: http.StatusBadRequest, Message: "jwt_auth_profile: required"}, peerPending},
		{"failed on the peer", &api.APIError{StatusCode: http.StatusInternalServerError, Message: "busy"}, peerPending},
		{"no answer", errors.New("connection refused"), peerPending},
	}
	for _, tt := range cases {
		if got := peerStateOf(tt.err); got.phase != tt.want {
			t.Errorf("%s: phase %d, want %d", tt.name, got.phase, tt.want)
		}
	}
}

// The fan-out runs in parallel, but each answer has to land on its own peer.
func TestInstallOnPeersAnswersInClientOrder(t *testing.T) {
	ok := newFakeLoxiLB(t, "")
	refusing := newFakeLoxiLB(t, "")
	refusing.createStatus = http.StatusBadRequest
	refusing.createResult = "no"
	clients := []*api.LoxiClient{ok.client(t), refusing.client(t)}

	errs := (&Manager{}).installOnPeers(clients, plainModel(), false)
	if errs[0] != nil || errs[1] == nil {
		t.Errorf("errs = %v, want nil then a refusal", errs)
	}
	states := peerStatesOf(clients, errs)
	if states[clients[0].Host].phase != peerInstalled || states[clients[1].Host].phase != peerPending {
		t.Errorf("states = %+v", states)
	}
}

// A peer that is down is left to the re-sync on its return: counting it would
// keep every Service retrying for as long as it is down.
func TestPeersPendingLeavesOutPeersThatAreDown(t *testing.T) {
	up := &api.LoxiClient{Host: "10.0.0.11", IsAlive: true}
	down := &api.LoxiClient{Host: "10.0.0.12"}
	clients := []*api.LoxiClient{up, down}
	pending := peerState{phase: peerPending, err: errors.New("x")}

	if peersPending(clients, map[string]peerState{up.Host: {phase: peerInstalled}, down.Host: pending}) {
		t.Error("a peer that is down kept the Service pending")
	}
	if !peersPending(clients, map[string]peerState{up.Host: pending, down.Host: pending}) {
		t.Error("a peer that is up and lacks the rule did not keep it pending")
	}
	if peersPending(clients, map[string]peerState{up.Host: {phase: peerHeld, err: ErrAuthUnsupported}}) {
		t.Error("a peer kept from the rule on purpose counted as pending")
	}
}

// In the steady state a sync asks nothing of the peers - the case of a single
// loxilb, which behaves as before.
func TestRetryPeersLeavesInstalledPeersAlone(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	peer.IsAlive = true
	m, _ := resyncManager(peer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	m.lbCache[GenKey("llm", "svc")].LbServicePairs["pair"].peers = map[string]peerState{peer.Host: {phase: peerInstalled}}

	if m.retryPeers(GenKey("llm", "svc")) {
		t.Error("an installed rule reads as pending")
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 0 {
		t.Errorf("a peer that has the rule was sent it again %d times", len(gw.bodies))
	}
}

// One peer took the rule and one refused it: the retry goes to that one only.
func TestRetryPeersRetriesOnlyThePeerWithoutTheRule(t *testing.T) {
	has := newFakeLoxiLB(t, "")
	lacks := newFakeLoxiLB(t, "")
	hasPeer, lacksPeer := has.client(t), lacks.client(t)
	hasPeer.IsAlive, lacksPeer.IsAlive = true, true
	m, _ := resyncManager(hasPeer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	m.LoxiClients.AddLoxiClient(lacksPeer)
	m.lbCache[GenKey("llm", "svc")].LbServicePairs["pair"].peers = map[string]peerState{
		hasPeer.Host:   {phase: peerInstalled},
		lacksPeer.Host: {phase: peerPending, err: &api.APIError{StatusCode: http.StatusBadRequest, Message: "not yet"}},
	}

	if m.retryPeers(GenKey("llm", "svc")) {
		t.Error("still pending after the peer took the rule")
	}
	has.mu.Lock()
	hasPosts := len(has.bodies)
	has.mu.Unlock()
	lacks.mu.Lock()
	lacksPosts := len(lacks.bodies)
	lacks.mu.Unlock()
	if hasPosts != 0 || lacksPosts != 1 {
		t.Errorf("posts: %d to the peer with the rule, %d to the one without; want 0 and 1", hasPosts, lacksPosts)
	}
	if st := m.lbCache[GenKey("llm", "svc")].LbServicePairs["pair"].peers[lacksPeer.Host]; st.phase != peerInstalled {
		t.Errorf("the peer that took the rule is recorded as %d", st.phase)
	}
}

// A peer that is down is not tried; the re-sync on its return covers it.
func TestRetryPeersSkipsPeersThatAreDown(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t) // IsAlive false
	m, _ := resyncManager(peer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	m.lbCache[GenKey("llm", "svc")].LbServicePairs["pair"].peers = map[string]peerState{
		peer.Host: {phase: peerPending, err: errors.New("connection refused")},
	}

	if m.retryPeers(GenKey("llm", "svc")) {
		t.Error("a peer that is down kept the Service pending")
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 0 {
		t.Error("a peer that is down was tried")
	}
}

// A peer held for a missing feature gets the rule once it lists the feature,
// without anything about the Service changing.
func TestRetryPeersPicksUpAPeerThatGotTheFeature(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // no features yet
	peer := gw.client(t)
	peer.IsAlive = true
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{"jwt": authLoadBalancerModel(api.APIKeyAuthJWT, "kc")})

	m.retryPeers(GenKey("llm", "jwt"))
	if st := m.lbCache[GenKey("llm", "jwt")].LbServicePairs["pair"].peers[peer.Host]; st.phase != peerHeld {
		t.Fatalf("phase %d on a peer without the features, want held", st.phase)
	}
	drainEvents(recorder)

	gw.features = []string{api.FeatureAPIKeyAuth, api.FeatureJWTAuthProfile}
	if !peer.RefreshFlavor(context.Background()) {
		t.Fatal("RefreshFlavor failed against the fake")
	}

	if m.retryPeers(GenKey("llm", "jwt")) {
		t.Error("still pending after the upgrade")
	}
	if st := m.lbCache[GenKey("llm", "jwt")].LbServicePairs["pair"].peers[peer.Host]; st.phase != peerInstalled {
		t.Errorf("phase %d after the peer listed the features, want installed", st.phase)
	}
	if body := gw.lastBody(t); !strings.Contains(body, `"api_key_auth":"jwt"`) {
		t.Errorf("the rule went out without its policy\n%s", body)
	}
}

func ruleStatusService(name string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"}}
}

// A rule gone from a peer is reported and, in the same sync, put back.
func TestRuleMissingIsPutBack(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // ruleStatus nil: 404
	peer := gw.client(t)
	peer.IsAlive = true
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	key := GenKey("llm", "svc")
	m.lbCache[key].LbServicePairs["pair"].peers = map[string]peerState{peer.Host: {phase: peerInstalled}}

	m.syncRuleStatus(ruleStatusService("svc"), key)
	if st := m.lbCache[key].LbServicePairs["pair"].peers[peer.Host]; st.phase != peerPending {
		t.Fatalf("phase %d after a 404, want pending", st.phase)
	}
	if events := drainEvents(recorder); len(events) != 1 || !strings.Contains(events[0], ReasonRuleMissing) {
		t.Errorf("events = %q, want one %s warning", events, ReasonRuleMissing)
	}

	if m.retryPeers(key) {
		t.Error("still pending after the rule went back")
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 1 {
		t.Errorf("POSTed %d times, want the rule put back once", len(gw.bodies))
	}
}

// A peer kept from the rule on purpose never had it: asking it for the rule's
// status only produced a RuleMissing warning on every sync.
func TestRuleStatusSkipsPeersWithoutTheRule(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{"svc": plainModel()})
	key := GenKey("llm", "svc")
	m.lbCache[key].LbServicePairs["pair"].peers = map[string]peerState{
		peer.Host: {phase: peerHeld, err: ErrEndpointPickerUnsupported},
	}

	m.syncRuleStatus(ruleStatusService("svc"), key)

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %q, want none", events)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.statusQueries != 0 {
		t.Errorf("the held peer was asked for the rule's status %d times", gw.statusQueries)
	}
}
