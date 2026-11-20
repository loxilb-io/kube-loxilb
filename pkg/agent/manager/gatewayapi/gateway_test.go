package gatewayapi

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	tk "github.com/loxilb-io/loxilib"
	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
)

func portsOf(ports []corev1.ServicePort) []string {
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s:%d->%s/%s", p.Name, p.Port, p.TargetPort.String(), p.Protocol))
	}
	return out
}

// Listeners that share a port share one port of the ingress service. A port
// for each listener was refused by the API server as a duplicate, and with it
// the whole Service: no listener of the Gateway was served.
func TestIngressServicePorts(t *testing.T) {
	gatewayWith := func(listeners ...v1.Listener) *v1.Gateway {
		return &v1.Gateway{Spec: v1.GatewaySpec{Listeners: listeners}}
	}

	cases := []struct {
		name    string
		gateway *v1.Gateway
		claimed map[v1.SectionName]bool
		want    []string
	}{
		{
			name:    "the Run:ai Gateway, three HTTPS listeners on 443",
			gateway: runaiGateway(),
			want:    []string{"https-443:443->443/TCP"},
		},
		{
			name: "hosts on HTTP and HTTPS",
			gateway: gatewayWith(
				listener("a-http", v1.HTTPProtocolType, 80, "a.example.com"),
				listener("b-http", v1.HTTPProtocolType, 80, "b.example.com"),
				listener("a-https", v1.HTTPSProtocolType, 443, "a.example.com"),
				listener("b-https", v1.HTTPSProtocolType, 443, "b.example.com")),
			want: []string{"http-80:80->80/TCP", "https-443:443->443/TCP"},
		},
		{
			name: "a port other than the standard one",
			gateway: gatewayWith(
				listener("alt", v1.HTTPSProtocolType, 8443, "")),
			want: []string{"https-8443:8443->443/TCP"},
		},
		{
			name: "HTTPS on a port HTTP has first",
			gateway: gatewayWith(
				listener("http", v1.HTTPProtocolType, 8080, ""),
				listener("https", v1.HTTPSProtocolType, 8080, "")),
			want: []string{"http-8080:8080->80/TCP"},
		},
		{
			name: "L4 listeners are not the ingress service's",
			gateway: gatewayWith(
				listener("tcp", v1.TCPProtocolType, 5432, ""),
				listener("udp", v1.UDPProtocolType, 53, ""),
				listener("http", v1.HTTPProtocolType, 80, "")),
			want: []string{"http-80:80->80/TCP"},
		},
		{
			name: "a port an InferencePool serves for one of its listeners",
			gateway: gatewayWith(
				listener("models", v1.HTTPProtocolType, 80, "models.example.com"),
				listener("web", v1.HTTPProtocolType, 80, "web.example.com"),
				listener("secure", v1.HTTPSProtocolType, 443, "web.example.com")),
			claimed: map[v1.SectionName]bool{"models": true},
			want:    []string{"https-443:443->443/TCP"},
		},
		{
			name: "a listener name no port name could hold",
			gateway: gatewayWith(
				listener("http.a-name-of-more-than-sixty-three-characters-with-dots-in-it.example", v1.HTTPProtocolType, 80, "")),
			want: []string{"http-80:80->80/TCP"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := portsOf(ingressServicePorts(tc.gateway, tc.claimed, nil))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("ports = %v, want %v", got, tc.want)
			}
		})
	}
}

func gatewayTestManager(t *testing.T, pool *ippool.IPPool, gateways []*v1.Gateway, services ...*corev1.Service) *GatewayManager {
	t.Helper()
	kube := fake.NewClientset()
	for _, svc := range services {
		if _, err := kube.CoreV1().Services(svc.Namespace).Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed service: %v", err)
		}
	}
	// Created rather than handed to the constructor, which files a Gateway
	// under a version the typed v1 client does not read.
	sigs := sigsfake.NewSimpleClientset()
	gwObjs := make([]interface{}, 0, len(gateways))
	for _, gw := range gateways {
		if _, err := sigs.GatewayV1().Gateways(gw.Namespace).Create(context.Background(), gw.DeepCopy(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed gateway: %v", err)
		}
		gwObjs = append(gwObjs, gw)
	}
	sigs.ClearActions()
	return &GatewayManager{
		kubeClient:     kube,
		sigsClient:     sigs,
		networkConfig:  &config.NetworkConfig{LoxilbGatewayClass: "loxilb.io/loxilb", LoxilbLoadBalancerClass: "loxilb.io/loxilb"},
		externalIPPool: pool,
		gatewayLister:  sigs_v1_lister.NewGatewayLister(newIndexer(gwObjs...)),
		gatewayClassLister: sigs_v1_lister.NewGatewayClassLister(newIndexer(
			gatewayClass(testClass, "loxilb.io/loxilb"),
			gatewayClass("other-gc", "example.com/other"),
		)),
		queue: testQueue(),
	}
}

