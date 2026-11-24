package gatewayapi

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	corev1lister "k8s.io/client-go/listers/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// Where the peers route by a wildcard host, a route's wildcard hostname is a
// direct rule of its own, beside an exact host's: the gateway finds the
// exact host first and the wildcard after it, as Gateway API matches them.
// A route without hostnames on a wildcard listener has the listener's.
// Without the peers, the wildcard is left out and said, as before.
func TestAWildcardHostnameIsADirectRuleWhereThePeersRouteByOne(t *testing.T) {
	inputs := func(routes ...*v1.HTTPRoute) directInputs {
		return directInputs{routes: routes, grants: staticGrants(), wildcardHosts: true}
	}
	wild := directRoute("wild", 10, hostnames("*.example.com"), "/")
	exact := directRoute("shop", 5, hostnames("shop.example.com"), "/api")

	rules, refusals := directRules(directGateway(), 80, inputs(wild, exact))
	got := ruleList(rules)
	want := []string{"*.example.com|/ prefix -> apps/web:80 http1 (wild)", "shop.example.com|/api prefix -> apps/web:80 http1 (shop)"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("rules = %v, want %v", got, want)
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %v, want none", refusals)
	}

	gw := directGateway()
	host := v1.Hostname("*.example.com")
	gw.Spec.Listeners[0].Hostname = &host
	anyHost := directRoute("any", 10, nil, "/")
	if rules, _ := directRules(gw, 80, inputs(anyHost)); len(rules) != 1 || rules[0].host != "*.example.com" {
		t.Errorf("on a wildcard listener, a route without hostnames: rules = %v, want the listener's wildcard", ruleList(rules))
	}

	in := inputs(wild)
	in.wildcardHosts = false
	rules, refusals = directRules(directGateway(), 80, in)
	if len(rules) != 0 || len(refusals[types.NamespacedName{Namespace: "apps", Name: "wild"}]) == 0 {
		t.Errorf("without the peers: rules = %v, refusals = %v, want it left out and said why", ruleList(rules), refusals)
	}
}

// A wildcard rule's Service carries the host as it is, under a name the API
// takes: the name is made of a hash, not of the host.
func TestAWildcardRulesServiceCarriesItsHost(t *testing.T) {
	gw := directGateway()
	rules, _ := directRules(gw, 80, directInputs{
		routes: []*v1.HTTPRoute{directRoute("wild", 10, hostnames("*.example.com"), "/")}, grants: staticGrants(), wildcardHosts: true,
	})
	if len(rules) != 1 {
		t.Fatalf("rules = %v", ruleList(rules))
	}
	svc := directService(gw, rules[0], "loxilb.io/loxilb", "loxilb.io/loxilb")
	if got := svc.Annotations[gatewayRouteHostAnnotation]; got != "*.example.com" {
		t.Errorf("host annotation = %q, want *.example.com", got)
	}
	if errs := validation.IsDNS1035Label(svc.Name); len(errs) > 0 {
		t.Errorf("Service name %q: %v", svc.Name, errs)
	}
}

func routeByWildcardHosts() bool { return true }

// The Gateway manager writes a wildcard route's rule Service where the peers
// route by a wildcard host, and the route manager then reads the route as
// served there.
func TestAWildcardRouteIsServedDirectlyWhereThePeersRouteByOne(t *testing.T) {
	section := v1.SectionName("http")
	route := directRoute("wild", 10, hostnames("*.example.com"), "/",
		func(r *v1.HTTPRoute) { r.Spec.ParentRefs[0].SectionName = &section })
	gw := directGateway()

	gm := directManager(t, gatewayPeers, route)
	gm.wildcardHosts = routeByWildcardHosts
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway: %v", err)
	}
	name := directServiceName("gw", 80, "*.example.com", "/")
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no rule service for the wildcard host: %v", err)
	}
	if got := svc.Annotations[gatewayRouteHostAnnotation]; got != "*.example.com" {
		t.Errorf("host annotation = %q", got)
	}

	ingressService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw-ingress-service",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-gateway": "gw"}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443}}}}
	for _, tc := range []struct {
		name   string
		peers  WildcardHosts
		served bool
	}{
		{"peers that route by one", routeByWildcardHosts, true},
		{"peers that do not", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := statusManager(route, gw)
			h.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(route))
			h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))
			h.serviceLister = corev1lister.NewServiceLister(newIndexer(ingressService))
			h.referenceGrants = staticGrants()
			h.directPeers = gatewayPeers
			h.wildcardHosts = tc.peers

			if err := h.syncHTTPRoute(route); err != nil {
				t.Fatalf("syncHTTPRoute: %v", err)
			}
			parent := ours(t, writtenRoute(t, h, route))
			accepted := meta.FindStatusCondition(parent.Conditions, string(v1.RouteConditionAccepted))
			said := accepted != nil && strings.Contains(accepted.Message, "is a wildcard")
			if tc.served && (accepted == nil || accepted.Status != metav1.ConditionTrue || said) {
				t.Errorf("Accepted = %+v, want True without a word of the wildcard", accepted)
			}
			if !tc.served && !said {
				t.Errorf("Accepted = %+v, want it to say the wildcard is not served", accepted)
			}
		})
	}
}
