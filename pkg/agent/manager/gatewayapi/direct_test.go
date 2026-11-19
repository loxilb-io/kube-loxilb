package gatewayapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	corev1lister "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func gatewayPeers() (bool, string) { return true, "" }
func plainPeers() (bool, string)   { return false, "loxilb-lb(10.0.0.1) is plain loxilb" }

// directGateway - Gateway infra/gw on 10.0.0.10: HTTP on 80 for any host,
// HTTPS on 443, both taking routes from every namespace.
func directGateway() *v1.Gateway {
	gw := &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw", UID: types.UID("gw-uid"), Generation: 1},
		Spec: v1.GatewaySpec{
			GatewayClassName: testClass,
			Addresses:        []v1.GatewaySpecAddress{{Value: "10.0.0.10"}},
			Listeners: []v1.Listener{
				listener("http", v1.HTTPProtocolType, 80, ""),
				withCertificate(listener("https", v1.HTTPSProtocolType, 443, ""), "cert"),
			},
		},
	}
	for i := range gw.Spec.Listeners {
		gw.Spec.Listeners[i].AllowedRoutes = fromNamespaces(v1.NamespacesFromAll, nil)
	}
	return gw
}

// directRoute - an HTTPRoute in apps to Service web:80 on gw, at age minutes
// old, for the given hostnames and Prefix path.
func directRoute(name string, age int, hosts []v1.Hostname, path string, mutate ...func(*v1.HTTPRoute)) *v1.HTTPRoute {
	gwNs := v1.Namespace("infra")
	prefix := v1.PathMatchPathPrefix
	port := v1.PortNumber(80)
	r := &v1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(age) * time.Minute))},
		Spec: v1.HTTPRouteSpec{
			CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: []v1.ParentReference{{Name: "gw", Namespace: &gwNs}}},
			Hostnames:       hosts,
			Rules: []v1.HTTPRouteRule{{
				Matches:     []v1.HTTPRouteMatch{{Path: &v1.HTTPPathMatch{Type: &prefix, Value: &path}}},
				BackendRefs: []v1.HTTPBackendRef{{BackendRef: v1.BackendRef{BackendObjectReference: v1.BackendObjectReference{Name: "web", Port: &port}}}},
			}},
		},
	}
	for _, f := range mutate {
		f(r)
	}
	return r
}

func ruleList(rules []directRule) []string {
	var out []string
	for _, r := range rules {
		match := "prefix"
		if r.exact {
			match = "exact"
		}
		out = append(out, fmt.Sprintf("%s|%s %s -> %s:%d %s (%s)", r.host, r.path, match, r.backend, r.backendPort, r.backendProtocol, r.route.Name))
	}
	sort.Strings(out)
	return out
}

func TestDirectPortsOf(t *testing.T) {
	cases := []struct {
		name    string
		gateway func(*v1.Gateway)
		peers   DirectPeers
		claimed map[v1.SectionName]bool
		direct  []v1.PortNumber
		why     map[v1.PortNumber]string
	}{
		{name: "HTTP on gateway peers", peers: gatewayPeers, direct: []v1.PortNumber{80},
			why: map[v1.PortNumber]string{443: "is HTTPS"}},
		{name: "a plain peer", peers: plainPeers,
			why: map[v1.PortNumber]string{80: "plain loxilb", 443: "plain loxilb"}},
		{name: "an IPv6 address", peers: gatewayPeers,
			gateway: func(gw *v1.Gateway) { gw.Spec.Addresses[0].Value = "fd00::10" },
			why:     map[v1.PortNumber]string{80: "not IPv4"}},
		{name: "no address yet", peers: gatewayPeers,
			gateway: func(gw *v1.Gateway) { gw.Spec.Addresses = nil },
			why:     map[v1.PortNumber]string{80: "no address"}},
		{name: "a listener an InferencePool serves", peers: gatewayPeers, claimed: map[v1.SectionName]bool{"http": true},
			why: map[v1.PortNumber]string{80: "InferencePool"}},
		{name: "HTTPS on the HTTP port", peers: gatewayPeers,
			gateway: func(gw *v1.Gateway) {
				gw.Spec.Listeners = append(gw.Spec.Listeners, withCertificate(listener("also-80", v1.HTTPSProtocolType, 80, "a.example.com"), "cert"))
			},
			why: map[v1.PortNumber]string{80: "is HTTPS"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := directGateway()
			if tc.gateway != nil {
				tc.gateway(gw)
			}
			direct, why := directPortsOf(gw, tc.claimed, tc.peers)
			if len(direct) != len(tc.direct) {
				t.Errorf("direct = %v, want %v", direct, tc.direct)
			}
			for _, p := range tc.direct {
				if !direct[p] {
					t.Errorf("port %d not direct", p)
				}
			}
			for port, part := range tc.why {
				if !strings.Contains(why[port], part) {
					t.Errorf("port %d: why = %q, want it to say %q", port, why[port], part)
				}
			}
		})
	}
}

