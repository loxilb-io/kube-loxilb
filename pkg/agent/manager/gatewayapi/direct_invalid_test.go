package gatewayapi

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1lister "k8s.io/client-go/listers/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func takeRulesWithoutEndpoints() bool { return true }

// The backends the spec calls invalid.
var invalidBackends = map[string]func(*v1.HTTPRoute){
	"another namespace without a grant": func(r *v1.HTTPRoute) {
		r.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
	},
	"not a Service": func(r *v1.HTTPRoute) {
		kind := v1.Kind("Bucket")
		group := v1.Group("example.com")
		r.Spec.Rules[0].BackendRefs[0].Kind, r.Spec.Rules[0].BackendRefs[0].Group = &kind, &group
	},
}

// Where the peers take a rule without endpoints, a rule whose backend is
// invalid is one, answered 500, in place of the longest other prefix of its
// host. What a rule cannot hold yet is still left out.
func TestAnInvalidBackendIsADirectRuleWhereThePeersTakeOne(t *testing.T) {
	inputs := func(routes ...*v1.HTTPRoute) directInputs {
		return directInputs{routes: routes, grants: staticGrants(), withoutEndpoints: true}
	}
	shop := types.NamespacedName{Namespace: "apps", Name: "shop"}

	for name, mutate := range invalidBackends {
		t.Run(name, func(t *testing.T) {
			route := directRoute("shop", 10, hostnames("shop.example.com"), "/cart", mutate)
			rules, refusals := directRules(directGateway(), 80, inputs(route))
			if len(rules) != 1 || rules[0].invalidBackend == "" || rules[0].backend != (types.NamespacedName{}) {
				t.Fatalf("rules = %v (%+v), want one without a backend", ruleList(rules), rules)
			}
			if rules[0].host != "shop.example.com" || rules[0].path != "/cart" || rules[0].backendProtocol != "http1" {
				t.Errorf("rule = %+v", rules[0])
			}
			if len(refusals[shop]) != 0 {
				t.Errorf("refusals = %v, want none", refusals[shop])
			}

			// Without the peers, as before.
			in := inputs(route)
			in.withoutEndpoints = false
			if rules, refusals := directRules(directGateway(), 80, in); len(rules) != 0 || len(refusals[shop]) == 0 {
				t.Errorf("without the peers: rules = %v, refusals = %v, want it left out and said why", ruleList(rules), refusals)
			}
		})
	}

	weight := int32(1)
	for name, mutate := range map[string]func(*v1.HTTPRoute){
		"a filter as well": func(r *v1.HTTPRoute) {
			invalidBackends["not a Service"](r)
			r.Spec.Rules[0].Filters = []v1.HTTPRouteFilter{{Type: v1.HTTPRouteFilterRequestHeaderModifier}}
		},
		"one of two backends": func(r *v1.HTTPRoute) {
			second := r.Spec.Rules[0].BackendRefs[0]
			second.Name, second.Weight = "web-v2", &weight
			second.Namespace = namespacePtr("backends")
			r.Spec.Rules[0].BackendRefs = append(r.Spec.Rules[0].BackendRefs, second)
		},
		"an InferencePool": func(r *v1.HTTPRoute) {
			kind := v1.Kind(inferencePoolKind)
			group := v1.Group(inferencePoolGroup)
			r.Spec.Rules[0].BackendRefs[0].Kind, r.Spec.Rules[0].BackendRefs[0].Group = &kind, &group
		},
	} {
		t.Run(name, func(t *testing.T) {
			route := directRoute("shop", 10, hostnames("shop.example.com"), "/cart", mutate)
			if rules, refusals := directRules(directGateway(), 80, inputs(route)); len(rules) != 0 || len(refusals[shop]) == 0 {
				t.Errorf("rules = %v, refusals = %v, want it left out and said why", ruleList(rules), refusals)
			}
		})
	}

	// An older rule with an invalid backend has no protocol to hold the
	// listener to: a younger HTTP/2 rule is served, and it takes HTTP/2.
	t.Run("the listener's protocol", func(t *testing.T) {
		h2c := h2cAppProtocol
		in := inputs(
			directRoute("shop", 10, hostnames("shop.example.com"), "/cart", invalidBackends["not a Service"]),
			directRoute("grpc", 1, hostnames("grpc.example.com"), "/", func(r *v1.HTTPRoute) { r.Spec.Rules[0].BackendRefs[0].Name = "grpc" }),
		)
		in.services = corev1lister.NewServiceLister(newIndexer(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "grpc"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, AppProtocol: &h2c}}}}))
		rules, refusals := directRules(directGateway(), 80, in)
		if len(rules) != 2 || len(refusals) != 0 {
			t.Fatalf("rules = %v, refusals = %v, want both", ruleList(rules), refusals)
		}
		for _, r := range rules {
			if r.backendProtocol != "http2" {
				t.Errorf("rule %s speaks %s, want the listener's http2", r.key(), r.backendProtocol)
			}
		}
	})
}

