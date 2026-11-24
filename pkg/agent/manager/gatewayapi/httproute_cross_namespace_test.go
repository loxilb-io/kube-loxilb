package gatewayapi

import (
	"context"
	"strings"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// openWebGateway - webGateway, its listeners taking routes from any namespace.
func openWebGateway() *v1.Gateway {
	gw := webGateway()
	for i := range gw.Spec.Listeners {
		gw.Spec.Listeners[i].AllowedRoutes = fromNamespaces(v1.NamespacesFromAll, nil)
	}
	return gw
}

// appsRoute - webRoute, in namespace apps: another namespace than
// webGateway's, llm.
func appsRoute() *v1.HTTPRoute {
	route := webRoute()
	route.Namespace = "apps"
	route.Spec.ParentRefs = []v1.ParentReference{{Name: "web-gw", Namespace: namespacePtr("llm")}}
	return route
}

// routeIngress - an Ingress made for route web of namespace apps for a
// listener of web-gw, as this manager labels and annotates them.
func routeIngress(namespace, name, listener string, withRouteNamespace bool) *netv1.Ingress {
	ing := &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name,
		Labels: map[string]string{"implementation": implementation},
		Annotations: map[string]string{
			"gateway-api-controller": "loxilb.io/loxilb", "parent-http-route": "web",
			"parent-gateway": "web-gw", "parent-gateway-namespace": "llm", "parent-listener": listener,
		},
	}}
	if withRouteNamespace {
		ing.Annotations[routeNamespaceAnnotation] = "apps"
	}
	return ing
}

// A route in another namespace than its Gateway gets the Ingress of an HTTPS
// listener in the Gateway's namespace, where loxilb-ingress finds the
// listener's certificate, and names its backend's namespace on it. The HTTP
// listener's stays in the route's namespace, as it was. Both say whose they
// are.
func TestAnHTTPSIngressOfARouteInAnotherNamespaceIsInTheGateways(t *testing.T) {
	h := servingManager([]*v1.Gateway{openWebGateway()})
	route := appsRoute()

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}

	inGateway := ingressesIn(t, h, "llm")
	https, ok := inGateway[crossNamespaceIngressName("apps", "web", "llm", "web-gw", "https")]
	if len(inGateway) != 1 || !ok {
		t.Fatalf("Ingresses in the Gateway's namespace: %v, want the HTTPS listener's", inGateway)
	}
	if tls := https.Spec.TLS; len(tls) != 1 || tls[0].SecretName != "example-cert" {
		t.Errorf("TLS = %+v, want the listener's certificate", tls)
	}
	for key, want := range map[string]string{
		routeNamespaceAnnotation:   "apps",
		"external-backend-service": "true",
		"service-web-namespace":    "apps",
		"loxilb.io/epselect":       "hash",
		"parent-http-route":        "web",
	} {
		if got := https.Annotations[key]; got != want {
			t.Errorf("HTTPS Ingress annotation %s = %q, want %q", key, got, want)
		}
	}

	inRoute := ingressesIn(t, h, "apps")
	http, ok := inRoute[listenerIngressName("web", "llm", "web-gw", "http")]
	if len(inRoute) != 1 || !ok {
		t.Fatalf("Ingresses in the route's namespace: %v, want the HTTP listener's, under the name it had", inRoute)
	}
	if http.Annotations[routeNamespaceAnnotation] != "apps" {
		t.Errorf("HTTP Ingress %s = %q, want apps", routeNamespaceAnnotation, http.Annotations[routeNamespaceAnnotation])
	}
	if _, ok := http.Annotations["external-backend-service"]; ok {
		t.Errorf("the HTTP Ingress names its backend's namespace, which is its own: %v", http.Annotations)
	}
}

