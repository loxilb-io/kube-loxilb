package gatewayapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	corev1lister "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

const testClass = "loxilb-gc"

func gatewayClass(name, controller string) *v1.GatewayClass {
	return &v1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.GatewayClassSpec{ControllerName: v1.GatewayController(controller)},
	}
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// servingManager - a manager that serves gateways of testClass, with the
// namespaces llm and apps (labelled team=apps).
func servingManager(gateways []*v1.Gateway, ingresses ...*netv1.Ingress) *HTTPRouteManager {
	h := withRouteObjects(httpRouteTestManager(ingresses...), nil, gateways...)
	h.gatewayClassLister = sigs_v1_lister.NewGatewayClassLister(newIndexer(
		gatewayClass(testClass, "loxilb.io/loxilb"),
		gatewayClass("other-gc", "example.com/other"),
	))
	h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(
		namespace("llm", nil),
		namespace("apps", map[string]string{"team": "apps"}),
	))
	return h
}

func withCertificate(l v1.Listener, secret string) v1.Listener {
	l.TLS = &v1.ListenerTLSConfig{CertificateRefs: []v1.SecretObjectReference{{Name: v1.ObjectName(secret)}}}
	return l
}

// webGateway - an HTTP listener for any host and an HTTPS one for
// *.example.com, both taking routes from their own namespace only.
func webGateway() *v1.Gateway {
	return &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "web-gw", Generation: 1},
		Spec: v1.GatewaySpec{
			GatewayClassName: testClass,
			Listeners: []v1.Listener{
				listener("http", v1.HTTPProtocolType, 80, ""),
				withCertificate(listener("https", v1.HTTPSProtocolType, 443, "*.example.com"), "example-cert"),
			},
		},
	}
}

// webRoute - shop.example.com to Service web on webGateway, by its name only.
func webRoute() *v1.HTTPRoute {
	route := routeWith([]v1.HTTPBackendRef{backendRef("", "", "web")})
	route.Name = "web"
	route.Annotations = map[string]string{"loxilb.io/epselect": "hash"}
	route.Spec.ParentRefs = []v1.ParentReference{{Name: "web-gw"}}
	route.Spec.Hostnames = hostnames("shop.example.com")
	return route
}

func ingressesIn(t *testing.T, h *HTTPRouteManager, namespace string) map[string]netv1.Ingress {
	t.Helper()
	list, err := h.kubeClient.NetworkingV1().Ingresses(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ingresses: %v", err)
	}
	got := map[string]netv1.Ingress{}
	for _, ing := range list.Items {
		got[ing.Name] = ing
	}
	return got
}

func ingressActions(h *HTTPRouteManager, verb string) int {
	n := 0
	for _, action := range h.kubeClient.(*fake.Clientset).Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "ingresses" {
			n++
		}
	}
	return n
}

func ruleHosts(ing netv1.Ingress) []string {
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts
}

func ownedIngress(name, route string) *netv1.Ingress {
	return &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "llm",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-http-route": route},
	}}
}

// A route without a sectionName attaches to every listener that takes it, and
// each gets an Ingress of its own: the HTTP one serves the host on port 80,
// the HTTPS one with the listener's certificate on 443. Such a route used to
// get no Ingress at all, and a second listener's collided with the first's
// name and was dropped.
func TestSyncHTTPRouteGivesEachListenerAnIngress(t *testing.T) {
	h := servingManager([]*v1.Gateway{webGateway()})
	route := webRoute()

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}

	got := ingressesIn(t, h, "llm")
	if len(got) != 2 {
		t.Fatalf("got Ingresses %v, want one for each listener", len(got))
	}

	httpIng, ok := got[listenerIngressName("web", "llm", "web-gw", "http")]
	if !ok {
		t.Fatalf("no Ingress for listener http among %v", got)
	}
	if hosts := ruleHosts(httpIng); len(hosts) != 1 || hosts[0] != "shop.example.com" {
		t.Errorf("http Ingress hosts = %v, want [shop.example.com]", hosts)
	}
	if len(httpIng.Spec.TLS) != 0 {
		t.Errorf("http Ingress has TLS %v, want none", httpIng.Spec.TLS)
	}

	httpsIng, ok := got[listenerIngressName("web", "llm", "web-gw", "https")]
	if !ok {
		t.Fatalf("no Ingress for listener https among %v", got)
	}
	if tls := httpsIng.Spec.TLS; len(tls) != 1 || tls[0].SecretName != "example-cert" ||
		len(tls[0].Hosts) != 1 || tls[0].Hosts[0] != "shop.example.com" {
		t.Errorf("https Ingress TLS = %+v, want example-cert for shop.example.com", tls)
	}

	for name, want := range map[string]string{
		"gateway-api-controller":   "loxilb.io/loxilb",
		"parent-gateway":           "web-gw",
		"parent-gateway-namespace": "llm",
		"parent-listener":          "https",
		"parent-http-route":        "web",
		"loxilb.io/epselect":       "hash",
	} {
		if got := httpsIng.Annotations[name]; got != want {
			t.Errorf("annotation %s = %q, want %q", name, got, want)
		}
	}

	// The route is the informer's; what the Ingress carries must not end up
	// in it.
	if len(route.Annotations) != 1 {
		t.Errorf("route annotations = %v, want only its own", route.Annotations)
	}
}

