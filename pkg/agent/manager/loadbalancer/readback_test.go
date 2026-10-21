package loadbalancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkg/errors"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// authGateway - a gateway that lists the credential features.
func authGateway(t *testing.T) *fakeLoxiLB {
	t.Helper()
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.features = []string{api.FeatureAPIKeyAuth, api.FeatureJWTAuthProfile}
	return gw
}

// counts - POSTs, DELETEs and read-backs this fake has seen.
func (f *fakeLoxiLB) counts() (posts, deletes, readBacks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies), f.deletes, f.readBacks
}

// storedRule - the one rule this fake holds, or nil.
func (f *fakeLoxiLB) storedRule(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rules) > 1 {
		t.Fatalf("%d rules stored, want at most one", len(f.rules))
	}
	for _, rule := range f.rules {
		return rule
	}
	return nil
}

// A gateway gets the rule under a stable id; plain loxilb never sees one.
func TestInstallLBSendsAStableIDToGatewaysOnly(t *testing.T) {
	gw := authGateway(t)
	plain := newFakeLoxiLB(t, "")
	m := &Manager{}
	lb := plainModel()

	if err := m.installLB(gw.client(t), lb, false); err != nil {
		t.Fatalf("installLB on the gateway: %v", err)
	}
	if err := m.installLB(plain.client(t), lb, false); err != nil {
		t.Fatalf("installLB on plain loxilb: %v", err)
	}

	want := api.RuleID(&lb.Service)
	if body := gw.lastBody(t); !strings.Contains(body, `"id":"`+want+`"`) {
		t.Errorf("the gateway did not get id %s\n%s", want, body)
	}
	if body := plain.lastBody(t); strings.Contains(body, `"id"`) {
		t.Errorf("plain loxilb got an id\n%s", body)
	}
	if lb.Service.ID != "" {
		t.Error("installLB set the id on the caller's model")
	}
}

// The usual case: stored as sent, read back once, nothing else.
func TestReadBackAcceptsARuleStoredAsSent(t *testing.T) {
	gw := authGateway(t)
	m := &Manager{}

	if err := m.installLB(gw.client(t), authLoadBalancerModel(api.APIKeyAuthJWT, "kc"), false); err != nil {
		t.Fatalf("installLB: %v", err)
	}
	if posts, deletes, readBacks := gw.counts(); posts != 1 || deletes != 0 || readBacks != 1 {
		t.Errorf("posts %d, deletes %d, read-backs %d; want 1, 0, 1", posts, deletes, readBacks)
	}
}

// A peer that was down while the policy was removed comes back with it: the
// replace leaves the omitted field as it was and answers 409. The read-back
// sees the old policy and adds the rule again without it.
func TestReadBackReplacesAStalePolicy(t *testing.T) {
	gw := authGateway(t)
	peer := gw.client(t)
	m := &Manager{}
	if err := m.installLB(peer, authLoadBalancerModel(api.APIKeyAuthJWT, "kc"), false); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// The annotations go. The policy is no part of the id, so the peer still
	// holds the rule with the policy under the id the new rule carries, and
	// the replace leaves the omitted fields as they were.
	plainRule := authLoadBalancerModel("", "")
	if api.RuleID(&plainRule.Service) != gw.storedRule(t)["id"] {
		t.Fatal("the policy changed the rule's id")
	}
	gw.mu.Lock()
	gw.createAnswers = []fakeAnswer{{status: http.StatusConflict, result: "lbrule-exists error"}}
	gw.mu.Unlock()

	if err := m.installLB(peer, plainRule, false); err != nil {
		t.Fatalf("install without the policy: %v", err)
	}
	if got := gw.storedRule(t); got["api_key_auth"] != nil || got["jwt_auth_profile"] != nil {
		t.Errorf("the peer still holds the old policy: %v", got)
	}
	if _, deletes, _ := gw.counts(); deletes != 1 {
		t.Errorf("deleted %d times, want once", deletes)
	}
}

