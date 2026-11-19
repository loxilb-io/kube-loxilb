package gatewayapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	corev1lister "k8s.io/client-go/listers/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
	sigs_v1alpha2_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1alpha2"
)

// l4Gateway - TCP listeners on 5432 and 6432, a UDP one on 53 and an HTTP
// one, with an address.
func l4Gateway() *v1.Gateway {
	return &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "l4-gw", Generation: 1},
		Spec: v1.GatewaySpec{GatewayClassName: testClass, Addresses: []v1.GatewaySpecAddress{{Value: "10.10.10.1"}}, Listeners: []v1.Listener{
			listener("tcp", v1.TCPProtocolType, 5432, ""),
			listener("tcp-alt", v1.TCPProtocolType, 6432, ""),
			listener("udp", v1.UDPProtocolType, 53, ""),
			listener("http", v1.HTTPProtocolType, 80, ""),
		}},
	}
}

// tcpRoute - the TCPRoute db in llm, to backendRef db-lb:5432 on l4-gw, made
// by this controller with the pods app=db.
func tcpRoute(mutate ...func(*v1alpha2.TCPRoute)) *v1alpha2.TCPRoute {
	port := v1.PortNumber(5432)
	r := &v1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db",
			Labels:      map[string]string{"selectorkey": "app", "selectorvalue": "db"},
			Annotations: map[string]string{"loxilb.io/lbmode": "fullnat"}},
		Spec: v1alpha2.TCPRouteSpec{
			CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: []v1.ParentReference{{Name: "l4-gw"}}},
			Rules: []v1alpha2.TCPRouteRule{{BackendRefs: []v1.BackendRef{{BackendObjectReference: v1.BackendObjectReference{
				Name: "db-lb", Port: &port}}}}},
		},
	}
	for _, f := range mutate {
		f(r)
	}
	return r
}

type l4Fixture struct {
	routes   []*v1alpha2.TCPRoute
	gateways []*v1.Gateway
	services []*corev1.Service
	grants   *ReferenceGrants
}

func (f l4Fixture) manager(t *testing.T) *l4RouteManager {
	t.Helper()
	kube := fake.NewClientset()
	svcObjs := make([]interface{}, 0, len(f.services))
	for _, svc := range f.services {
		if _, err := kube.CoreV1().Services(svc.Namespace).Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed service: %v", err)
		}
		svcObjs = append(svcObjs, svc)
	}
	kube.ClearActions()

	sigs := sigsfake.NewSimpleClientset()
	routeObjs := make([]interface{}, 0, len(f.routes))
	for _, r := range f.routes {
		if _, err := sigs.GatewayV1alpha2().TCPRoutes(r.Namespace).Create(context.Background(), r.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed route: %v", err)
		}
		routeObjs = append(routeObjs, r)
	}
	sigs.ClearActions()
	gwObjs := make([]interface{}, 0, len(f.gateways))
	for _, gw := range f.gateways {
		gwObjs = append(gwObjs, gw)
	}

	return &l4RouteManager{
		l4RouteKind:     tcpRouteKind(sigs_v1alpha2_lister.NewTCPRouteLister(newIndexer(routeObjs...))),
		name:            tcpRouteMgrName,
		kubeClient:      kube,
		sigsClient:      sigs,
		gatewayProvider: "loxilb.io/loxilb",
		gatewayLister:   sigs_v1_lister.NewGatewayLister(newIndexer(gwObjs...)),
		gatewayClassLister: sigs_v1_lister.NewGatewayClassLister(newIndexer(
			gatewayClass(testClass, "loxilb.io/loxilb"), gatewayClass("other-gc", "example.com/other"))),
		namespaceLister: corev1lister.NewNamespaceLister(newIndexer(namespace("llm", nil), namespace("apps", nil))),
		serviceLister:   corev1lister.NewServiceLister(newIndexer(svcObjs...)),
		referenceGrants: f.grants,
		queue:           testQueue(),
	}
}

func getService(t *testing.T, m *l4RouteManager, namespace, name string) *corev1.Service {
	t.Helper()
	svc, err := m.kubeClient.CoreV1().Services(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service %s/%s: %v", namespace, name, err)
	}
	return svc
}

func serviceGone(t *testing.T, m *l4RouteManager, namespace, name string) bool {
	t.Helper()
	_, err := m.kubeClient.CoreV1().Services(namespace).Get(context.Background(), name, metav1.GetOptions{})
	return errors.IsNotFound(err)
}

func portSummary(svc *corev1.Service) string {
	var ps []string
	for _, p := range svc.Spec.Ports {
		ps = append(ps, fmt.Sprintf("%s:%d->%s/%s", p.Name, p.Port, p.TargetPort.String(), p.Protocol))
	}
	return strings.Join(ps, ",")
}

