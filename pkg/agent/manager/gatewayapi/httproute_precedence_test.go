package gatewayapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

var routeEpoch = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

// sharingRoute - a route of namespace llm for shop.example.com on web-gw's
// listeners, made minutes after routeEpoch, with one rule per path to
// backend. A path ending in "=" is an Exact match.
func sharingRoute(name string, minutes int, backend string, paths ...string) *v1.HTTPRoute {
	route := webRoute()
	route.Name = name
	route.CreationTimestamp = metav1.NewTime(routeEpoch.Add(time.Duration(minutes) * time.Minute))
	route.Spec.Rules = nil
	for _, p := range paths {
		matchType := v1.PathMatchPathPrefix
		if strings.HasSuffix(p, "=") {
			matchType, p = v1.PathMatchExact, strings.TrimSuffix(p, "=")
		}
		value := p
		route.Spec.Rules = append(route.Spec.Rules, v1.HTTPRouteRule{
			Matches:     []v1.HTTPRouteMatch{{Path: &v1.HTTPPathMatch{Type: &matchType, Value: &value}}},
			BackendRefs: []v1.HTTPBackendRef{backendRef("", "", backend)},
		})
	}
	return route
}

// precedenceManager - servingManager with routes in its lister.
func precedenceManager(gateways []*v1.Gateway, routes ...*v1.HTTPRoute) *HTTPRouteManager {
	h := servingManager(gateways)
	return withRouteObjects(h, routes, gateways...)
}

// servedPaths - host|path -> backend, of the Ingresses routeIngresses gives
// route.
func servedPaths(t *testing.T, h *HTTPRouteManager, route *v1.HTTPRoute) (map[string]string, []parentAttachment) {
	t.Helper()
	ings, parents, err := h.routeIngresses(route)
	if err != nil {
		t.Fatalf("routeIngresses: %v", err)
	}
	got := map[string]string{}
	for _, ing := range ings {
		for _, rule := range ing.Spec.Rules {
			for _, p := range rule.HTTP.Paths {
				got[ing.Annotations["parent-listener"]+" "+rule.Host+"|"+p.Path] = p.Backend.Service.Name
			}
		}
	}
	return got, parents
}

// Two routes asking for one host and path give their Ingresses to
// loxilb-ingress, which keeps one rule for the pair and hands it from one to
// the other. The older route has the pair; the younger one's Ingress leaves it
// out, and with nothing else to serve, the route says why it is not accepted.
func TestTheOlderRouteHasAPairItSharesWithAYoungerOne(t *testing.T) {
	old := sharingRoute("old", 0, "web-a", "/")
	young := sharingRoute("young", 1, "web-b", "/")
	h := precedenceManager([]*v1.Gateway{webGateway()}, old, young)

	got, _ := servedPaths(t, h, old)
	want := map[string]string{"http shop.example.com|/": "web-a", "https shop.example.com|/": "web-a"}
	if !mapsEqual(got, want) {
		t.Errorf("older route serves %v, want %v", got, want)
	}

	got, parents := servedPaths(t, h, young)
	if len(got) != 0 {
		t.Errorf("younger route serves %v, want nothing", got)
	}
	accepted, reason, message := parentAccepted(parents[0])
	if accepted || reason != string(v1.RouteReasonUnsupportedValue) ||
		!strings.Contains(message, `host "shop.example.com" and path "/" are served for route llm/old`) {
		t.Errorf("younger route Accepted = %v/%s %q, want False/UnsupportedValue naming llm/old", accepted, reason, message)
	}
}

// The spec weighs an Exact match before the age of its route, and routes of
// one age by namespace and name.
func TestPairPrecedenceFollowsTheSpec(t *testing.T) {
	cases := []struct {
		name   string
		routes []*v1.HTTPRoute
		winner string
	}{
		{"an exact match before an older prefix",
			[]*v1.HTTPRoute{sharingRoute("old", 0, "web-a", "/api"), sharingRoute("young", 1, "web-b", "/api=")}, "young"},
		{"the older of two prefixes",
			[]*v1.HTTPRoute{sharingRoute("b", 0, "web-a", "/api"), sharingRoute("a", 1, "web-b", "/api")}, "b"},
		{"by name at one age",
			[]*v1.HTTPRoute{sharingRoute("b", 0, "web-a", "/api"), sharingRoute("a", 0, "web-b", "/api")}, "a"},
		{"a trailing slash is the same path",
			[]*v1.HTTPRoute{sharingRoute("old", 0, "web-a", "/api"), sharingRoute("young", 1, "web-b", "/api/")}, "old"},
		{"also on the older route",
			[]*v1.HTTPRoute{sharingRoute("old", 0, "web-a", "/api/"), sharingRoute("young", 1, "web-b", "/api")}, "old"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := precedenceManager([]*v1.Gateway{webGateway()}, c.routes...)
			for _, r := range c.routes {
				got, _ := servedPaths(t, h, r)
				if serves := len(got) > 0; serves != (r.Name == c.winner) {
					t.Errorf("route %s serves %v, want the pair only for %s", r.Name, got, c.winner)
				}
			}
		})
	}
}