// A replace that would change a field the gateway does not change in place is
// refused with 409 - the same status as "nothing to change". The read-back
// sees the old value and adds the rule again.
func TestReadBackReplacesARuleTheGatewayWouldNotChange(t *testing.T) {
	gw := authGateway(t)
	peer := gw.client(t)
	m := &Manager{}
	lb := plainModel()
	if err := m.installLB(peer, lb, false); err != nil {
		t.Fatalf("first install: %v", err)
	}

	gw.mu.Lock()
	for _, rule := range gw.rules {
		rule["egress"] = true // what the peer kept from before
	}
	gw.createAnswers = []fakeAnswer{{status: http.StatusConflict, result: "lbrule-exist error: cant modify rule egress mode"}}
	gw.mu.Unlock()

	if err := m.installLB(peer, lb, false); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := gw.storedRule(t); got["egress"] == true {
		t.Errorf("the old egress mode is still stored: %v", got)
	}
}

// A gateway without the id-only rebind keeps a rule installed before ids
// under its minted id and answers 409: the read-back finds nothing under the
// new id and adds the rule again, once.
func TestReadBackRecreatesARuleNotStoredUnderItsID(t *testing.T) {
	gw := authGateway(t)
	peer := gw.client(t)
	m := &Manager{}
	lb := plainModel()

	gw.mu.Lock()
	gw.rules = map[string]map[string]any{
		"minted-before-ids": {"id": "minted-before-ids", "externalIP": "10.0.0.1", "port": 8090, "protocol": "tcp", "mode": 4},
	}
	gw.createAnswers = []fakeAnswer{{status: http.StatusConflict, result: "lbrule-exists error"}}
	gw.mu.Unlock()

	if err := m.installLB(peer, lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}
	if got := gw.storedRule(t); got["id"] != api.RuleID(&lb.Service) {
		t.Errorf("stored id %v, want the rule's own id", got["id"])
	}
	if posts, deletes, _ := gw.counts(); posts != 2 || deletes != 1 {
		t.Errorf("posts %d, deletes %d; want 2 and 1", posts, deletes)
	}
}

// A peer that lists the features and still stores the rule without the policy
// would serve it unauthenticated. After one fresh add the rule is removed and
// the peer is held - a refusal, not a failure to retry.
func TestReadBackHoldsAPeerThatDropsThePolicy(t *testing.T) {
	gw := authGateway(t)
	gw.dropFields = []string{"api_key_auth", "jwt_auth_profile"}
	m := &Manager{}

	err := m.installLB(gw.client(t), authLoadBalancerModel(api.APIKeyAuthJWT, "kc"), false)
	if !errors.Is(err, ErrAuthUnsupported) || !isPeerRefusal(err) {
		t.Fatalf("installLB = %v, want ErrAuthUnsupported", err)
	}
	if got := gw.storedRule(t); got != nil {
		t.Errorf("a rule without its policy was left on the peer: %v", got)
	}
}

// Whatever still differs after a fresh add is how the peer stores the rule.
// It is let be: re-adding it on every sync would only drop connections.
func TestReadBackLeavesADifferenceThatSurvivesAFreshAdd(t *testing.T) {
	gw := authGateway(t)
	gw.dropFields = []string{"kvEngineType"}
	m := &Manager{}
	lb := plainModel()
	lb.Service.KvEngineType = "sglang"

	if err := m.installLB(gw.client(t), lb, false); err != nil {
		t.Fatalf("installLB = %v, want the rule left as installed", err)
	}
	if posts, deletes, _ := gw.counts(); posts != 2 || deletes != 1 {
		t.Errorf("posts %d, deletes %d; want one fresh add and no more", posts, deletes)
	}
}

// A rule that cannot be read back is left as the POST answered.
func TestReadBackLeavesARuleItCannotRead(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/netlox/v1/version":
			_ = json.NewEncoder(w).Encode(&api.VersionModel{Version: "test", Product: api.ProductInferenceGateway})
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		case r.Method == http.MethodDelete:
			deletes++
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	if err := (&Manager{}).installLB(clientFor(t, srv), plainModel(), false); err != nil {
		t.Fatalf("installLB = %v", err)
	}
	if deletes != 0 {
		t.Errorf("a rule that could not be read back was deleted %d times", deletes)
	}
}

// With appended endpoints the rule is shared with other clusters; putting it
// right would mean deleting it, so it is not read back.
func TestReadBackSkippedWithAppendedEndpoints(t *testing.T) {
	gw := authGateway(t)
	m := &Manager{networkConfig: &config.NetworkConfig{AppendEPs: true}}

	if err := m.installLB(gw.client(t), plainModel(), false); err != nil {
		t.Fatalf("installLB: %v", err)
	}
	if _, _, readBacks := gw.counts(); readBacks != 0 {
		t.Errorf("read back %d times with appended endpoints", readBacks)
	}
}