func TestDirectRules(t *testing.T) {
	inputs := func(routes ...*v1.HTTPRoute) directInputs {
		return directInputs{routes: routes, grants: staticGrants()}
	}

	t.Run("host and path as the gateway compares them", func(t *testing.T) {
		rules, refusals := directRules(directGateway(), 80, inputs(
			directRoute("shop", 10, hostnames("shop.example.com"), "/api/"),
			directRoute("any", 5, nil, "/"),
		))
		want := []string{
			"shop.example.com|/api prefix -> apps/web:80 http1 (shop)",
			"|/ prefix -> apps/web:80 http1 (any)",
		}
		if got := ruleList(rules); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("rules = %v, want %v", got, want)
		}
		if len(refusals) != 0 {
			t.Errorf("refusals = %v", refusals)
		}
	})

	t.Run("what a direct rule cannot hold is left out", func(t *testing.T) {
		weight := int32(1)
		zero := int32(0)
		regex := v1.PathMatchRegularExpression
		cart := "/cart"
		cases := map[string]func(*v1.HTTPRoute){
			"header": func(r *v1.HTTPRoute) {
				r.Spec.Rules[0].Matches[0].Headers = []v1.HTTPHeaderMatch{{Name: "x-env", Value: "canary"}}
			},
			"filter": func(r *v1.HTTPRoute) {
				r.Spec.Rules[0].Filters = []v1.HTTPRouteFilter{{Type: v1.HTTPRouteFilterRequestHeaderModifier}}
			},
			"two backends": func(r *v1.HTTPRoute) {
				second := r.Spec.Rules[0].BackendRefs[0]
				second.Name, second.Weight = "web-v2", &weight
				r.Spec.Rules[0].BackendRefs = append(r.Spec.Rules[0].BackendRefs, second)
			},
			"regex": func(r *v1.HTTPRoute) {
				r.Spec.Rules[0].Matches[0].Path = &v1.HTTPPathMatch{Type: &regex, Value: &cart}
			},
			"wildcard host": func(r *v1.HTTPRoute) { r.Spec.Hostnames = hostnames("*.example.com") },
			"another namespace without a grant": func(r *v1.HTTPRoute) {
				r.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
			},
			"not a Service": func(r *v1.HTTPRoute) {
				kind := v1.Kind("Bucket")
				group := v1.Group("example.com")
				r.Spec.Rules[0].BackendRefs[0].Kind, r.Spec.Rules[0].BackendRefs[0].Group = &kind, &group
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				route := directRoute("shop", 10, hostnames("shop.example.com"), "/", mutate)
				rules, refusals := directRules(directGateway(), 80, inputs(route))
				if len(rules) != 0 {
					t.Errorf("served %v", ruleList(rules))
				}
				if len(refusals[types.NamespacedName{Namespace: "apps", Name: "shop"}]) == 0 {
					t.Error("not said why")
				}
			})
		}

		// A second backend with no weight takes no traffic: one remains.
		route := directRoute("shop", 10, hostnames("shop.example.com"), "/", func(r *v1.HTTPRoute) {
			drained := r.Spec.Rules[0].BackendRefs[0]
			drained.Name, drained.Weight = "web-old", &zero
			r.Spec.Rules[0].BackendRefs = append(r.Spec.Rules[0].BackendRefs, drained)
		})
		if rules, _ := directRules(directGateway(), 80, inputs(route)); len(rules) != 1 || rules[0].backend.Name != "web" {
			t.Errorf("rules = %v, want the one backend with weight", ruleList(rules))
		}
	})

	t.Run("a backend in another namespace with a grant", func(t *testing.T) {
		route := directRoute("shop", 10, hostnames("shop.example.com"), "/", func(r *v1.HTTPRoute) {
			r.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
		})
		in := inputs(route)
		in.grants = staticGrants(referenceGrant("backends", "web", grantSpec(kindHTTPRoute, "apps", "web")))
		if rules, _ := directRules(directGateway(), 80, in); len(rules) != 1 || rules[0].backend.Namespace != "backends" {
			t.Errorf("rules = %v", ruleList(rules))
		}
	})

	t.Run("routes that want the same host and path", func(t *testing.T) {
		exact := v1.PathMatchExact
		older := directRoute("older", 20, hostnames("shop.example.com"), "/api")
		younger := directRoute("younger", 1, hostnames("shop.example.com"), "/api")
		// Exact and Prefix on one host and path share the gateway's pool:
		// the Exact one has it, however young.
		exactOne := directRoute("exact", 0, hostnames("shop.example.com"), "/api", func(r *v1.HTTPRoute) {
			r.Spec.Rules[0].Matches[0].Path.Type = &exact
		})

		rules, refusals := directRules(directGateway(), 80, inputs(younger, older))
		if got := ruleList(rules); len(got) != 1 || !strings.HasSuffix(got[0], "(older)") {
			t.Errorf("rules = %v, want the older route's", got)
		}
		if why := refusals[types.NamespacedName{Namespace: "apps", Name: "younger"}]; len(why) != 1 || !strings.Contains(why[0], "apps/older") {
			t.Errorf("younger's refusal = %v", why)
		}

		rules, _ = directRules(directGateway(), 80, inputs(older, exactOne))
		if got := ruleList(rules); len(got) != 1 || !strings.HasSuffix(got[0], "(exact)") {
			t.Errorf("rules = %v, want the Exact match", got)
		}
	})

	t.Run("one route through two parentRefs", func(t *testing.T) {
		section := v1.SectionName("http")
		route := directRoute("shop", 10, hostnames("shop.example.com"), "/", func(r *v1.HTTPRoute) {
			again := r.Spec.ParentRefs[0]
			again.SectionName = &section
			r.Spec.ParentRefs = append(r.Spec.ParentRefs, again)
		})
		rules, refusals := directRules(directGateway(), 80, inputs(route))
		if len(rules) != 1 || len(refusals) != 0 {
			t.Errorf("rules = %v, refusals = %v, want one rule and no conflict with itself", ruleList(rules), refusals)
		}
	})

	t.Run("one backend protocol for the listener", func(t *testing.T) {
		h2c := h2cAppProtocol
		services := corev1lister.NewServiceLister(newIndexer(
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
				Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "grpc"},
				Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, AppProtocol: &h2c}}}},
		))
		grpc := directRoute("grpc", 1, hostnames("grpc.example.com"), "/", func(r *v1.HTTPRoute) {
			r.Spec.Rules[0].BackendRefs[0].Name = "grpc"
		})
		in := inputs(directRoute("web", 10, hostnames("shop.example.com"), "/"), grpc)
		in.services = services
		rules, refusals := directRules(directGateway(), 80, in)
		if got := ruleList(rules); len(got) != 1 || !strings.Contains(got[0], "http1 (web)") {
			t.Errorf("rules = %v, want the older route's HTTP/1 rule only", got)
		}
		if why := refusals[types.NamespacedName{Namespace: "apps", Name: "grpc"}]; len(why) != 1 || !strings.Contains(why[0], "http2") {
			t.Errorf("grpc's refusal = %v", why)
		}
	})
}