func ingressService(gw *v1.Gateway) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: gw.Namespace, Name: gw.Name + "-ingress-service",
		Annotations: map[string]string{"gateway-api-controller": "loxilb.io/loxilb", "parent-gateway": gw.Name},
	}}
}

// A Gateway of another controller's class is that controller's: kube-loxilb
// gave every Gateway an address, an ingress service and an Accepted status,
// whichever class it named.
func TestGatewayManagerServesOnlyItsClass(t *testing.T) {
	for _, class := range []v1.ObjectName{"other-gc", "missing-gc"} {
		t.Run(string(class), func(t *testing.T) {
			gw := webGateway()
			gw.Spec.GatewayClassName = class
			// The pool is nil: taking an address from it would panic.
			gm := gatewayTestManager(t, nil, []*v1.Gateway{gw}, ingressService(gw))

			if err := gm.createGateway(gw); err != nil {
				t.Fatalf("createGateway: %v", err)
			}
			for _, action := range gm.sigsClient.(*sigsfake.Clientset).Actions() {
				if action.GetVerb() == "update" {
					t.Errorf("wrote the Gateway: %v", action)
				}
			}
			if _, err := gm.kubeClient.CoreV1().Services(gw.Namespace).Get(context.Background(), gw.Name+"-ingress-service", metav1.GetOptions{}); !errors.IsNotFound(err) {
				t.Errorf("the ingress service made while the Gateway was ours is still there (%v)", err)
			}
		})
	}
}

// A Gateway of this controller's gets its address and ingress service, and
// the informer's copy of it is left as it was.
func TestCreateGatewayLeavesTheCacheAlone(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "10.10.10.1/32", false)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	gw := webGateway()
	gm := gatewayTestManager(t, pool, []*v1.Gateway{gw})

	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway: %v", err)
	}
	if len(gw.Spec.Addresses) != 0 || gw.Annotations != nil {
		t.Errorf("the cached Gateway was written into: addresses %v, annotations %v", gw.Spec.Addresses, gw.Annotations)
	}
	svc, err := gm.kubeClient.CoreV1().Services(gw.Namespace).Get(context.Background(), gw.Name+"-ingress-service", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no ingress service: %v", err)
	}
	if svc.Spec.LoadBalancerIP != "10.10.10.1" {
		t.Errorf("ingress service address = %q, want the allocated one", svc.Spec.LoadBalancerIP)
	}
}

// A Gateway is one item in the queue however many events it has. Queued as
// the objects the events carried, each update was an item of its own, and
// workers took the same Gateway at once - their writes to its ingress service
// conflicting.
func TestGatewayEventsQueueOneItem(t *testing.T) {
	gm := gatewayTestManager(t, nil, []*v1.Gateway{webGateway()})
	first, second := webGateway(), webGateway()
	second.Generation = 2
	gm.enqueueObject(first)
	gm.enqueueObject(second)
	gm.enqueueClassGateways(gatewayClass(testClass, "loxilb.io/loxilb"))
	if item := queued(t, gm.queue); item != "llm/web-gw" {
		t.Errorf("queued %v, want the Gateway's key", item)
	}
}

