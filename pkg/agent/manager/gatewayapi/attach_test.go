package gatewayapi

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

func hostnamePtr(h string) *v1.Hostname {
	hn := v1.Hostname(h)
	return &hn
}

func namespacePtr(ns string) *v1.Namespace {
	n := v1.Namespace(ns)
	return &n
}

func hostnames(hs ...string) []v1.Hostname {
	if hs == nil {
		return nil
	}
	out := make([]v1.Hostname, 0, len(hs))
	for _, h := range hs {
		out = append(out, v1.Hostname(h))
	}
	return out
}

func listener(name string, protocol v1.ProtocolType, port v1.PortNumber, hostname string) v1.Listener {
	l := v1.Listener{Name: v1.SectionName(name), Protocol: protocol, Port: port}
	if hostname != "" {
		l.Hostname = hostnamePtr(hostname)
	}
	return l
}

func fromNamespaces(from v1.FromNamespaces, selector *metav1.LabelSelector) *v1.AllowedRoutes {
	return &v1.AllowedRoutes{Namespaces: &v1.RouteNamespaces{From: &from, Selector: selector}}
}

func sectionRef(name string) v1.ParentReference {
	section := v1.SectionName(name)
	return v1.ParentReference{Name: "gw", SectionName: &section}
}

func portRef(port v1.PortNumber) v1.ParentReference {
	return v1.ParentReference{Name: "gw", Port: &port}
}

func TestGatewayOf(t *testing.T) {
	ns := v1.Namespace("other")
	core := v1.Group("")
	gatewayGroup := v1.Group(v1.GroupName)
	serviceKind := v1.Kind("Service")
	gatewayKind := v1.Kind("Gateway")

	cases := []struct {
		name   string
		ref    v1.ParentReference
		wantNs string
		wantOK bool
	}{
		{"defaults", v1.ParentReference{Name: "gw"}, "route-ns", true},
		{"namespace", v1.ParentReference{Name: "gw", Namespace: &ns}, "other", true},
		{"explicit group and kind", v1.ParentReference{Name: "gw", Group: &gatewayGroup, Kind: &gatewayKind}, "route-ns", true},
		{"a Service, as a mesh parent", v1.ParentReference{Name: "gw", Group: &core, Kind: &serviceKind}, "", false},
		{"another kind in the group", v1.ParentReference{Name: "gw", Kind: &serviceKind}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNs, gotName, ok := gatewayOf("route-ns", tc.ref)
			if ok != tc.wantOK || gotNs != tc.wantNs || (ok && gotName != "gw") {
				t.Errorf("gatewayOf = (%q, %q, %v), want (%q, \"gw\", %v)", gotNs, gotName, ok, tc.wantNs, tc.wantOK)
			}
		})
	}
}

func TestHostnameIntersection(t *testing.T) {
	cases := []struct {
		a, b string
		want string // "" for none
	}{
		{"test.example.com", "test.example.com", "test.example.com"},
		{"test.example.com", "other.example.com", ""},
		{"*.example.com", "test.example.com", "test.example.com"},
		{"*.example.com", "foo.test.example.com", "foo.test.example.com"},
		{"*.example.com", "example.com", ""},
		{"*.example.com", "test.example.net", ""},
		// Not a label boundary.
		{"*.example.com", "testexample.com", ""},
		{"*.example.com", "*.example.com", "*.example.com"},
		{"*.example.com", "*.test.example.com", "*.test.example.com"},
		{"*.example.com", "*.example.net", ""},
	}
	for _, tc := range cases {
		for _, pair := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			got, ok := hostnameIntersection(v1.Hostname(pair[0]), v1.Hostname(pair[1]))
			if tc.want == "" {
				if ok {
					t.Errorf("hostnameIntersection(%s, %s) = %s, want none", pair[0], pair[1], got)
				}
				continue
			}
			if !ok || string(got) != tc.want {
				t.Errorf("hostnameIntersection(%s, %s) = (%s, %v), want %s", pair[0], pair[1], got, ok, tc.want)
			}
		}
	}
}