// A route keeps the pairs no route before it asks for, and says in its status
// which it left out.
func TestARouteKeepsThePairsItHasAlone(t *testing.T) {
	old := sharingRoute("old", 0, "web-a", "/a")
	young := sharingRoute("young", 1, "web-b", "/a", "/b")
	h := precedenceManager([]*v1.Gateway{webGateway()}, old, young)

	got, parents := servedPaths(t, h, young)
	want := map[string]string{"http shop.example.com|/b": "web-b", "https shop.example.com|/b": "web-b"}
	if !mapsEqual(got, want) {
		t.Errorf("younger route serves %v, want %v", got, want)
	}
	accepted, _, message := parentAccepted(parents[0])
	if !accepted || !strings.Contains(message, `path "/a" are served for route llm/old`) {
		t.Errorf("younger route Accepted = %v %q, want True with what it left out", accepted, message)
	}
}

// loxilb-ingress keeps an HTTP port's rules apart from an HTTPS port's, so a
// route on one listener does not take a pair from a route on the other.
func TestHTTPAndHTTPSPairsAreApart(t *testing.T) {
	old := sharingRoute("old", 0, "web-a", "/")
	old.Spec.ParentRefs = []v1.ParentReference{{Name: "web-gw", SectionName: sectionPtr("http")}}
	young := sharingRoute("young", 1, "web-b", "/")
	young.Spec.ParentRefs = []v1.ParentReference{{Name: "web-gw", SectionName: sectionPtr("https")}}
	h := precedenceManager([]*v1.Gateway{webGateway()}, old, young)

	got, _ := servedPaths(t, h, young)
	if want := map[string]string{"https shop.example.com|/": "web-b"}; !mapsEqual(got, want) {
		t.Errorf("younger route serves %v, want %v", got, want)
	}
}

// loxilb-ingress serves the Ingresses of every Gateway on one port, so a route
// of another Gateway takes the pair as well, and the status says so.
func TestARouteOfAnotherGatewayTakesThePairToo(t *testing.T) {
	other := webGateway()
	other.Name = "other-gw"
	old := sharingRoute("old", 0, "web-a", "/")
	old.Spec.ParentRefs = []v1.ParentReference{{Name: "other-gw"}}
	young := sharingRoute("young", 1, "web-b", "/")
	h := precedenceManager([]*v1.Gateway{webGateway(), other}, old, young)

	got, parents := servedPaths(t, h, young)
	if len(got) != 0 {
		t.Errorf("younger route serves %v, want nothing", got)
	}
	_, _, message := parentAccepted(parents[0])
	if !strings.Contains(message, "served for route llm/old of gateway llm/other-gw") {
		t.Errorf("younger route's status %q, want the other Gateway named", message)
	}
}

// What the Gateway manager waits for before it hands a port back to the
// Ingresses is what the routes will have there.
func TestIngressesOnWeighsTheRoutes(t *testing.T) {
	old := sharingRoute("old", 0, "web-a", "/")
	young := sharingRoute("young", 1, "web-b", "/")
	gw := webGateway()
	h := precedenceManager([]*v1.Gateway{gw}, old, young)

	ings, err := h.IngressesOn(gw, 443)
	if err != nil {
		t.Fatalf("IngressesOn: %v", err)
	}
	var routes []string
	for _, ing := range ings {
		routes = append(routes, ing.Annotations["parent-http-route"])
	}
	if !slices.Equal(routes, []string{"old"}) {
		t.Errorf("IngressesOn(443) are those of %v, want old's only", routes)
	}
}

// When the route that has a pair goes, or changes, the routes it kept the
// pair from are queued to take it; a route that asks for other pairs is not.
func TestTheRoutesAPairIsKeptFromAreQueuedWhenItMoves(t *testing.T) {
	old := sharingRoute("old", 0, "web-a", "/")
	young := sharingRoute("young", 1, "web-b", "/")
	apart := sharingRoute("apart", 2, "web-b", "/elsewhere")

	// old is gone from the lister, as it is when its delete is handled.
	h := precedenceManager([]*v1.Gateway{webGateway()}, young, apart)
	h.enqueueCompetitors(old)
	if got := drained(h.queue); !mapsEqual(got, map[HTTPRouteQueueEntry]bool{{"llm", "young"}: true}) {
		t.Errorf("queued %v, want young only", got)
	}

	// old moves to /other: it no longer asks for "/".
	moved := sharingRoute("old", 0, "web-a", "/other")
	h = precedenceManager([]*v1.Gateway{webGateway()}, moved, young, apart)
	h.enqueueCompetitors(old, moved)
	if got := drained(h.queue); !mapsEqual(got, map[HTTPRouteQueueEntry]bool{{"llm", "young"}: true}) {
		t.Errorf("queued %v, want young only", got)
	}
}

func sectionPtr(s string) *v1.SectionName {
	n := v1.SectionName(s)
	return &n
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