// A deleted Gateway is gone from the lister by the time its key is handled,
// and what it was allocated is noted on the object: the delete keeps it until
// the address is given back.
func TestReconcileGivesBackADeletedGatewaysAddress(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "10.10.10.1/32", false)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	gm := gatewayTestManager(t, pool, nil)
	ip, ident := pool.GetNewIPAddr("llm/web-gw", 0, "")
	if ip == nil {
		t.Fatal("could not take the pool's address")
	}
	gw := webGateway()
	gw.UID = "old"
	gw.Annotations = map[string]string{"ipam-address": ip.String(), "ident-ipam": ident}

	gm.gatewayDeleted(gw)
	if err := gm.reconcile("llm/web-gw"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ip, _ := pool.GetNewIPAddr("next", 0, ""); ip == nil {
		t.Error("the deleted Gateway's address was not given back")
	}
	if gone := gm.goneGateways("llm/web-gw", nil); len(gone) != 0 {
		t.Errorf("the delete is still kept after it was handled: %v", gone)
	}
}

// Deleted and created again under the same name before the key is handled:
// two Gateways. The first gives back its address, and the second is served -
// here with that same address, the only one the pool has.
func TestReconcileGivesBackTheAddressOfAGatewayCreatedAgain(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "10.10.10.1/32", false)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	again := webGateway()
	again.UID = "new"
	gm := gatewayTestManager(t, pool, []*v1.Gateway{again})
	ip, ident := pool.GetNewIPAddr("llm/web-gw", 0, "")
	if ip == nil {
		t.Fatal("could not take the pool's address")
	}
	old := webGateway()
	old.UID = "old"
	old.Annotations = map[string]string{"ipam-address": ip.String(), "ident-ipam": ident}

	gm.gatewayDeleted(old)
	if err := gm.reconcile("llm/web-gw"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	svc, err := gm.kubeClient.CoreV1().Services("llm").Get(context.Background(), "web-gw-ingress-service", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no ingress service for the Gateway created again: %v", err)
	}
	if svc.Spec.LoadBalancerIP != "10.10.10.1" {
		t.Errorf("ingress service address = %q, want the address the first Gateway gave back", svc.Spec.LoadBalancerIP)
	}
	if gone := gm.goneGateways("llm/web-gw", again); len(gone) != 0 {
		t.Errorf("the first Gateway's delete is still kept: %v", gone)
	}
}

// Deleting a Gateway gives back the address this controller allocated for it,
// and no other: one it came with may be in use by something else.
func TestDeleteGatewayReturnsOnlyItsOwnAddress(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "10.10.10.1/32", false)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	if ip, _ := pool.GetNewIPAddr("someone-else", 0, ""); ip == nil {
		t.Fatal("could not take the pool's address")
	}
	gm := gatewayTestManager(t, pool, nil)

	// Another controller's Gateway, or one with an address of its own, that
	// happens to hold the address: nothing of this controller's to return.
	if err := gm.deleteGateway("llm", "their-gw", "", ""); err != nil {
		t.Fatalf("deleteGateway: %v", err)
	}
	if ip, _ := pool.GetNewIPAddr("next", 0, ""); ip != nil {
		t.Errorf("the address %v was handed back for a Gateway that never had it from the pool", ip)
	}
}

func TestEnqueueClassGateways(t *testing.T) {
	ours, theirs := webGateway(), webGateway()
	theirs.Name, theirs.Spec.GatewayClassName = "their-gw", "other-gc"
	gm := gatewayTestManager(t, nil, []*v1.Gateway{ours, theirs})

	gm.enqueueClassGateways(tombstone(gatewayClass(testClass, "loxilb.io/loxilb")))
	if item := queued(t, gm.queue); item != "llm/web-gw" {
		t.Errorf("queued %v, want only the Gateway of the class", item)
	}
}

func TestAllocatedAddress(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		labels      map[string]string
		want        string
	}{
		{"allocated", map[string]string{"ipam-address": "10.0.0.1"}, nil, "10.0.0.1"},
		{"allocated by a build that noted it as a label", nil, map[string]string{"ipam-address": "10.0.0.2"}, "10.0.0.2"},
		{"an address of its own", nil, nil, ""},
	}
	for _, tc := range cases {
		gw := &v1.Gateway{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations, Labels: tc.labels}}
		if got := allocatedAddress(gw); got != tc.want {
			t.Errorf("%s: allocatedAddress = %q, want %q", tc.name, got, tc.want)
		}
	}
}
