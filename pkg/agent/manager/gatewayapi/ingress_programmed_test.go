package gatewayapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	netlister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/record"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// returnHarness - gw with port 80 handed to its direct routes, and what a
// port going back reads: loxilb-ingress's pods and Ingresses, and the peers'
// L4 rules, each as the test sets them.
type returnHarness struct {
	t         *testing.T
	gm        *GatewayManager
	gw        *v1.Gateway
	now       time.Time
	ingresses []interface{}
	installed map[int32]bool
	released  []string
	events    *record.FakeRecorder
}

func newReturnHarness(t *testing.T, pods ...*corev1.Pod) *returnHarness {
	t.Helper()
	shop := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm, gw := handedOverGateway(t, shop)
	h := &returnHarness{t: t, gm: gm, gw: gw, now: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
		installed: map[int32]bool{}, events: record.NewFakeRecorder(10)}

	routes := withRouteObjects(servingManager([]*v1.Gateway{gw}), []*v1.HTTPRoute{shop}, gw)
	routes.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))
	gm.ingressesOn = routes.IngressesOn
	gm.installedPorts = func(namespace, name string, ports []int32) map[int32]bool {
		if namespace != "infra" || name != "gw-ingress-service" {
			t.Errorf("asked about the L4 rules of %s/%s", namespace, name)
		}
		out := map[int32]bool{}
		for _, p := range ports {
			out[p] = h.installed[p]
		}
		return out
	}
	gm.releasePorts = func(_, _ string, ports []int32) (map[int32]bool, error) {
		h.released = append(h.released, fmt.Sprint(ports))
		out := map[int32]bool{}
		for _, p := range ports {
			out[p] = true
		}
		return out, nil
	}
	objs := make([]interface{}, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	gm.podLister = corev1lister.NewPodLister(newIndexer(objs...))
	gm.now = func() time.Time { return h.now }
	gm.eventRecorder = h.events
	h.setIngresses()
	return h
}

func (h *returnHarness) setIngresses(ings ...*netv1.Ingress) {
	h.ingresses = h.ingresses[:0]
	for _, ing := range ings {
		h.ingresses = append(h.ingresses, ing)
	}
	h.gm.ingressLister = netlister.NewIngressLister(newIndexer(h.ingresses...))
}

// sync - one Gateway sync, with the Service lister caught up first.
func (h *returnHarness) sync() error {
	h.t.Helper()
	syncServices(h.t, h.gm)
	return h.gm.createGateway(h.gw)
}

// lastRelease - the ports the last sync released.
func (h *returnHarness) lastRelease() string {
	if len(h.released) == 0 {
		return ""
	}
	return h.released[len(h.released)-1]
}

