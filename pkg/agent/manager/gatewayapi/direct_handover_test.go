package gatewayapi

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// handedOverGateway - infra/gw with port 80 handed to its direct routes: the
// rule of route is on the peers and the ingress service carries 443 only.
func handedOverGateway(t *testing.T, route *v1.HTTPRoute) (*GatewayManager, *v1.Gateway) {
	t.Helper()
	gm := directManager(t, gatewayPeers, route)
	gw := directGateway()
	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}
	for _, rule := range route.Spec.Hostnames {
		reportInstalled(t, gm, directServiceName("gw", 80, string(rule), "/api"))
	}
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Fatalf("ingress service ports = %v, want 80 handed over", got)
	}
	return gm, gw
}

// A port handed to the direct routes stays with them while a route is added
// and while a rule changes, before the new rules are on the peers. It used to
// go back to the ingress service each time until they were.
func TestADirectPortStaysDirectWhileItsRulesChange(t *testing.T) {
	shop := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm, gw := handedOverGateway(t, shop)

	cart := directRoute("cart", 5, hostnames("cart.example.com"), "/api")
	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(shop, cart))
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with a route added: %v", err)
	}
	if _, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(),
		directServiceName("gw", 80, "cart.example.com", "/api"), metav1.GetOptions{}); err != nil {
		t.Fatalf("no rule Service for the added route: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Errorf("a route added: ingress service ports = %v, want 80 kept with the direct routes", got)
	}

	exact := v1.PathMatchExact
	changed := directRoute("shop", 10, hostnames("shop.example.com"), "/api", func(r *v1.HTTPRoute) {
		r.Spec.Rules[0].Matches[0].Path.Type = &exact
	})
	gm.httpRouteLister = sigs_v1_lister.NewHTTPRouteLister(newIndexer(changed, cart))
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with a rule changed: %v", err)
	}
	svc, _ := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(),
		directServiceName("gw", 80, "shop.example.com", "/api"), metav1.GetOptions{})
	if svc.Annotations[gatewayRoutePathMatchAnnotation] != "exact" {
		t.Fatalf("the rule Service was not changed: %v", svc.Annotations)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Errorf("a rule changed: ingress service ports = %v, want 80 kept with the direct routes", got)
	}
}

// While a peer has not said what it is, as each has not when kube-loxilb
// starts, a port handed over stays so, with its rules. One that was not
// handed over is not.
func TestADirectPortStaysDirectWhileAPeerHasNotReported(t *testing.T) {
	shop := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm, gw := handedOverGateway(t, shop)
	name := directServiceName("gw", 80, "shop.example.com", "/api")

	gm.directPeers = unreportedPeers
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with a peer that has not reported: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Errorf("ingress service ports = %v, want 80 kept with the direct routes", got)
	}
	if _, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Errorf("the rule Service went: %v", err)
	}

	fresh := directManager(t, unreportedPeers, shop)
	if err := fresh.createGateway(directGateway()); err != nil {
		t.Fatal(err)
	}
	if got := ingressServicePortNumbers(t, fresh); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("a Gateway not served directly yet: ingress service ports = %v, want both", got)
	}
	if _, err := fresh.kubeClient.CoreV1().Services("infra").Get(context.Background(), name, metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Errorf("a rule Service was made for it (%v)", err)
	}
}

// A port leaves the ingress service only once its L4 rule is off the peers:
// it is released first, and stays while the release waits. A port that goes
// back is released no longer.
func TestAPortLeavesTheIngressServiceOnlyOnceItsL4RuleIsOff(t *testing.T) {
	shop := directRoute("shop", 10, hostnames("shop.example.com"), "/api")
	gm := directManager(t, gatewayPeers, shop)
	gw := directGateway()
	type call struct{ ports, ingress string }
	var calls []call
	off := false
	gm.releasePorts = func(namespace, name string, ports []int32) (map[int32]bool, error) {
		if namespace != "infra" || name != "gw-ingress-service" {
			t.Errorf("released ports of %s/%s", namespace, name)
		}
		calls = append(calls, call{fmt.Sprint(ports), fmt.Sprint(ingressServicePortNumbers(t, gm))})
		released := map[int32]bool{}
		for _, p := range ports {
			released[p] = off
		}
		return released, nil
	}

	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}
	reportInstalled(t, gm, directServiceName("gw", 80, "shop.example.com", "/api"))
	syncServices(t, gm)
	if err := gm.createGateway(gw); !isHandoverPending(err) {
		t.Fatalf("sync with the L4 rule still on a peer = %v, want the handover pending", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 kept until its L4 rule is off", got)
	}

	off = true
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with the L4 rule off: %v", err)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[443]" {
		t.Errorf("ingress service ports = %v, want 80 gone", got)
	}
	if last := calls[len(calls)-1]; last != (call{"[80]", "[80 443]"}) {
		t.Errorf("last release %+v, want 80 released while the ingress service still had it", last)
	}

	gm.directPeers = plainPeers
	syncServices(t, gm)
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with a plain peer: %v", err)
	}
	if last := calls[len(calls)-1]; last.ports != "[]" {
		t.Errorf("released %s with a plain peer, want none", last.ports)
	}
	if got := ingressServicePortNumbers(t, gm); fmt.Sprint(got) != "[80 443]" {
		t.Errorf("ingress service ports = %v, want 80 back", got)
	}
}