func syncRoute(t *testing.T, m *l4RouteManager, r *v1alpha2.TCPRoute) {
	t.Helper()
	if err := m.sync(l4RouteKey{r.Namespace, r.Name}, tcpRouteView(r)); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func syncDeleted(t *testing.T, m *l4RouteManager, namespace, name string) {
	t.Helper()
	if err := m.sync(l4RouteKey{namespace, name}, nil); err != nil {
		t.Fatalf("sync of the deleted route: %v", err)
	}
}

// A parentRef without a sectionName attaches to every listener that takes
// the route, which used to be none: the route was served nowhere.
func TestL4RouteAttachesToEveryListenerThatTakesIt(t *testing.T) {
	route := tcpRoute()
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}}.manager(t)
	syncRoute(t, m, route)

	svc := getService(t, m, "llm", "db-lb")
	if got := portSummary(svc); got != "tcp-5432:5432->5432/TCP,tcp-6432:6432->5432/TCP" {
		t.Errorf("ports = %s, want one for each TCP listener", got)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Spec.LoadBalancerIP != "10.10.10.1" ||
		svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "loxilb.io/loxilb" {
		t.Errorf("service spec = %+v, want a loxilb LoadBalancer on the gateway address", svc.Spec)
	}
	if svc.Spec.Selector["app"] != "db" {
		t.Errorf("selector = %v, want the route's", svc.Spec.Selector)
	}
	for k, want := range map[string]string{
		"parent-tcp-route": "db", "parent-tcp-route-namespace": "llm",
		l4BehaviourAnnotation: l4BehaviourCreate, "loxilb.io/lbmode": "fullnat",
	} {
		if svc.Annotations[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, svc.Annotations[k], want)
		}
	}

	// Nothing changed: nothing written.
	m.kubeClient.(*fake.Clientset).ClearActions()
	syncRoute(t, m, route)
	for _, a := range m.kubeClient.(*fake.Clientset).Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "create" {
			t.Errorf("a sync with nothing changed wrote: %v", a)
		}
	}
}

func TestL4RouteFollowsItsListeners(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) { r.Spec.ParentRefs[0].SectionName = sectionRef("tcp").SectionName })
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}}.manager(t)
	syncRoute(t, m, route)

	// One listener keeps the unnamed port earlier builds gave it.
	if got := portSummary(getService(t, m, "llm", "db-lb")); got != ":5432->5432/TCP" {
		t.Errorf("ports = %s, want the one listener's", got)
	}

	// The node port the API server gave it stays through an update.
	svc := getService(t, m, "llm", "db-lb")
	svc.Spec.Ports[0].NodePort = 31234
	if _, err := m.kubeClient.CoreV1().Services("llm").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := tcpRoute(func(r *v1alpha2.TCPRoute) { r.Spec.ParentRefs = []v1.ParentReference{{Name: "l4-gw"}} })
	syncRoute(t, m, moved)
	if got := getService(t, m, "llm", "db-lb").Spec.Ports[0].NodePort; got != 31234 {
		t.Errorf("node port = %d, want the one it had", got)
	}

	// A route that leaves the Gateway leaves no Service behind.
	gone := tcpRoute(func(r *v1alpha2.TCPRoute) { r.Spec.ParentRefs = []v1.ParentReference{{Name: "other-gw"}} })
	syncRoute(t, m, gone)
	if !serviceGone(t, m, "llm", "db-lb") {
		t.Error("the Service of a route that left its Gateway is still there")
	}
}

func TestL4RouteServesOnlyWhatItAttachesTo(t *testing.T) {
	cases := []struct {
		name    string
		gateway func(*v1.Gateway)
		route   func(*v1alpha2.TCPRoute)
	}{
		{name: "another controller's class", gateway: func(gw *v1.Gateway) { gw.Spec.GatewayClassName = "other-gc" }},
		{name: "a listener of another protocol", route: func(r *v1alpha2.TCPRoute) {
			r.Spec.ParentRefs[0].SectionName = sectionRef("udp").SectionName
		}},
		{name: "a route from a namespace the listeners do not take", route: func(r *v1alpha2.TCPRoute) {
			r.Namespace = "apps"
			r.Spec.ParentRefs[0].Namespace = namespacePtr("llm")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := l4Gateway()
			if tc.gateway != nil {
				tc.gateway(gw)
			}
			route := tcpRoute()
			if tc.route != nil {
				tc.route(route)
			}
			m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{gw}}.manager(t)
			syncRoute(t, m, route)
			list, _ := m.kubeClient.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
			if len(list.Items) != 0 {
				t.Errorf("made %d Services for a route not attached", len(list.Items))
			}
		})
	}
}