// A sync that finds the Ingresses as they should be writes nothing, and one
// after the route changed updates them in place.
func TestSyncHTTPRouteUpdatesInPlace(t *testing.T) {
	h := servingManager([]*v1.Gateway{webGateway()})
	route := webRoute()
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	h.kubeClient.(*fake.Clientset).ClearActions()
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for _, verb := range []string{"create", "update", "delete"} {
		if n := ingressActions(h, verb); n != 0 {
			t.Errorf("a sync with nothing changed made %d %s calls", n, verb)
		}
	}

	route.Spec.Rules[0].BackendRefs = []v1.HTTPBackendRef{backendRef("", "", "web-v2")}
	h.kubeClient.(*fake.Clientset).ClearActions()
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("sync after the change: %v", err)
	}
	if c, u := ingressActions(h, "create"), ingressActions(h, "update"); c != 0 || u != 2 {
		t.Errorf("made %d creates and %d updates, want 0 and 2", c, u)
	}
	for name, ing := range ingressesIn(t, h, "llm") {
		if backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name; backend != "web-v2" {
			t.Errorf("Ingress %s still points at %s", name, backend)
		}
	}
}

// The Ingress of a listener the route no longer attaches to goes, as does one
// named for the route alone, which is what earlier builds made. Another
// route's stays.
func TestSyncHTTPRouteRemovesWhatTheRouteLeft(t *testing.T) {
	h := servingManager([]*v1.Gateway{webGateway()}, ownedIngress("web", "web"), ownedIngress("shop", "shop"))
	route := webRoute()
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	got := ingressesIn(t, h, "llm")
	if _, ok := got["web"]; ok {
		t.Error("the Ingress named for the route alone is still there")
	}
	if _, ok := got["shop"]; !ok {
		t.Error("another route's Ingress was removed")
	}

	section := v1.SectionName("http")
	route.Spec.ParentRefs[0].SectionName = &section
	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("sync after leaving https: %v", err)
	}
	got = ingressesIn(t, h, "llm")
	if _, ok := got[listenerIngressName("web", "llm", "web-gw", "https")]; ok {
		t.Error("the Ingress of the listener the route left is still there")
	}
	if _, ok := got[listenerIngressName("web", "llm", "web-gw", "http")]; !ok {
		t.Error("the Ingress of the listener the route kept is gone")
	}

	// Deleting the route takes the rest.
	if err := h.reconcile("llm", "web"); err != nil {
		t.Fatalf("reconcile of the deleted route: %v", err)
	}
	got = ingressesIn(t, h, "llm")
	if len(got) != 1 {
		t.Errorf("Ingresses left after the route went: %v, want only another route's", len(got))
	}
}