func TestDirectServiceName(t *testing.T) {
	a := directServiceName("gw", 80, "shop.example.com", "/api")
	if a != directServiceName("gw", 80, "shop.example.com", "/api") {
		t.Error("the name is not stable")
	}
	for _, other := range []string{
		directServiceName("gw", 81, "shop.example.com", "/api"),
		directServiceName("gw", 80, "cart.example.com", "/api"),
		directServiceName("gw", 80, "shop.example.com", "/"),
		directServiceName("gw2", 80, "shop.example.com", "/api"),
	} {
		if other == a {
			t.Errorf("two rules share the name %s", a)
		}
	}
	for _, gateway := range []string{"gw", "9-lives", "a.gateway.with.dots", strings.Repeat("long-gateway-name-", 20)} {
		if errs := validation.IsDNS1035Label(directServiceName(gateway, 80, "", "/")); len(errs) != 0 {
			t.Errorf("gateway %q: %v", gateway, errs)
		}
	}
}

// directManager - a Gateway manager serving routes directly, with gw and
// the routes given, whose Service lister follows its fake API server.
func directManager(t *testing.T, peers DirectPeers, routes ...*v1.HTTPRoute) *GatewayManager {
	t.Helper()
	gm := gatewayTestManager(t, nil, []*v1.Gateway{directGateway()})
	objs := make([]interface{}, 0, len(routes))
	for _, r := range routes {
		objs = append(objs, r)
	}
	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(objs...))
	gm.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))
	gm.serviceLister = corev1lister.NewServiceLister(newIndexer())
	gm.directPeers = peers
	gm.referenceGrants = staticGrants()
	gm.networkConfig.Zone = "llb"
	return gm
}

