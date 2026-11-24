package loadbalancer

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// A gateway route whose host is a wildcard goes to the peers with that host,
// where every peer routes by one, and its address is reported.
func TestAWildcardRouteIsSentWithItsHost(t *testing.T) {
	peers := []*fakeLoxiLB{gatewayPeer(t, api.FeatureWildcardHost), gatewayPeer(t, api.FeatureWildcardHost)}
	m, route := routeTo(t, peers, func(s *corev1.Service) {
		s.Annotations[gatewayRouteHostAnnotation] = "*.example.com"
	}, backendService(), webReady())

	if err := m.addLoadBalancer(route); err != nil {
		t.Fatalf("addLoadBalancer: %v", err)
	}
	for i, p := range peers {
		if body := p.lastBody(t); !strings.Contains(body, `"host":"*.example.com"`) {
			t.Errorf("peer %d got %s, want the host *.example.com", i, body)
		}
	}
	if !installedAt(t, m, route) {
		t.Error("the route does not report its address")
	}
}

// A peer that does not route by a wildcard host is not sent one: it would
// keep the rule and match no request against it. A rule with an exact host
// goes to it as before.
func TestInstallLBSendsAWildcardHostOnlyWhereItIsRouted(t *testing.T) {
	model := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.10", Port: 80, Protocol: "tcp", Mode: api.LBModeFullProxy,
			Host: "*.example.com", PathPrefix: "/", PathMatchMode: api.PathMatchModePrefix,
			GatewayRoute: true,
		},
		Endpoints: []api.LoadBalancerEndpoint{{EndpointIP: "10.1.0.1", TargetPort: 8000, Weight: 1}},
	}
	m := &Manager{}

	without := gatewayPeer(t)
	if err := m.installLB(without.client(t), model, false); !errors.Is(err, ErrWildcardHostUnsupported) {
		t.Errorf("installLB on a peer without the feature = %v, want it declined", err)
	}
	if n := posts(without); n != 0 {
		t.Errorf("the peer without the feature got %d rules", n)
	}

	exact := model
	exact.Service.Host = "shop.example.com"
	if err := m.installLB(without.client(t), exact, false); err != nil {
		t.Errorf("installLB of an exact host on a peer without the feature: %v", err)
	}

	with := gatewayPeer(t, api.FeatureWildcardHost)
	if err := m.installLB(with.client(t), model, false); err != nil {
		t.Errorf("installLB on a peer with the feature: %v", err)
	}
}

func TestServesWildcardHosts(t *testing.T) {
	with, without := gatewayPeer(t, api.FeatureWildcardHost), gatewayPeer(t)
	undetected := &api.LoxiClient{Host: "10.9.9.9"}
	for name, tc := range map[string]struct {
		peers []*api.LoxiClient
		want  bool
	}{
		"no peer":                 {nil, false},
		"every peer":              {[]*api.LoxiClient{with.client(t), with.client(t)}, true},
		"one without the feature": {[]*api.LoxiClient{with.client(t), without.client(t)}, false},
		"one not detected yet":    {[]*api.LoxiClient{with.client(t), undetected}, false},
	} {
		t.Run(name, func(t *testing.T) {
			pool := api.NewLoxiClientPool()
			for _, c := range tc.peers {
				pool.AddLoxiClient(c)
			}
			m := &Manager{LoxiClients: pool}
			if got := m.ServesWildcardHosts(); got != tc.want {
				t.Errorf("ServesWildcardHosts = %v, want %v", got, tc.want)
			}
		})
	}
}
