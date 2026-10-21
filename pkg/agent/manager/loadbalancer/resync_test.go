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
	}, recorder
}

func plainModel() api.LoadBalancerModel {
	return api.LoadBalancerModel{Service: api.LoadBalancerService{
		ExternalIP: "10.0.0.1", Port: 8090, Protocol: "tcp", Mode: api.LBModeFullProxy,
	}}
}

// A peer that comes back on a build without Endpoint Picker support refuses
// the FailClose rule and takes the FailOpen one without the picker. Both have
// to show on their Service, as they do when the rule is first programmed;
// the re-sync used to leave only a log line.
func TestReinstallReportsWhatAPeerWithoutThePickerDoes(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // no features
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failclose": eppLoadBalancerModel(api.EppFailureModeFailClose),
		"failopen":  eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":     plainModel(),
	})

	m.reinstallLoxiLbRules(peer)

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
}

// Back on a build that has the picker, the re-sync installs everything and
// says nothing.
func TestReinstallQuietOnAPeerWithThePicker(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.features = []string{api.FeatureEndpointPicker}
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failclose": eppLoadBalancerModel(api.EppFailureModeFailClose),
		"failopen":  eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":     plainModel(),
	})

	m.reinstallLoxiLbRules(peer)

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events on a peer that supports the picker: %q", events)
	}
}

// A gateway that comes back on a build without the credential features is not
// sent the rule, and the Service says why.
func TestReinstallReportsCredentialPolicyRefusal(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway) // no features
	peer := gw.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"jwt": authLoadBalancerModel(api.APIKeyAuthJWT, "kc"),
	})

	m.reinstallLoxiLbRules(peer)

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
// reported once on the Service, is not asked again in the same pass, and does
// not restart kube-loxilb: the answer would be the same either way. The rule
// used to be retried five times and then end the process.
func TestReinstallReportsRejectionWithoutRestarting(t *testing.T) {
	const reason = "jwt_auth_profile: required by this api_key_auth mode and must name a configured profile"
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.features = []string{api.FeatureAPIKeyAuth, api.FeatureJWTAuthProfile}
	gw.createStatus = http.StatusBadRequest
	gw.createResult = reason
	peer := gw.client(t)
	peer.IsAlive = true // the condition the restart is gated on
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"jwt": authLoadBalancerModel(api.APIKeyAuthJWT, "kc"),
	})

	m.reinstallLoxiLbRules(peer)

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonLoxiLBRejected+" ") ||
		!strings.Contains(events[0], peer.Host) || !strings.Contains(events[0], reason) {
		t.Errorf("events = %q, want one %s warning naming the peer and loxilb's reason", events, ReasonLoxiLBRejected)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.bodies) != 1 {
		t.Errorf("POSTed %d times, want once: a 4xx is not retried", len(gw.bodies))
	}
}

// The other refusals reach their Service the same way.
func TestReinstallReportsPlainPeerRefusal(t *testing.T) {
	plain := newFakeLoxiLB(t, "")
	peer := plain.client(t)
	m, recorder := resyncManager(peer, map[string]api.LoadBalancerModel{
		"failopen": eppLoadBalancerModel(api.EppFailureModeFailOpen),
		"plain":    plainModel(),
	})

	m.reinstallLoxiLbRules(peer)

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonInferenceGatewayRequired+" ") {
		t.Errorf("events = %q, want one %s warning", events, ReasonInferenceGatewayRequired)
	}
}