// The rule's Service names why its backend is invalid instead of a backend,
// and is the same Service once the backend is fixed: its name does not
// depend on the backend.
func TestAnInvalidBackendsRuleServiceSaysWhy(t *testing.T) {
	route := directRoute("shop", 10, hostnames("shop.example.com"), "/api", invalidBackends["another namespace without a grant"])
	gm := directManager(t, gatewayPeers, route)
	gm.rulesWithoutEndpoints = takeRulesWithoutEndpoints
	gw := directGateway()

	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway: %v", err)
	}
	name := directServiceName("gw", 80, "shop.example.com", "/api")
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no rule service: %v", err)
	}
	why := svc.Annotations[gatewayRouteInvalidBackendAnnotation]
	if !strings.Contains(why, "ReferenceGrant") {
		t.Errorf("%s = %q, want why", gatewayRouteInvalidBackendAnnotation, why)
	}
	for _, key := range []string{gatewayRouteBackendAnnotation, gatewayRouteBackendPortAnnotation} {
		if v, ok := svc.Annotations[key]; ok {
			t.Errorf("%s = %q, want none", key, v)
		}
	}

	// The grant comes: the same Service gets the backend.
	gm.referenceGrants = staticGrants(referenceGrant("backends", "web", grantSpec(kindHTTPRoute, "apps", "web")))
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway with the grant: %v", err)
	}
	svc, err = gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the rule service went: %v", err)
	}
	if _, ok := svc.Annotations[gatewayRouteInvalidBackendAnnotation]; ok || svc.Annotations[gatewayRouteBackendAnnotation] != "backends/web" {
		t.Errorf("rule service annotations = %v, want backends/web", svc.Annotations)
	}
}

// A route whose one rule has an invalid backend is accepted on a direct
// listener where the peers take rules without endpoints: its rule is served,
// answered 500, and ResolvedRefs says what is wrong. Elsewhere the listener
// does not serve it.
func TestARouteWithAnInvalidBackendIsAcceptedWhereItIsAnswered500(t *testing.T) {
	gw := directGateway()
	section := v1.SectionName("http")
	route := directRoute("shop", 10, hostnames("shop.example.com"), "/api", invalidBackends["another namespace without a grant"],
		func(r *v1.HTTPRoute) { r.Spec.ParentRefs[0].SectionName = &section })
	ingressService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw-ingress-service",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-gateway": "gw"}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443}}}}

	for _, tc := range []struct {
		name     string
		peers    RulesWithoutEndpoints
		accepted metav1.ConditionStatus
		reason   v1.RouteConditionReason
	}{
		{"peers that take them", takeRulesWithoutEndpoints, metav1.ConditionTrue, v1.RouteReasonAccepted},
		{"peers that do not", nil, metav1.ConditionFalse, v1.RouteReasonUnsupportedValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := statusManager(route, gw)
			h.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(route))
			h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))
			h.serviceLister = corev1lister.NewServiceLister(newIndexer(ingressService))
			h.referenceGrants = staticGrants()
			h.directPeers = gatewayPeers
			h.rulesWithoutEndpoints = tc.peers

			if err := h.syncHTTPRoute(route); err != nil {
				t.Fatalf("syncHTTPRoute: %v", err)
			}
			parent := ours(t, writtenRoute(t, h, route))
			checkCondition(t, parent, v1.RouteConditionAccepted, tc.accepted, tc.reason, "")
			checkCondition(t, parent, v1.RouteConditionResolvedRefs, metav1.ConditionFalse, v1.RouteReasonRefNotPermitted, "backends")
		})
	}
}