// Before its Gateway has an address there is nothing to serve it on, and
// what the route already has stays: the address comes in a spec update,
// which queues the route.
func TestL4RouteWaitsForTheAddress(t *testing.T) {
	gw := l4Gateway()
	gw.Spec.Addresses = nil
	route := tcpRoute()
	existing := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db-lb",
		Labels:      map[string]string{"implementation": implementation},
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-tcp-route": "db"}}}
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{gw}, services: []*corev1.Service{existing}}.manager(t)

	syncRoute(t, m, route)
	if serviceGone(t, m, "llm", "db-lb") {
		t.Error("the route's Service went while its Gateway had no address")
	}
}

// A Service in another namespace is written to, not only read, so the route
// needs a grant there; and one made there is deleted with the route, where it
// used to be left behind.
func TestL4RouteAcrossNamespaces(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Spec.Rules[0].BackendRefs[0].Namespace = namespacePtr("backends")
	})

	t.Run("without a grant", func(t *testing.T) {
		m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, grants: staticGrants()}.manager(t)
		syncRoute(t, m, route)
		if !serviceGone(t, m, "backends", "db-lb") {
			t.Error("made a Service in another namespace without a grant")
		}
	})

	t.Run("with one", func(t *testing.T) {
		grants := staticGrants(referenceGrant("backends", "db", grantSpec(kindTCPRoute, "llm", "db-lb")))
		m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, grants: grants}.manager(t)
		syncRoute(t, m, route)
		if got := getService(t, m, "backends", "db-lb").Annotations["parent-tcp-route-namespace"]; got != "llm" {
			t.Errorf("parent-tcp-route-namespace = %q, want the route's", got)
		}

		syncDeleted(t, m, "llm", "db")
		if !serviceGone(t, m, "backends", "db-lb") {
			t.Error("the Service in another namespace outlived its route")
		}
	})
}

// A Service an earlier build made names the route but not its namespace. One
// of the desired name is taken as the route's, and noted so; one elsewhere is
// another route's of the same name for all that can be told, and stays.
func TestL4RouteTakesOverEarlierServices(t *testing.T) {
	legacy := func(namespace string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "db-lb",
			Labels:      map[string]string{"implementation": implementation},
			Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-tcp-route": "db"}}}
	}
	route := tcpRoute()
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()},
		services: []*corev1.Service{legacy("llm"), legacy("apps")}}.manager(t)

	syncRoute(t, m, route)
	if got := getService(t, m, "llm", "db-lb").Annotations["parent-tcp-route-namespace"]; got != "llm" {
		t.Errorf("the earlier Service was not taken over: namespace annotation %q", got)
	}

	syncDeleted(t, m, "llm", "db")
	if !serviceGone(t, m, "llm", "db-lb") {
		t.Error("the route's Service outlived it")
	}
	if serviceGone(t, m, "apps", "db-lb") {
		t.Error("a Service of another namespace's route of the same name was deleted")
	}
}

func TestL4RouteLeavesOthersServicesAlone(t *testing.T) {
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db-lb"}}
	route := tcpRoute()
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, services: []*corev1.Service{foreign}}.manager(t)

	err := m.sync(l4RouteKey{"llm", "db"}, tcpRouteView(route))
	if err == nil || !strings.Contains(err.Error(), "not made for the route") {
		t.Fatalf("sync = %v, want the name taken", err)
	}
	if svc := getService(t, m, "llm", "db-lb"); svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		t.Error("someone else's Service was made a LoadBalancer")
	}
}

// update takes the backend's own Service. Letting it go used to delete it;
// it is put back as it was now.
func TestL4RouteUpdateGivesTheServiceBack(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Labels = map[string]string{l4BehaviourLabel: l4BehaviourUpdate}
		r.Spec.Rules[0].BackendRefs[0].Name = "db"
	})
	original := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db", Labels: map[string]string{"app": "db"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "db"},
			Ports: []corev1.ServicePort{{Name: "pg", Port: 5432, TargetPort: intstr.FromInt(5432), Protocol: corev1.ProtocolTCP}}},
	}
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, services: []*corev1.Service{original}}.manager(t)

	syncRoute(t, m, route)
	taken := getService(t, m, "llm", "db")
	if taken.Spec.Type != corev1.ServiceTypeLoadBalancer || taken.Annotations[l4OriginalAnnotation] == "" {
		t.Fatalf("the Service was not taken: %+v %v", taken.Spec, taken.Annotations)
	}

	syncDeleted(t, m, "llm", "db")
	back := getService(t, m, "llm", "db")
	if back.Spec.Type != corev1.ServiceTypeClusterIP || back.Spec.LoadBalancerClass != nil || back.Spec.LoadBalancerIP != "" {
		t.Errorf("not put back: %+v", back.Spec)
	}
	if got := portSummary(back); got != "pg:5432->5432/TCP" {
		t.Errorf("ports = %s, want the ones it had", got)
	}
	if _, ok := back.Annotations["parent-tcp-route"]; ok || back.Labels["implementation"] != "" {
		t.Errorf("still marked as the route's: %v %v", back.Annotations, back.Labels)
	}
	if back.Labels["app"] != "db" {
		t.Errorf("its own labels went: %v", back.Labels)
	}
}