func TestListenerHostnames(t *testing.T) {
	cases := []struct {
		name     string
		listener string
		route    []v1.Hostname
		want     []v1.Hostname
		wantOK   bool
	}{
		{"neither: any host", "", nil, nil, true},
		{"route only", "", hostnames("a.example.com"), hostnames("a.example.com"), true},
		{"listener only", "*.example.com", nil, hostnames("*.example.com"), true},
		// The example of the HTTPRoute hostnames documentation.
		{"non-matching route hostnames are dropped", "*.example.com",
			hostnames("test.example.com", "test.example.net"), hostnames("test.example.com"), true},
		{"a route wildcard narrows to the listener's host", "test.example.com",
			hostnames("*.example.com"), hostnames("test.example.com"), true},
		{"duplicates after narrowing", "test.example.com",
			hostnames("*.example.com", "test.example.com"), hostnames("test.example.com"), true},
		{"none shared", "*.example.com", hostnames("example.com", "test.example.net"), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := listener("l", v1.HTTPSProtocolType, 443, tc.listener)
			got, ok := listenerHostnames(&l, tc.route)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("listenerHostnames = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestListenerRouteKinds(t *testing.T) {
	other := v1.Group("example.com")

	cases := []struct {
		name        string
		listener    v1.Listener
		want        []v1.Kind
		wantInvalid int
	}{
		{"HTTP", listener("l", v1.HTTPProtocolType, 80, ""), []v1.Kind{kindHTTPRoute}, 0},
		{"HTTPS", listener("l", v1.HTTPSProtocolType, 443, ""), []v1.Kind{kindHTTPRoute}, 0},
		{"TCP", listener("l", v1.TCPProtocolType, 9000, ""), []v1.Kind{kindTCPRoute}, 0},
		{"UDP", listener("l", v1.UDPProtocolType, 9000, ""), []v1.Kind{kindUDPRoute}, 0},
		{"TLS", listener("l", v1.TLSProtocolType, 443, ""), nil, 0},
		{"named kinds", func() v1.Listener {
			l := listener("l", v1.HTTPProtocolType, 80, "")
			l.AllowedRoutes = &v1.AllowedRoutes{Kinds: []v1.RouteGroupKind{
				{Kind: kindHTTPRoute},
				{Kind: kindHTTPRoute},                // repeated
				{Kind: kindTCPRoute},                 // not on HTTP
				{Kind: "GRPCRoute"},                  // not served
				{Group: &other, Kind: kindHTTPRoute}, // another group
			}}
			return l
		}(), []v1.Kind{kindHTTPRoute}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, invalid := listenerRouteKinds(&tc.listener)
			if !reflect.DeepEqual(got, tc.want) || len(invalid) != tc.wantInvalid {
				t.Errorf("listenerRouteKinds = (%v, %d invalid), want (%v, %d invalid)", got, len(invalid), tc.want, tc.wantInvalid)
			}
		})
	}
}

// runaiGateway - the Gateway of the Run:ai Gateway API guide: three HTTPS
// listeners on one port, told apart by hostname, taking routes from every
// namespace.
func runaiGateway() *v1.Gateway {
	gw := &v1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "runai", Name: "gw"}}
	for _, l := range []v1.Listener{
		listener("https-cluster-domain", v1.HTTPSProtocolType, 443, "cluster.example.com"),
		listener("https-workloads", v1.HTTPSProtocolType, 443, "*.cluster.example.com"),
		listener("https-inference", v1.HTTPSProtocolType, 443, "*.runai-inference.cluster.example.com"),
	} {
		l.AllowedRoutes = fromNamespaces(v1.NamespacesFromAll, nil)
		gw.Spec.Listeners = append(gw.Spec.Listeners, l)
	}
	return gw
}

func attachedNames(a routeAttachment) []string {
	var names []string
	for _, l := range a.Listeners {
		names = append(names, string(l.Listener.Name)+"="+joinHostnames(l.Hostnames))
	}
	return names
}