func (h *returnHarness) ruleServiceThere() bool {
	h.t.Helper()
	_, err := h.gm.kubeClient.CoreV1().Services("infra").Get(context.Background(),
		directServiceName("gw", 80, "shop.example.com", "/api"), metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

// written - the port's Ingress as the route manager writes it, at
// generation, with marks.
func (h *returnHarness) written(generation int64, marks map[string]string) *netv1.Ingress {
	h.t.Helper()
	want, err := h.gm.ingressesOn(h.gw, 80)
	if err != nil || len(want) != 1 {
		h.t.Fatalf("Ingresses on port 80 = %v, %v; want the route's one", want, err)
	}
	ing := want[0].DeepCopy()
	ing.Generation = generation
	for k, v := range marks {
		ing.Annotations[k] = v
	}
	return ing
}

func reporting(pod *corev1.Pod) *corev1.Pod {
	pod.Labels[programmedReportLabel] = programmedReportV1
	return pod
}

// A port going back to the ingress service is on it at once, so its listener
// gets its Ingress, but its L4 rule is held back until each loxilb-ingress
// pod that reports has installed the Ingress as it is now; a pod that does
// not report is not waited for. Then the L4 rule goes, and the direct rules
// once every peer has it.
func TestAPortGoingBackWaitsForLoxilbIngressThenItsL4Rule(t *testing.T) {
	h := newReturnHarness(t,
		reporting(loxilbIngressPod("lx-a", "10.42.0.5", true)),
		loxilbIngressPod("lx-old", "10.42.0.6", true),
		reporting(loxilbIngressPod("lx-down", "10.42.0.7", false)))
	h.gm.directPeers = plainPeers

	err := h.sync()
	if !isHandoverPending(err) || !strings.Contains(err.Error(), "is not written yet") {
		t.Fatalf("sync with no Ingress yet = %v, want the port to wait for it", err)
	}
	if got := ingressServicePortNumbers(t, h.gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 back on it", got)
	}
	if got := h.lastRelease(); got != "[80]" {
		t.Errorf("released %s, want 80 held back", got)
	}
	if !h.ruleServiceThere() {
		t.Fatal("the direct rule went while the port waited")
	}
	if !h.gm.bringingBack("infra/gw") {
		t.Error("an Ingress of the Gateway does not bring it back while its port waits")
	}

	h.setIngresses(h.written(1, map[string]string{programmedAnnotationPrefix + "lx-old": "1"}))
	err = h.sync()
	if !isHandoverPending(err) || !strings.Contains(err.Error(), "is not on kube-system/lx-a") || strings.Contains(err.Error(), "lx-old") {
		t.Fatalf("sync with the Ingress on no pod that reports = %v, want it to wait for lx-a alone", err)
	}
	if got := h.lastRelease(); got != "[80]" {
		t.Errorf("released %s, want 80 held back", got)
	}

	stale := h.written(2, map[string]string{programmedAnnotationPrefix + "lx-a": "1"})
	h.setIngresses(stale)
	if err := h.sync(); !isHandoverPending(err) || !strings.Contains(err.Error(), "(generation 2) is not on kube-system/lx-a") {
		t.Fatalf("sync with lx-a on the Ingress's last generation = %v, want it to wait for this one", err)
	}

	h.setIngresses(h.written(2, map[string]string{programmedAnnotationPrefix + "lx-a": "2"}))
	err = h.sync()
	if !isHandoverPending(err) || !strings.Contains(err.Error(), "waits for its L4 rule") {
		t.Fatalf("sync with the Ingress installed = %v, want the port to wait for its L4 rule", err)
	}
	if got := h.lastRelease(); got != "[]" {
		t.Errorf("released %s, want 80 sent", got)
	}
	if !h.ruleServiceThere() {
		t.Fatal("the direct rule went before the L4 rule was on the peers")
	}

	// Sent once, it is not held back again for a later change.
	h.setIngresses(h.written(3, map[string]string{programmedAnnotationPrefix + "lx-a": "2"}))
	if err := h.sync(); !isHandoverPending(err) || h.lastRelease() != "[]" {
		t.Fatalf("sync with the Ingress changed again = %v, released %s; want 80 sent still", err, h.lastRelease())
	}

	h.installed[80] = true
	if err := h.sync(); err != nil {
		t.Fatalf("sync with the L4 rule on every peer: %v", err)
	}
	if h.ruleServiceThere() {
		t.Error("the direct rule stayed once the L4 rule served the port")
	}
	if err := h.sync(); err != nil {
		t.Fatal(err)
	}
	if h.gm.bringingBack("infra/gw") {
		t.Error("the port's return is still under way")
	}
	if len(h.events.Events) != 0 {
		t.Errorf("events %v, want none", <-h.events.Events)
	}
}

// loxilb-ingress is waited for ingressProgrammedTimeout at most: then the L4
// rule goes, with a warning on the Gateway.
func TestAPortGoingBackWaitsForLoxilbIngressOnlySoLong(t *testing.T) {
	h := newReturnHarness(t, reporting(loxilbIngressPod("lx-a", "10.42.0.5", true)))
	h.gm.directPeers = plainPeers

	if err := h.sync(); !isHandoverPending(err) {
		t.Fatalf("first sync = %v", err)
	}
	h.now = h.now.Add(ingressProgrammedTimeout - time.Second)
	if err := h.sync(); !isHandoverPending(err) || h.lastRelease() != "[80]" {
		t.Fatalf("sync before the timeout = %v, released %s; want 80 held back", err, h.lastRelease())
	}
	h.now = h.now.Add(time.Second)
	if err := h.sync(); !isHandoverPending(err) || !strings.Contains(err.Error(), "waits for its L4 rule") {
		t.Fatalf("sync at the timeout = %v, want the L4 rule sent", err)
	}
	if got := h.lastRelease(); got != "[]" {
		t.Errorf("released %s, want 80 sent", got)
	}
	select {
	case ev := <-h.events.Events:
		if !strings.HasPrefix(ev, "Warning "+reasonIngressNotProgrammed) || !strings.Contains(ev, "port 80") {
			t.Errorf("event %q", ev)
		}
	default:
		t.Error("no warning")
	}
}

// With no loxilb-ingress pod that reports - one from before the reports, or
// none ready - nothing is waited for but the L4 rule.
func TestAPortGoingBackWithNoPodThatReports(t *testing.T) {
	h := newReturnHarness(t, loxilbIngressPod("lx-old", "10.42.0.6", true))
	h.gm.directPeers = plainPeers

	err := h.sync()
	if !isHandoverPending(err) || !strings.Contains(err.Error(), "waits for its L4 rule") {
		t.Fatalf("sync = %v, want the port to wait for its L4 rule only", err)
	}
	if got := h.lastRelease(); got != "[]" {
		t.Errorf("released %s, want 80 sent", got)
	}
}

// A pod whose name does not fit in an annotation key cannot report, and is
// not waited for.
func TestAPodThatCannotReportIsNotWaitedFor(t *testing.T) {
	long := reporting(loxilbIngressPod(strings.Repeat("x", 53), "10.42.0.5", true))
	fits := reporting(loxilbIngressPod(strings.Repeat("y", 52), "10.42.0.6", true))
	gm := gatewayTestManager(t, nil, nil)
	gm.podLister = corev1lister.NewPodLister(newIndexer(long, fits))
	pods := gm.reportingPods()
	if len(pods) != 1 || pods[0].Name != fits.Name {
		t.Errorf("reporting pods = %v, want the one whose name fits", pods)
	}
}

// The rules of a port no listener has any more go at once: there is no L4
// rule to wait for.
func TestTheRulesOfAPortWithNoListenerGoAtOnce(t *testing.T) {
	h := newReturnHarness(t, reporting(loxilbIngressPod("lx-a", "10.42.0.5", true)))
	h.gw = h.gw.DeepCopy()
	h.gw.Spec.Listeners = h.gw.Spec.Listeners[1:]
	if err := h.sync(); err != nil {
		t.Fatalf("sync without the HTTP listener: %v", err)
	}
	if h.ruleServiceThere() {
		t.Error("the rule of a port no listener has stayed")
	}
}

// The Ingresses a port waits for are those the route manager writes for the
// route's listener on it.
func TestIngressesOnAreWhatTheRouteGets(t *testing.T) {
	gw := directGateway()
	shop := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	cart := directRoute("cart", 5, hostnames("cart.example.com"), "/cart")
	h := withRouteObjects(servingManager([]*v1.Gateway{gw}), []*v1.HTTPRoute{shop, cart}, gw)
	h.namespaceLister = corev1lister.NewNamespaceLister(newIndexer(namespace("apps", nil), namespace("infra", nil)))

	got, err := h.IngressesOn(gw, 80)
	if err != nil {
		t.Fatal(err)
	}
	var want []*netv1.Ingress
	for _, route := range []*v1.HTTPRoute{shop, cart} {
		ings, _, err := h.routeIngresses(route)
		if err != nil {
			t.Fatal(err)
		}
		for _, ing := range ings {
			if ing.Annotations["parent-listener"] == "http" {
				want = append(want, ing)
			}
		}
	}
	if len(got) != 2 || len(want) != 2 {
		t.Fatalf("Ingresses on 80: %d, the routes' on the HTTP listener: %d; want 2", len(got), len(want))
	}
	byName := map[string]*netv1.Ingress{}
	for _, ing := range got {
		byName[ing.Namespace+"/"+ing.Name] = ing
	}
	for _, w := range want {
		g := byName[w.Namespace+"/"+w.Name]
		if g == nil || !equality.Semantic.DeepEqual(g.Spec, w.Spec) {
			t.Errorf("Ingress %s/%s on 80 = %v, want %v", w.Namespace, w.Name, g, w.Spec)
		}
	}
	if got, _ := h.IngressesOn(gw, 8080); len(got) != 0 {
		t.Errorf("Ingresses on a port no listener has: %v", got)
	}
}