// A failure to write an Ingress used to be logged and dropped, and the route
// was never tried again. It is returned now so the route is retried, and the
// Ingresses left over stay until the rest are in place.
func TestSyncHTTPRouteReportsFailures(t *testing.T) {
	t.Run("the API server refuses", func(t *testing.T) {
		h := servingManager([]*v1.Gateway{webGateway()}, ownedIngress("web", "web"))
		h.kubeClient.(*fake.Clientset).PrependReactor("create", "ingresses",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("refused")
			})

		if err := h.syncHTTPRoute(webRoute()); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("syncHTTPRoute = %v, want the refusal", err)
		}
		if _, ok := ingressesIn(t, h, "llm")["web"]; !ok {
			t.Error("the old Ingress was removed before its replacement was in place")
		}
	})

	t.Run("an Ingress of that name is someone else's", func(t *testing.T) {
		name := listenerIngressName("web", "llm", "web-gw", "http")
		foreign := &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"}}
		h := servingManager([]*v1.Gateway{webGateway()}, foreign)

		err := h.syncHTTPRoute(webRoute())
		if err == nil || !strings.Contains(err.Error(), "not made for the route") {
			t.Fatalf("syncHTTPRoute = %v, want the name taken", err)
		}
		if ing := ingressesIn(t, h, "llm")[name]; len(ing.Spec.Rules) != 0 || len(ing.Annotations) != 0 {
			t.Errorf("someone else's Ingress was overwritten: %+v", ing)
		}
	})
}

// Only a Gateway of this controller's class is served, and only through the
// listeners that take the route.
func TestSyncHTTPRouteServesOnlyWhatItAttachesTo(t *testing.T) {
	cases := []struct {
		name    string
		gateway func(*v1.Gateway)
		route   func(*v1.HTTPRoute)
		want    int
	}{
		{name: "this controller's class", want: 2},
		{
			name:    "another controller's class",
			gateway: func(gw *v1.Gateway) { gw.Spec.GatewayClassName = "other-gc" },
		},
		{
			name:    "a class that does not exist yet",
			gateway: func(gw *v1.Gateway) { gw.Spec.GatewayClassName = "missing-gc" },
		},
		{
			name:  "a route from another namespace, by default",
			route: func(r *v1.HTTPRoute) { r.Namespace = "apps"; r.Spec.ParentRefs[0].Namespace = namespacePtr("llm") },
		},
		{
			name: "a route from a namespace the listener selects",
			gateway: func(gw *v1.Gateway) {
				gw.Spec.Listeners[0].AllowedRoutes = fromNamespaces(v1.NamespacesFromSelector,
					&metav1.LabelSelector{MatchLabels: map[string]string{"team": "apps"}})
			},
			route: func(r *v1.HTTPRoute) { r.Namespace = "apps"; r.Spec.ParentRefs[0].Namespace = namespacePtr("llm") },
			want:  1,
		},
		{
			name:  "a hostname neither listener has",
			route: func(r *v1.HTTPRoute) { r.Spec.Hostnames = hostnames("shop.example.org") },
			want:  1, // the HTTP listener takes any host
		},
		{
			name: "a port that selects the HTTPS listener",
			route: func(r *v1.HTTPRoute) {
				port := v1.PortNumber(443)
				r.Spec.ParentRefs[0].Port = &port
			},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := webGateway()
			if tc.gateway != nil {
				tc.gateway(gw)
			}
			route := webRoute()
			if tc.route != nil {
				tc.route(route)
			}
			h := servingManager([]*v1.Gateway{gw})

			if err := h.syncHTTPRoute(route); err != nil {
				t.Fatalf("syncHTTPRoute: %v", err)
			}
			if got := len(ingressesIn(t, h, route.Namespace)); got != tc.want {
				t.Errorf("got %d Ingresses, want %d", got, tc.want)
			}
		})
	}
}

