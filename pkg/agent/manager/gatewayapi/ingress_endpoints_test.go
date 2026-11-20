package gatewayapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1lister "k8s.io/client-go/listers/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// loxilbIngressPod - a loxilb-ingress pod as loxilb-ingress's manifest makes
// it, in kube-system.
func loxilbIngressPod(name, ip string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kube-system", Name: name, UID: types.UID(name + "-uid"),
			Labels: map[string]string{"app": "loxilb-ingress", "app.kubernetes.io/name": "loxilb-ingress"},
		},
		Spec: corev1.PodSpec{NodeName: "node-" + name},
		Status: corev1.PodStatus{
			PodIP:      ip,
			PodIPs:     []corev1.PodIP{{IP: ip}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

// ingressEndpoints - "ip:ready" for each endpoint of the ingress service's
// slices, and the slices' ports as name=port.
func ingressEndpoints(t *testing.T, gm *GatewayManager, gw *v1.Gateway) (string, string) {
	t.Helper()
	list, err := gm.kubeClient.DiscoveryV1().EndpointSlices(gw.Namespace).List(context.Background(),
		metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + gw.Name + "-ingress-service"})
	if err != nil {
		t.Fatal(err)
	}
	var eps, ports []string
	for _, s := range list.Items {
		if s.Labels[discoveryv1.LabelManagedBy] != ingressSliceManager {
			t.Errorf("slice %s managed by %q", s.Name, s.Labels[discoveryv1.LabelManagedBy])
		}
		for _, ep := range s.Endpoints {
			eps = append(eps, fmt.Sprintf("%s:%v", ep.Addresses[0], *ep.Conditions.Ready))
		}
		for _, p := range s.Ports {
			ports = append(ports, fmt.Sprintf("%s=%d", *p.Name, *p.Port))
		}
	}
	sort.Strings(eps)
	sort.Strings(ports)
	return strings.Join(eps, ","), strings.Join(ports, ",")
}

// The ingress service of a Gateway outside kube-system names the
// loxilb-ingress pods there: by EndpointSlice, as a selector picks pods of
// its own namespace only. The slice follows the pods.
func TestTheIngressServiceNamesLoxilbIngressWhereverItRuns(t *testing.T) {
	gw := directGateway()
	gm := gatewayTestManager(t, nil, []*v1.Gateway{gw})
	pods := []interface{}{loxilbIngressPod("a", "10.42.0.5", true), loxilbIngressPod("b", "10.42.0.6", false)}
	gm.podLister = corev1lister.NewPodLister(newIndexer(pods...))

	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway: %v", err)
	}
	svc, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), "gw-ingress-service", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no ingress service: %v", err)
	}
	if len(svc.Spec.Selector) != 0 {
		t.Errorf("selector = %v, want none", svc.Spec.Selector)
	}
	eps, ports := ingressEndpoints(t, gm, gw)
	if eps != "10.42.0.5:true,10.42.0.6:false" {
		t.Errorf("endpoints = %s, want both loxilb-ingress pods, as ready as they are", eps)
	}
	if ports != "http-80=80,https-443=443" {
		t.Errorf("ports = %s, want each Service port on loxilb-ingress's", ports)
	}

	gm.podLister = corev1lister.NewPodLister(newIndexer(loxilbIngressPod("b", "10.42.0.6", true)))
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync after the pods changed: %v", err)
	}
	if eps, _ := ingressEndpoints(t, gm, gw); eps != "10.42.0.6:true" {
		t.Errorf("endpoints after a went and b got ready = %s", eps)
	}
}

// An ingress service made with the selector by an earlier kube-loxilb loses
// it, and gets the slice.
func TestAnIngressServiceWithTheSelectorLosesIt(t *testing.T) {
	gw := directGateway()
	old := ingressService(gw)
	old.OwnerReferences = gatewayControllerRef(gw)
	old.Spec.Selector = map[string]string{"app": "loxilb-ingress"}
	old.Spec.Ports = ingressServicePorts(gw, nil, nil)
	gm := gatewayTestManager(t, nil, []*v1.Gateway{gw}, old)
	gm.podLister = corev1lister.NewPodLister(newIndexer(loxilbIngressPod("a", "10.42.0.5", true)))

	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("createGateway: %v", err)
	}
	svc, _ := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), "gw-ingress-service", metav1.GetOptions{})
	if len(svc.Spec.Selector) != 0 {
		t.Errorf("selector = %v, want none", svc.Spec.Selector)
	}
	if eps, _ := ingressEndpoints(t, gm, gw); eps != "10.42.0.5:true" {
		t.Errorf("endpoints = %s", eps)
	}
}

// With no ingress service left - every listener is served elsewhere - its
// slice goes too.
func TestTheIngressServiceSliceGoesWithIt(t *testing.T) {
	gw := directGateway()
	gm := gatewayTestManager(t, nil, []*v1.Gateway{gw})
	gm.podLister = corev1lister.NewPodLister(newIndexer(loxilbIngressPod("a", "10.42.0.5", true)))
	if err := gm.createGateway(gw); err != nil {
		t.Fatal(err)
	}

	gw.Spec.Listeners = []v1.Listener{{Name: "tcp", Protocol: v1.TCPProtocolType, Port: 9000}}
	if err := gm.createGateway(gw); err != nil {
		t.Fatalf("sync with no HTTP listener: %v", err)
	}
	if _, err := gm.kubeClient.CoreV1().Services("infra").Get(context.Background(), "gw-ingress-service", metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Fatalf("the ingress service stayed (%v)", err)
	}
	if eps, ports := ingressEndpoints(t, gm, gw); eps != "" || ports != "" {
		t.Errorf("slice left: endpoints %q, ports %q", eps, ports)
	}
}