// syncServices - bring the manager's Service lister to what its API server
// holds, as the informer would.
func syncServices(t *testing.T, gm *GatewayManager) {
	t.Helper()
	list, err := gm.kubeClient.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	objs := make([]interface{}, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	gm.serviceLister = corev1lister.NewServiceLister(newIndexer(objs...))
}

func reportInstalled(t *testing.T, gm *GatewayManager, name string) {
	t.Helper()
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// As the load-balancer manager writes it in a zone: a hostname.
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "llb-" + svc.Spec.LoadBalancerIP}}
	if _, err := gm.kubeClient.CoreV1().Services("infra").UpdateStatus(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func ingressServicePortNumbers(t *testing.T, gm *GatewayManager) []int32 {
	t.Helper()
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), "gw-ingress-service", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var ports []int32
	for _, p := range svc.Spec.Ports {
		ports = append(ports, p.Port)
	}
	return ports
}

// A port goes direct in steps: its rules first, then, once they are on the
// peers, the ingress service lets the port go. A route that goes takes its
// rule with it.
func TestGatewayServesRoutesDirectly(t *testing.T) {
	route := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm := directManager(t, gatewayPeers, route)
	gw := directGateway()
	name := directServiceName("gw", 80, "shop.example.com", "/api")

	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no rule service: %v", err)
	}
	if !controlledBy(svc, gw) || svc.Annotations[gatewayRouteHostAnnotation] != "shop.example.com" ||
		svc.Annotations[gatewayRouteBackendAnnotation] != "apps/web" || svc.Spec.LoadBalancerIP != "10.0.0.10" {
		t.Errorf("rule service = %+v", svc.ObjectMeta)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 kept until the rule is on the peers", got)
	}
	ingress, _ := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), "gw-ingress-service", metav1.GetOptions{})
	if !controlledBy(ingress, gw) {
		t.Error("the ingress service is not controlled by the Gateway, so it cannot share the port with the rule")
	}

	// The rule's Service is there but not on the peers yet: the port stays.
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync before the rule is installed: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 kept while the rule is not on the peers", got)
	}

	reportInstalled(t, gm, name)
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync after the rule is installed: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Errorf("ingress service ports = %v, want HTTPS only", got)
	}

	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer())
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync after the route went: %v", err)
	}
	if _, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Errorf("the rule outlived its route (%v)", err)
	}
}