// loxilb-ingress terminates TLS only for a host the Ingress has a certificate
// for, and serves anything else in plain HTTP. An HTTPS listener that cannot
// give it both gets no Ingress, rather than one that answers unencrypted.
func TestListenerIngressOnHTTPS(t *testing.T) {
	route := webRoute()
	route.Spec.Hostnames = nil

	t.Run("no hostname on either side", func(t *testing.T) {
		gw := webGateway()
		gw.Spec.Listeners[1].Hostname = nil
		la := listenerAttachment{Listener: &gw.Spec.Listeners[1]}
		ing, unserved, _ := servingManager(nil).listenerIngress(route, gw, la)
		if ing != nil {
			t.Errorf("got an Ingress with rules for hosts %v", ruleHosts(*ing))
		}
		if !strings.Contains(unserved, "names a hostname") {
			t.Errorf("unserved = %q, want why", unserved)
		}
	})

	t.Run("no Secret to terminate with", func(t *testing.T) {
		gw := webGateway()
		configMap := v1.Kind("ConfigMap")
		gw.Spec.Listeners[1].TLS.CertificateRefs[0].Kind = &configMap
		la := listenerAttachment{Listener: &gw.Spec.Listeners[1], Hostnames: hostnames("shop.example.com")}
		ing, unserved, _ := servingManager(nil).listenerIngress(route, gw, la)
		if ing != nil {
			t.Errorf("got an Ingress with TLS %v", ing.Spec.TLS)
		}
		if !strings.Contains(unserved, "no Secret") {
			t.Errorf("unserved = %q, want why", unserved)
		}
	})

	t.Run("HTTP for any host", func(t *testing.T) {
		gw := webGateway()
		la := listenerAttachment{Listener: &gw.Spec.Listeners[0]}
		ing, unserved, _ := servingManager(nil).listenerIngress(route, gw, la)
		if ing == nil || unserved != "" {
			t.Fatalf("no Ingress for an HTTP listener serving any host (%q)", unserved)
		}
		if hosts := ruleHosts(*ing); len(hosts) != 1 || hosts[0] != "" {
			t.Errorf("rule hosts = %q, want one rule for any host", hosts)
		}
	})
}

// On the Run:ai Gateway, three HTTPS listeners share port 443. An inference
// route attaches to both wildcard listeners, but its requests reach only the
// more specific one, so that is the only one with an Ingress - and with it
// the only certificate loxilb-ingress has for those hosts. The route is in
// another namespace than the Gateway, so the Ingress is in the Gateway's,
// where its certificate is.
func TestSyncHTTPRouteFollowsListenerPrecedence(t *testing.T) {
	gw := runaiGateway()
	gw.Spec.GatewayClassName = testClass
	for i := range gw.Spec.Listeners {
		gw.Spec.Listeners[i] = withCertificate(gw.Spec.Listeners[i], string(gw.Spec.Listeners[i].Name)+"-cert")
	}
	h := servingManager([]*v1.Gateway{gw})
	h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("team-a", nil)))

	route := webRoute()
	route.Namespace = "team-a"
	route.Spec.ParentRefs = []v1.ParentReference{{Name: "gw", Namespace: namespacePtr("runai")}}
	route.Spec.Hostnames = hostnames("*.runai-inference.cluster.example.com")

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	if got := ingressesIn(t, h, "team-a"); len(got) != 0 {
		t.Errorf("Ingresses in the route's namespace: %v, want none", got)
	}
	got := ingressesIn(t, h, "runai")
	if len(got) != 1 {
		t.Fatalf("got %d Ingresses, want only the inference listener's", len(got))
	}
	ing, ok := got[crossNamespaceIngressName("team-a", "web", "runai", "gw", "https-inference")]
	if !ok {
		t.Fatalf("the Ingress is not the inference listener's: %v", got)
	}
	if tls := ing.Spec.TLS; len(tls) != 1 || tls[0].SecretName != "https-inference-cert" {
		t.Errorf("TLS = %+v, want the inference listener's certificate", tls)
	}
}

// A route that only names InferencePools has nothing for an Ingress to carry;
// creating one anyway was refused by the API server on every reconcile. One
// left from when the route still named a Service goes.
func TestSyncHTTPRouteSkipsInferencePoolOnlyRoutes(t *testing.T) {
	h := servingManager([]*v1.Gateway{webGateway()},
		ownedIngress("web", "web"), ownedIngress("unrelated", "another-route"))
	route := webRoute()
	route.Spec.Rules[0].BackendRefs = []v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "vllm-qwen3")}

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	if got := ingressCreates(h); got != 0 {
		t.Errorf("tried to create %d Ingresses for a route with only InferencePool backends", got)
	}
	got := ingressesIn(t, h, "llm")
	if _, ok := got["web"]; ok {
		t.Error("the route's old Ingress is still there")
	}
	if _, ok := got["unrelated"]; !ok {
		t.Error("another route's Ingress was removed")
	}
}