// A route in the Gateway's namespace keeps its Ingresses as they were: there,
// under their names, without a backend namespace.
func TestARouteInTheGatewaysNamespaceKeepsItsIngresses(t *testing.T) {
	h := servingManager([]*v1.Gateway{openWebGateway()})
	route := webRoute()
	route.Namespace = "llm"

	if err := h.syncHTTPRoute(route); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}
	got := ingressesIn(t, h, "llm")
	https, ok := got[listenerIngressName("web", "llm", "web-gw", "https")]
	if len(got) != 2 || !ok {
		t.Fatalf("Ingresses: %v, want both listeners', the HTTPS one under the name it had", got)
	}
	if _, ok := https.Annotations["external-backend-service"]; ok {
		t.Errorf("the HTTPS Ingress names a backend namespace: %v", https.Annotations)
	}
}

// What a route's annotations name is looked up by loxilb-ingress in the
// Ingress's namespace. On an Ingress in the Gateway's, the mTLS Secrets and
// the direct-loadbalance Service would be the Gateway's, so those are left
// out; the backend namespaces are this manager's to write, from what the
// grants permit, on any Ingress. The route's status says what was left out.
func TestAnIngressCarriesOnlyTheRouteAnnotationsItMay(t *testing.T) {
	h := servingManager([]*v1.Gateway{openWebGateway()})
	route := appsRoute()
	route.Annotations = map[string]string{
		"loxilb.io/epselect":                     "hash",
		"loxilb.io/mtls-frontend-mode":           "required",
		"loxilb.io/mtls-frontend-secret":         "client-ca",
		"loxilb.io/direct-loadbalance-service":   "other",
		"external-backend-service":               "true",
		"service-web-namespace":                  "secrets",
		"loxilb.io/direct-loadbalance-namespace": "secrets",
	}

	ingresses, parents, err := h.routeIngresses(route)
	if err != nil {
		t.Fatalf("routeIngresses: %v", err)
	}
	byListener := map[string]*netv1.Ingress{}
	for _, ing := range ingresses {
		byListener[ing.Annotations["parent-listener"]] = ing
	}
	https, http := byListener["https"], byListener["http"]
	if https == nil || http == nil {
		t.Fatalf("Ingresses: %v, want one per listener", byListener)
	}

	for _, key := range []string{"loxilb.io/mtls-frontend-mode", "loxilb.io/mtls-frontend-secret",
		"loxilb.io/direct-loadbalance-service", "loxilb.io/direct-loadbalance-namespace"} {
		if _, ok := https.Annotations[key]; ok {
			t.Errorf("the Ingress in the Gateway's namespace carries %s", key)
		}
		if _, ok := http.Annotations[key]; !ok {
			t.Errorf("the Ingress in the route's namespace lost %s", key)
		}
	}
	if got := https.Annotations["service-web-namespace"]; got != "apps" {
		t.Errorf("HTTPS Ingress service-web-namespace = %q, want the backendRef's, apps", got)
	}
	if _, ok := http.Annotations["service-web-namespace"]; ok {
		t.Errorf("the HTTP Ingress carries the route's service-web-namespace: %v", http.Annotations)
	}
	if _, ok := http.Annotations["external-backend-service"]; ok {
		t.Errorf("the HTTP Ingress carries the route's external-backend-service: %v", http.Annotations)
	}
	if https.Annotations["loxilb.io/epselect"] != "hash" || http.Annotations["loxilb.io/epselect"] != "hash" {
		t.Errorf("epselect is not on both Ingresses")
	}

	if len(parents) != 1 {
		t.Fatalf("parents: %+v", parents)
	}
	notes := strings.Join(parents[0].Notes, " | ")
	if !strings.Contains(notes, "listener https: annotations external-backend-service, loxilb.io/direct-loadbalance-namespace, "+
		"loxilb.io/direct-loadbalance-service, loxilb.io/mtls-frontend-mode, loxilb.io/mtls-frontend-secret, service-web-namespace are not carried") {
		t.Errorf("notes %q do not name what the HTTPS Ingress left out", notes)
	}
	if !strings.Contains(notes, "listener http: annotations external-backend-service, service-web-namespace are not carried") {
		t.Errorf("notes %q do not name what the HTTP Ingress left out", notes)
	}
}