// With a plain peer the port goes back to the ingress service, and the
// rules left from when it was direct go after it.
func TestGatewayFallsBackToTheIngressService(t *testing.T) {
	route := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm := directManager(t, gatewayPeers, route)
	gw := directGateway()
	name := directServiceName("gw", 80, "shop.example.com", "/api")
	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}
	reportInstalled(t, gm, name)
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}

	gm.directPeers = plainPeers
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with a plain peer: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 back", got)
	}
	if _, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Errorf("the direct rule stayed beside the ingress service (%v)", err)
	}
}

// With routes served directly, a listener the ingress service does not carry
// gets no Ingress, and what its direct rules leave out of a route is said in
// the route's status.
func TestHTTPRouteLeavesDirectListenersToTheGateway(t *testing.T) {
	gw := directGateway()
	route := directRoute("shop", 10, hostnames("shop.example.com"), "/api", func(r *v1.HTTPRoute) {
		r.Spec.Rules = append(r.Spec.Rules, v1.HTTPRouteRule{
			Filters:     []v1.HTTPRouteFilter{{Type: v1.HTTPRouteFilterRequestHeaderModifier}},
			BackendRefs: r.Spec.Rules[0].BackendRefs,
		})
	})
	ingressService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw-ingress-service",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-gateway": "gw"}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443}}}}

	h := statusManager(route, gw)
	h.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(route))
	h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))
	h.serviceLister = corev1lister.NewServiceLister(newIndexer(ingressService))
	h.referenceGrants = staticGrants()
	h.directPeers = gatewayPeers

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	// HTTPS stays on the ingress service, and keeps its Ingress.
	got := ingressesIn(t, h, "apps")
	if len(got) != 1 {
		t.Fatalf("got %d Ingresses, want the HTTPS listener's only", len(got))
	}
	for _, ing := range got {
		if ing.Annotations["parent-listener"] != "https" {
			t.Errorf("the Ingress is listener %s's, which is served directly", ing.Annotations["parent-listener"])
		}
	}
	parent := ours(t, writtenRoute(t, h, route))
	checkCondition(t, parent, v1.RouteConditionAccepted, metav1.ConditionTrue, v1.RouteReasonAccepted, "has filters")
}

// The load-balancer manager reports an installed rule by its address, as an
// IP, or in a zone as a hostname; only the Service's own address counts.
func TestInstalled(t *testing.T) {
	svc := func(ing ...corev1.LoadBalancerIngress) *corev1.Service {
		s := &corev1.Service{Spec: corev1.ServiceSpec{LoadBalancerIP: "10.0.0.10"}}
		s.Status.LoadBalancer.Ingress = ing
		return s
	}
	cases := []struct {
		name string
		svc  *corev1.Service
		want bool
	}{
		{"nothing yet", svc(), false},
		{"by IP", svc(corev1.LoadBalancerIngress{IP: "10.0.0.10"}), true},
		{"by the zone's hostname", svc(corev1.LoadBalancerIngress{Hostname: "llb-10.0.0.10"}), true},
		{"another address", svc(corev1.LoadBalancerIngress{Hostname: "llb-10.0.0.11"}), false},
	}
	for _, tc := range cases {
		if got := installed(tc.svc, "llb"); got != tc.want {
			t.Errorf("%s: installed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The load-balancer manager writes the ingress Service's status too; a
// Gateway reconcile holding a stale copy reads it again instead of failing,
// which turned the Gateway's Programmed False until the next retry.
func TestIngressServiceUpdateRetriesAConflict(t *testing.T) {
	gw := directGateway()
	stale := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "gw-ingress-service"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http-81", Port: 81}}}}
	gm := gatewayTestManager(t, nil, []*v1.Gateway{gw}, stale)
	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer())

	conflicts := 1
	gm.kubeClient.(*fake.Clientset).PrependReactor("update", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, errors.NewConflict(schema.GroupResource{Resource: "services"}, "gw-ingress-service", fmt.Errorf("stale"))
		}
		return false, nil, nil
	})
	if _, err := gm.createIngressLbService(context.Background(), gw, nil); err != nil {
		t.Fatalf("createIngressLbService = %v, want the conflict retried", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ports = %v", got)
	}
}