// In a route that mixes the two, the InferencePool rule is left out instead
// of becoming an IngressRule without paths, which would invalidate the
// whole Ingress.
func TestListenerIngressLeavesOutInferencePoolRules(t *testing.T) {
	route := routeWith(
		[]v1.HTTPBackendRef{backendRef(inferencePoolGroup, inferencePoolKind, "vllm-qwen3")},
		[]v1.HTTPBackendRef{backendRef("", "", "web")},
	)
	route.Spec.Hostnames = hostnames("llm.example.com")
	gw := webGateway()

	ing, _, _ := servingManager(nil).listenerIngress(route, gw,
		listenerAttachment{Listener: &gw.Spec.Listeners[0], Hostnames: route.Spec.Hostnames})
	if ing == nil {
		t.Fatal("no Ingress for the Service rule")
	}
	if len(ing.Spec.Rules) != 1 {
		t.Fatalf("got %d IngressRules, want 1 for the Service rule only", len(ing.Spec.Rules))
	}
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 || paths[0].Backend.Service.Name != "web" {
		t.Errorf("paths = %+v, want the one Service backend", paths)
	}
}

func TestListenerIngressName(t *testing.T) {
	base := listenerIngressName("web", "llm", "gw", "http")
	if again := listenerIngressName("web", "llm", "gw", "http"); again != base {
		t.Errorf("the name changed between calls: %s, %s", base, again)
	}
	if !strings.HasPrefix(base, "web-") {
		t.Errorf("name %s does not start with the route's", base)
	}
	for _, other := range []string{
		listenerIngressName("web", "llm", "gw", "https"),
		listenerIngressName("web", "llm", "gw2", "http"),
		listenerIngressName("web", "apps", "gw", "http"),
		listenerIngressName("shop", "llm", "gw", "http"),
	} {
		if other == base {
			t.Errorf("two listeners share the name %s", base)
		}
	}

	long := listenerIngressName(strings.Repeat("a", 250)+".b", "llm", "gw", "http")
	if errs := validation.IsDNS1123Subdomain(long); len(errs) != 0 {
		t.Errorf("the name of a long route is not valid: %v", errs)
	}
}

func TestEnqueueClassRoutes(t *testing.T) {
	ours, theirs := webGateway(), webGateway()
	theirs.Name, theirs.Spec.GatewayClassName = "their-gw", "other-gc"
	routes := []*v1.HTTPRoute{
		routeNaming("llm", "ours", v1.ParentReference{Name: "web-gw"}),
		routeNaming("llm", "theirs", v1.ParentReference{Name: "their-gw"}),
	}
	h := withRouteObjects(httpRouteTestManager(), routes, ours, theirs)

	h.enqueueClassRoutes(gatewayClass(testClass, "loxilb.io/loxilb"))
	want := HTTPRouteQueueEntry{"llm", "ours"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("the class queued %v, want only %v", got, want)
	}

	h.enqueueClassRoutes(tombstone(gatewayClass(testClass, "loxilb.io/loxilb")))
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("the class's tombstone queued %v, want only %v", got, want)
	}
}

func TestNamespaceUpdated(t *testing.T) {
	routes := []*v1.HTTPRoute{
		routeNaming("apps", "in-apps", v1.ParentReference{Name: "gw"}),
		routeNaming("llm", "in-llm", v1.ParentReference{Name: "gw"}),
	}
	h := withRouteObjects(httpRouteTestManager(), routes)

	old := namespace("apps", map[string]string{"team": "apps"})
	h.namespaceUpdated(old, namespace("apps", map[string]string{"team": "apps"}))
	if got := drained(h.queue); len(got) != 0 {
		t.Errorf("an update without a label change queued %v", got)
	}

	h.namespaceUpdated(old, namespace("apps", map[string]string{"team": "other"}))
	want := HTTPRouteQueueEntry{"apps", "in-apps"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("a label change queued %v, want only %v", got, want)
	}
}