// loxilb-ingress reads a backend's namespace from the Ingress by the Service's
// name, so two backends of one name in different namespaces cannot share an
// Ingress: the second would be read as the first, on either listener. It is
// left out, and said.
func TestBackendsOfOneNameInTwoNamespacesAreNotMixed(t *testing.T) {
	h := servingManager([]*v1.Gateway{openWebGateway()})
	h.referenceGrants = staticGrants(referenceGrant("shared", "from-apps", grantSpec(kindHTTPRoute, "apps", "")))
	route := appsRoute()
	shared := backendRef("", "", "web")
	ns := v1.Namespace("shared")
	shared.Namespace = &ns
	prefix, path := v1.PathMatchPathPrefix, "/shared"
	route.Spec.Rules = append(route.Spec.Rules, v1.HTTPRouteRule{
		Matches:     []v1.HTTPRouteMatch{{Path: &v1.HTTPPathMatch{Type: &prefix, Value: &path}}},
		BackendRefs: []v1.HTTPBackendRef{shared},
	})

	ingresses, parents, err := h.routeIngresses(route)
	if err != nil {
		t.Fatalf("routeIngresses: %v", err)
	}
	if len(ingresses) != 2 {
		t.Fatalf("Ingresses: %d, want one per listener", len(ingresses))
	}
	for _, ing := range ingresses {
		listener := ing.Annotations["parent-listener"]
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				if path.Path == "/shared" {
					t.Errorf("listener %s's Ingress has shared/web at /shared, which it would read as apps/web", listener)
				}
			}
		}
		if got, ok := ing.Annotations["service-web-namespace"]; ok && got != "apps" {
			t.Errorf("listener %s's Ingress names web's namespace %q", listener, got)
		}
	}
	notes := strings.Join(parents[0].Notes, " | ")
	for _, listener := range []string{"http", "https"} {
		if !strings.Contains(notes, "listener "+listener+": backendRef shared/web is left out") {
			t.Errorf("notes %q do not say listener %s left shared/web out", notes, listener)
		}
	}
}

// A route's Ingresses are found where they are: in its namespace, with or
// without the route's namespace on them, and in a Gateway's with it. Another
// route's of the same name are not its, whether they say their namespace or
// are in it.
func TestOwnedIngressesAreFoundWhereverTheyAre(t *testing.T) {
	legacy := routeIngress("apps", "web-old", "http", false)
	legacy.Labels = nil // made before the label was there
	moved := routeIngress("llm", "apps-web-1", "https", true)
	other := routeIngress("llm", "team-web-1", "https", false)
	other.Annotations[routeNamespaceAnnotation] = "team"
	gatewaysOwn := routeIngress("llm", "web-1", "http", false) // route llm/web's, in its namespace
	h := servingManager(nil, legacy, moved, other, gatewaysOwn)

	owned, err := h.ownedIngresses(context.Background(), "apps", "web")
	if err != nil {
		t.Fatalf("ownedIngresses: %v", err)
	}
	if len(owned) != 2 || owned[GenKey("apps", "web-old")] == nil || owned[GenKey("llm", "apps-web-1")] == nil {
		t.Errorf("owned = %v, want apps/web-old and llm/apps-web-1", keysOf(owned))
	}
}

