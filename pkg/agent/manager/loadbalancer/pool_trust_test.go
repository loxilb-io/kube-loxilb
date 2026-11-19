package loadbalancer

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

// refusingPools - the InferencePool manager has decided on every Service:
// it made none of them.
func refusingPools(*corev1.Service) (generated, decided bool) { return false, true }

// undecidedPools - the InferencePool manager has not reconciled any pool yet.
func undecidedPools(*corev1.Service) (generated, decided bool) { return false, false }

// The pool label is something any Service can carry. One the InferencePool
// manager did not make is refused, said so, and nothing reaches the gateway;
// one it has not decided on waits, without an event.
func TestPoolServiceTheInferencePoolManagerDidNotMakeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   PoolServiceCheck
		wantErr error
		event   bool
	}{
		{"refused", refusingPools, ErrPoolServiceUntrusted, true},
		{"no InferencePool manager", NoPoolServices, ErrPoolServiceUntrusted, true},
		{"not decided yet", undecidedPools, errPoolsNotDecided, false},
		{"not given yet", nil, errPoolsNotDecided, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
			peer := gw.client(t)
			peer.IsAlive = true
			m, _ := addHarness(t, peer)
			m.SetPoolServiceCheck(tc.check)

			err := m.addLoadBalancer(poolService("a", "qwen", "192.168.0.13", 30001))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("addLoadBalancer = %v, want %v", err, tc.wantErr)
			}
			if n := posts(gw); n != 0 {
				t.Errorf("POSTed %d rules for it", n)
			}
			refused := false
			recorder := m.eventRecorder.(*record.FakeRecorder)
			for len(recorder.Events) > 0 {
				if strings.Contains(<-recorder.Events, ReasonPoolServiceRefused) {
					refused = true
				}
			}
			if refused != tc.event {
				t.Errorf("%s event recorded: %v, want %v", ReasonPoolServiceRefused, refused, tc.event)
			}
		})
	}
}

// A refused Service takes no listener, so it has no say on one: the label
// lent it the age of any pool in its namespace, and with it the listener's
// policy. One not decided on yet still counts, as before, so the verdicts
// do not turn over while the InferencePool manager starts.
func TestRefusedPoolServiceHasNoSayOnTheListener(t *testing.T) {
	lookalike := onListener("lookalike", 5, api.APIKeyAuthJWT, "kc")
	lookalike.Labels = map[string]string{inferencePoolLabel: "old-pool"}
	pool := poolOnListener("pool", 1, "qwen")
	m, _ := listenerManager(t, lookalike, pool)
	m.SetInferencePoolLister(poolLister(t, &infv1.InferencePool{ObjectMeta: metav1.ObjectMeta{
		Name: "old-pool", Namespace: "llm", CreationTimestamp: metav1.NewTime(listenerEpoch),
	}}))

	m.SetPoolServiceCheck(func(svc *corev1.Service) (bool, bool) { return svc.Name != "lookalike", true })
	if v := m.judgeListeners(pool); v.yieldTo != nil {
		t.Errorf("the pool yields to %s, a Service refused as a pool's", v.yieldTo.Name)
	}
	if addr := m.listenerAddrOf(lookalike); addr != "" {
		t.Errorf("the refused Service shares the listener at %q", addr)
	}
	if addr := m.listenerAddrOf(pool); addr != pool.Spec.LoadBalancerIP {
		t.Errorf("the pool's Service shares the listener at %q, want %q", addr, pool.Spec.LoadBalancerIP)
	}

	m.SetPoolServiceCheck(undecidedPools)
	if v := m.judgeListeners(pool); v.yieldTo == nil || v.yieldTo.Name != "lookalike" {
		t.Errorf("not decided yet: the pool yields to %v, want the older pool's Service as before", v.yieldTo)
	}
}

// A gateway route Service no Gateway controls is refused too, and has no say
// on the listener either.
func TestRefusedGatewayRouteHasNoSayOnTheListener(t *testing.T) {
	lookalike := onListener("lookalike", 0, api.APIKeyAuthJWT, "kc")
	lookalike.Labels = map[string]string{gatewayRouteLabel: "true"}
	pool := poolOnListener("pool", 1, "qwen")
	m, _ := listenerManager(t, lookalike, pool)
	m.SetGatewayLister(nil, func() bool { return true })

	if v := m.judgeListeners(pool); v.yieldTo != nil {
		t.Errorf("the pool yields to %s, a gateway route no Gateway controls", v.yieldTo.Name)
	}
}