// A backendRef into another namespace is served only while a ReferenceGrant
// there permits it. It used to be served on the route's word alone.
func TestSyncHTTPRouteFollowsReferenceGrants(t *testing.T) {
	route := webRoute()
	section := v1.SectionName("http")
	route.Spec.ParentRefs[0].SectionName = &section
	route.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
	name := listenerIngressName("web", "llm", "web-gw", "http")

	t.Run("without a grant", func(t *testing.T) {
		h := servingManager([]*v1.Gateway{webGateway()})
		h.referenceGrants = staticGrants()
		if err := h.syncHTTPRoute(route); err != nil {
			t.Fatalf("syncHTTPRoute: %v", err)
		}
		if got := ingressesIn(t, h, "llm"); len(got) != 0 {
			t.Errorf("got %d Ingresses for a backend no grant permits", len(got))
		}
	})

	t.Run("with a grant, then without", func(t *testing.T) {
		h := servingManager([]*v1.Gateway{webGateway()})
		h.referenceGrants = staticGrants(referenceGrant("backends", "web", grantSpec(kindHTTPRoute, "llm", "web")))
		if err := h.syncHTTPRoute(route); err != nil {
			t.Fatalf("syncHTTPRoute: %v", err)
		}
		ing, ok := ingressesIn(t, h, "llm")[name]
		if !ok {
			t.Fatal("no Ingress for a backend a grant permits")
		}
		if ing.Annotations["external-backend-service"] != "true" || ing.Annotations["service-web-namespace"] != "backends" {
			t.Errorf("the Ingress does not say where the backend is: %v", ing.Annotations)
		}

		// The grant is withdrawn, and the route queued by it: the backend
		// goes with it.
		h.referenceGrants = staticGrants()
		if err := h.syncHTTPRoute(route); err != nil {
			t.Fatalf("syncHTTPRoute after the grant went: %v", err)
		}
		if _, ok := ingressesIn(t, h, "llm")[name]; ok {
			t.Error("the Ingress outlived the grant")
		}
	})
}

func TestReferenceGrantChangedQueuesItsRoutes(t *testing.T) {
	routes := []*v1.HTTPRoute{
		routeNaming("apps", "in-apps", v1.ParentReference{Name: "gw"}),
		routeNaming("llm", "in-llm", v1.ParentReference{Name: "gw"}),
		routeNaming("tcp", "in-tcp", v1.ParentReference{Name: "gw"}),
	}
	h := withRouteObjects(httpRouteTestManager(), routes)

	spec := grantSpec(kindHTTPRoute, "apps", "")
	spec.From = append(spec.From, v1.ReferenceGrantFrom{Group: v1.GroupName, Kind: kindTCPRoute, Namespace: "tcp"})
	h.referenceGrantChanged(spec)

	want := HTTPRouteQueueEntry{"apps", "in-apps"}
	if got := drained(h.queue); len(got) != 1 || !got[want] {
		t.Errorf("the grant queued %v, want only %v", got, want)
	}
}

// What the loxilb-ingress pods write on a route's Ingress stays: it is not a
// difference to write the Ingress over, and an update keeps it.
func TestAnIngressUpdateKeepsWhatLoxilbIngressWrote(t *testing.T) {
	desired := &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "shop-1", Annotations: map[string]string{"parent-http-route": "shop"}},
		Spec:       netv1.IngressSpec{DefaultBackend: &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "web", Port: netv1.ServiceBackendPort{Number: 80}}}},
	}
	existing := desired.DeepCopy()
	existing.Annotations[programmedAnnotationPrefix+"lx-a"] = "1"
	h := httpRouteTestManager(existing)
	route := &v1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "shop"}}

	if err := h.applyIngress(context.Background(), route, desired, existing); err != nil {
		t.Fatal(err)
	}
	if n := len(h.kubeClient.(*fake.Clientset).Actions()); n != 0 {
		t.Errorf("%d writes for an Ingress that differs only by a report", n)
	}

	changed := desired.DeepCopy()
	changed.Spec.DefaultBackend.Service.Port.Number = 8080
	if err := h.applyIngress(context.Background(), route, changed, existing); err != nil {
		t.Fatal(err)
	}
	got, _ := h.kubeClient.NetworkingV1().Ingresses("apps").Get(context.Background(), "shop-1", metav1.GetOptions{})
	if got.Spec.DefaultBackend.Service.Port.Number != 8080 || got.Annotations[programmedAnnotationPrefix+"lx-a"] != "1" {
		t.Errorf("updated Ingress: port %d, annotations %v; want the new port and lx-a's report", got.Spec.DefaultBackend.Service.Port.Number, got.Annotations)
	}
}