// On an upgrade, the HTTPS listener's Ingress of a route in another
// namespace moves from the route's namespace to the Gateway's. The old one
// goes first: loxilb-ingress deletes a deleted Ingress's hosts' certificates,
// and would take the new one's with it.
func TestAMovedIngressGoesBeforeItsReplacementComes(t *testing.T) {
	old := routeIngress("apps", listenerIngressName("web", "llm", "web-gw", "https"), "https", false)
	h := servingManager([]*v1.Gateway{openWebGateway()}, old)

	if err := h.syncHTTPRoute(appsRoute()); err != nil {
		t.Fatalf("syncHTTPRoute: %v", err)
	}

	deleted, created := -1, -1
	for i, action := range h.kubeClient.(*fake.Clientset).Actions() {
		if action.GetResource().Resource != "ingresses" {
			continue
		}
		switch {
		case action.GetVerb() == "delete" && action.GetNamespace() == "apps":
			deleted = i
		case action.GetVerb() == "create" && action.GetNamespace() == "llm":
			created = i
		}
	}
	if deleted < 0 || created < 0 || deleted > created {
		t.Errorf("delete of the old Ingress at action %d, create of the new at %d; want the delete first", deleted, created)
	}
	if got := ingressesIn(t, h, "apps"); got[old.Name].Name != "" {
		t.Errorf("the old HTTPS Ingress is still in the route's namespace")
	}
	if got := ingressesIn(t, h, "llm"); len(got) != 1 {
		t.Errorf("Ingresses in the Gateway's namespace: %v, want the new HTTPS one", got)
	}
}

// Deleting the route deletes its Ingress in the Gateway's namespace too, and
// a route that leaves the Gateway leaves nothing there.
func TestARoutesIngressInTheGatewaysNamespaceGoesWithIt(t *testing.T) {
	t.Run("the route is deleted", func(t *testing.T) {
		h := servingManager([]*v1.Gateway{openWebGateway()})
		if err := h.syncHTTPRoute(appsRoute()); err != nil {
			t.Fatalf("syncHTTPRoute: %v", err)
		}
		if err := h.deleteHTTPRoute("apps", "web"); err != nil {
			t.Fatalf("deleteHTTPRoute: %v", err)
		}
		if got := ingressesIn(t, h, "llm"); len(got) != 0 {
			t.Errorf("Ingresses left in the Gateway's namespace: %v", got)
		}
		if got := ingressesIn(t, h, "apps"); len(got) != 0 {
			t.Errorf("Ingresses left in the route's namespace: %v", got)
		}
	})

	t.Run("the route leaves the Gateway", func(t *testing.T) {
		h := servingManager([]*v1.Gateway{openWebGateway()})
		route := appsRoute()
		if err := h.syncHTTPRoute(route); err != nil {
			t.Fatalf("syncHTTPRoute: %v", err)
		}
		route.Spec.ParentRefs = []v1.ParentReference{{Name: "elsewhere", Namespace: namespacePtr("llm")}}
		if err := h.syncHTTPRoute(route); err != nil {
			t.Fatalf("second syncHTTPRoute: %v", err)
		}
		if got := ingressesIn(t, h, "llm"); len(got) != 0 {
			t.Errorf("Ingresses left in the Gateway's namespace: %v", got)
		}
	})
}

// The name of an Ingress in a Gateway's namespace has the route's namespace
// in it: routes of one name from two namespaces get two Ingresses.
func TestCrossNamespaceIngressName(t *testing.T) {
	a := crossNamespaceIngressName("team-a", "web", "runai", "gw", "https")
	b := crossNamespaceIngressName("team-b", "web", "runai", "gw", "https")
	if a == b {
		t.Errorf("routes web of team-a and team-b get one name, %s", a)
	}
	if !strings.HasPrefix(a, "team-a-web-") {
		t.Errorf("name %s does not start with the route's namespace and name", a)
	}
	long := crossNamespaceIngressName(strings.Repeat("n", 63), strings.Repeat("r", 253), "runai", "gw", "https")
	if len(long) > validation.DNS1123SubdomainMaxLength {
		t.Errorf("name of %d characters, over the limit", len(long))
	}
	if errs := validation.IsDNS1123Subdomain(long); len(errs) > 0 {
		t.Errorf("name %s: %v", long, errs)
	}
}

func keysOf(m map[string]*netv1.Ingress) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