// A Service without a loadBalancerClass - every ClusterIP Service, so update's
// usual case - used to be dereferenced as if it had one, and kube-loxilb went
// down on it. The class is set now; where the API server will not change it,
// on a LoadBalancer that has none, the route fails and is retried with why.
func TestL4RouteUpdateTakesAServiceWithoutAClass(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Labels = map[string]string{l4BehaviourLabel: l4BehaviourUpdate}
		r.Spec.Rules[0].BackendRefs[0].Name = "db"
	})
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, services: []*corev1.Service{lb}}.manager(t)

	syncRoute(t, m, route)
	if c := getService(t, m, "llm", "db").Spec.LoadBalancerClass; c == nil || *c != "loxilb.io/loxilb" {
		t.Errorf("loadBalancerClass = %v, want loxilb's", c)
	}
}

// duplicate copies the backend's Service. A second sync used to fail on the
// copy it had made itself.
func TestL4RouteDuplicate(t *testing.T) {
	route := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Labels = map[string]string{l4BehaviourLabel: l4BehaviourDuplicate}
		r.Spec.Rules[0].BackendRefs[0].Name = "db"
	})
	source := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "db"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.9", Selector: map[string]string{"app": "db"},
			Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{route}, gateways: []*v1.Gateway{l4Gateway()}, services: []*corev1.Service{source}}.manager(t)

	syncRoute(t, m, route)
	syncRoute(t, m, route)
	copied := getService(t, m, "llm", "db-gw-api")
	if copied.Spec.Selector["app"] != "db" || copied.Spec.ClusterIP != "" || copied.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("copy = %+v, want the backend's selector on a LoadBalancer of its own", copied.Spec)
	}
	if getService(t, m, "llm", "db").Spec.Type == corev1.ServiceTypeLoadBalancer {
		t.Error("the backend's own Service was changed")
	}
}

func TestUDPRouteServicesAreUDP(t *testing.T) {
	port := v1.PortNumber(53)
	route := &v1alpha2.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "llm", Name: "dns"},
		Spec: v1alpha2.UDPRouteSpec{
			CommonRouteSpec: v1.CommonRouteSpec{ParentRefs: []v1.ParentReference{{Name: "l4-gw"}}},
			Rules: []v1alpha2.UDPRouteRule{{BackendRefs: []v1.BackendRef{{BackendObjectReference: v1.BackendObjectReference{
				Name: "dns-lb", Port: &port}}}}},
		},
	}
	m := l4Fixture{gateways: []*v1.Gateway{l4Gateway()}}.manager(t)
	m.l4RouteKind = udpRouteKind(sigs_v1alpha2_lister.NewUDPRouteLister(newIndexer(route)))

	if err := m.sync(l4RouteKey{"llm", "dns"}, udpRouteView(route)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	svc := getService(t, m, "llm", "dns-lb")
	if got := portSummary(svc); got != ":53->53/UDP" {
		t.Errorf("ports = %s, want the UDP listener's", got)
	}
	if svc.Annotations["parent-udp-route"] != "dns" {
		t.Errorf("annotations = %v, want the UDPRoute named", svc.Annotations)
	}
}

func TestL4RouteQueueing(t *testing.T) {
	other := tcpRoute(func(r *v1alpha2.TCPRoute) {
		r.Namespace, r.Name = "apps", "cache"
		r.Spec.ParentRefs = []v1.ParentReference{{Name: "other-gw"}}
	})
	m := l4Fixture{routes: []*v1alpha2.TCPRoute{tcpRoute(), other}, gateways: []*v1.Gateway{l4Gateway()}}.manager(t)
	want := l4RouteKey{"llm", "db"}

	m.enqueueGatewayRoutes(tombstone(l4Gateway()))
	if item := queued(t, m.queue); item != want {
		t.Errorf("the Gateway queued %v, want %v", item, want)
	}
	m.enqueueClassRoutes(gatewayClass(testClass, "loxilb.io/loxilb"))
	if item := queued(t, m.queue); item != want {
		t.Errorf("the class queued %v, want %v", item, want)
	}
	m.enqueueNamespaceRoutes("apps")
	if item := queued(t, m.queue); item != (l4RouteKey{"apps", "cache"}) {
		t.Errorf("the namespace queued %v, want apps/cache", item)
	}
}