func joinHostnames(hs []v1.Hostname) string {
	if hs == nil {
		return "*"
	}
	s := make([]string, 0, len(hs))
	for _, h := range hs {
		s = append(s, string(h))
	}
	return strings.Join(s, ",")
}

func TestAttachRoute(t *testing.T) {
	plain := func(ls ...v1.Listener) *v1.Gateway {
		return &v1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gw-ns", Name: "gw"}, Spec: v1.GatewaySpec{Listeners: ls}}
	}
	withAllowed := func(l v1.Listener, a *v1.AllowedRoutes) v1.Listener {
		l.AllowedRoutes = a
		return l
	}
	http := listener("http", v1.HTTPProtocolType, 80, "")
	https := listener("https", v1.HTTPSProtocolType, 443, "")
	tcp := listener("tcp", v1.TCPProtocolType, 9000, "")

	nsLabels := func(name string) (labels.Set, error) {
		switch name {
		case "team-a":
			return labels.Set{"team": "a"}, nil
		case "team-b":
			return labels.Set{"team": "b"}, nil
		}
		return nil, errors.New("not found")
	}
	teamA := &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}

	httpRoute := func(ns string, hs ...string) attachingRoute {
		return attachingRoute{Kind: kindHTTPRoute, Namespace: ns, Hostnames: hostnames(hs...)}
	}

	cases := []struct {
		name       string
		gw         *v1.Gateway
		ref        v1.ParentReference
		route      attachingRoute
		want       []string
		wantReason v1.RouteConditionReason
	}{
		{
			name:  "sectionName",
			gw:    plain(http, https),
			ref:   sectionRef("https"),
			route: httpRoute("gw-ns"),
			want:  []string{"https=*"},
		},
		{
			name:       "sectionName naming no listener",
			gw:         plain(http, https),
			ref:        sectionRef("grpc"),
			route:      httpRoute("gw-ns"),
			wantReason: v1.RouteReasonNoMatchingParent,
		},
		{
			name:  "no sectionName: every listener that takes the route",
			gw:    plain(http, tcp, https),
			ref:   v1.ParentReference{Name: "gw"},
			route: httpRoute("gw-ns"),
			want:  []string{"http=*", "https=*"},
		},
		{
			name:  "port",
			gw:    plain(http, https),
			ref:   portRef(80),
			route: httpRoute("gw-ns"),
			want:  []string{"http=*"},
		},
		{
			name: "sectionName and port must both match",
			gw:   plain(http, https),
			ref: func() v1.ParentReference {
				r := sectionRef("https")
				port := v1.PortNumber(80)
				r.Port = &port
				return r
			}(),
			route:      httpRoute("gw-ns"),
			wantReason: v1.RouteReasonNoMatchingParent,
		},
		{
			name:       "a listener of another protocol",
			gw:         plain(http, tcp),
			ref:        sectionRef("tcp"),
			route:      httpRoute("gw-ns"),
			wantReason: v1.RouteReasonNotAllowedByListeners,
		},
		{
			name:  "a TCPRoute on a TCP listener",
			gw:    plain(http, tcp),
			ref:   v1.ParentReference{Name: "gw"},
			route: attachingRoute{Kind: kindTCPRoute, Namespace: "gw-ns"},
			want:  []string{"tcp=*"},
		},
		{
			name:       "another namespace by default",
			gw:         plain(http),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("team-a"),
			wantReason: v1.RouteReasonNotAllowedByListeners,
		},
		{
			name:  "another namespace with from: All",
			gw:    plain(withAllowed(http, fromNamespaces(v1.NamespacesFromAll, nil))),
			ref:   v1.ParentReference{Name: "gw"},
			route: httpRoute("team-a"),
			want:  []string{"http=*"},
		},
		{
			name:       "from: None",
			gw:         plain(withAllowed(http, fromNamespaces(v1.NamespacesFromNone, nil))),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("gw-ns"),
			wantReason: v1.RouteReasonNotAllowedByListeners,
		},
		{
			name:  "a selected namespace",
			gw:    plain(withAllowed(http, fromNamespaces(v1.NamespacesFromSelector, teamA))),
			ref:   v1.ParentReference{Name: "gw"},
			route: httpRoute("team-a"),
			want:  []string{"http=*"},
		},
		{
			name:       "a namespace the selector leaves out",
			gw:         plain(withAllowed(http, fromNamespaces(v1.NamespacesFromSelector, teamA))),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("team-b"),
			wantReason: v1.RouteReasonNotAllowedByListeners,
		},
		{
			name:       "a namespace whose labels cannot be read",
			gw:         plain(withAllowed(http, fromNamespaces(v1.NamespacesFromSelector, teamA))),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("gone"),
			wantReason: v1.RouteReasonNotAllowedByListeners,
		},
		{
			name: "only the listener sharing a hostname",
			gw: plain(listener("a", v1.HTTPProtocolType, 80, "a.example.com"),
				listener("b", v1.HTTPProtocolType, 80, "b.example.com")),
			ref:   v1.ParentReference{Name: "gw"},
			route: httpRoute("gw-ns", "b.example.com"),
			want:  []string{"b=b.example.com"},
		},
		{
			name:       "no hostname shared",
			gw:         plain(listener("a", v1.HTTPProtocolType, 80, "a.example.com")),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("gw-ns", "c.example.com"),
			wantReason: v1.RouteReasonNoMatchingListenerHostname,
		},
		{
			// A listener that took the route but for its hostnames says more
			// than one that does not take routes from its namespace.
			name: "hostnames, the nearest miss, give the reason",
			gw: plain(listener("a", v1.HTTPProtocolType, 80, "a.example.com"),
				withAllowed(listener("b", v1.HTTPProtocolType, 80, ""), fromNamespaces(v1.NamespacesFromNone, nil))),
			ref:        v1.ParentReference{Name: "gw"},
			route:      httpRoute("gw-ns", "c.example.com"),
			wantReason: v1.RouteReasonNoMatchingListenerHostname,
		},
		{
			// The knative-serving route the Run:ai installer makes, with no
			// sectionName, from its own namespace.
			name:  "Run:ai: a wildcard route takes the listeners it narrows",
			gw:    runaiGateway(),
			ref:   v1.ParentReference{Name: "gw", Namespace: namespacePtr("runai")},
			route: httpRoute("knative-serving", "*.runai-inference.cluster.example.com"),
			want: []string{
				"https-workloads=*.runai-inference.cluster.example.com",
				"https-inference=*.runai-inference.cluster.example.com",
			},
		},
		{
			name:  "Run:ai: a workload host",
			gw:    runaiGateway(),
			ref:   v1.ParentReference{Name: "gw", Namespace: namespacePtr("runai")},
			route: httpRoute("runai-proj", "proj-ws.cluster.example.com"),
			want:  []string{"https-workloads=proj-ws.cluster.example.com"},
		},
		{
			name:  "Run:ai: the cluster host",
			gw:    runaiGateway(),
			ref:   v1.ParentReference{Name: "gw", Namespace: namespacePtr("runai")},
			route: httpRoute("runai", "cluster.example.com"),
			want:  []string{"https-cluster-domain=cluster.example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attachRoute(tc.gw, tc.ref, tc.route, nsLabels)
			if tc.wantReason != "" {
				if got.Accepted() {
					t.Fatalf("attached to %v, want %s", attachedNames(got), tc.wantReason)
				}
				if got.Reason != tc.wantReason {
					t.Errorf("reason %s (%s), want %s", got.Reason, got.Message, tc.wantReason)
				}
				if got.Message == "" {
					t.Errorf("reason %s without a message", got.Reason)
				}
				return
			}
			if !got.Accepted() {
				t.Fatalf("not attached: %s: %s", got.Reason, got.Message)
			}
			if names := attachedNames(got); !reflect.DeepEqual(names, tc.want) {
				t.Errorf("attached to %v, want %v", names, tc.want)
			}
			if got.Reason != "" || got.Message != "" {
				t.Errorf("attached with reason %s (%s)", got.Reason, got.Message)
			}
		})
	}
}
