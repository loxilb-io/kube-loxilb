package loadbalancer

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// withSecondPort - llm/web with 8081 as well, installed.
func withSecondPort(t *testing.T, m *Manager, svc *corev1.Service) {
	t.Helper()
	two := svc.DeepCopy()
	two.Spec.Ports = append(two.Spec.Ports, corev1.ServicePort{
		Name: "b", Port: 8081, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(81), NodePort: 30081,
	})
	if _, err := m.kubeClient.CoreV1().Services("llm").Update(context.Background(), two, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update service: %v", err)
	}
	if err := m.addLoadBalancer(two); err != nil {
		t.Fatalf("addLoadBalancer with two ports = %v", err)
	}
	if n := len(m.lbCache[GenKey("llm", "web")].LbServicePairs); n != 2 {
		t.Fatalf("%d pairs cached, want 2", n)
	}
}

func cachedPorts(m *Manager) string {
	var ports []int
	for _, sp := range m.lbCache[GenKey("llm", "web")].LbServicePairs {
		ports = append(ports, int(sp.Port))
	}
	if len(ports) == 2 && ports[0] > ports[1] {
		ports[0], ports[1] = ports[1], ports[0]
	}
	return fmt.Sprint(ports)
}

// A released port's rule comes off the peer at once, and the Service's syncs
// do not send it again while the Service still lists the port. Released no
// longer, it is sent again.
func TestAReleasedPortsRuleComesOffAndStaysOff(t *testing.T) {
	gw := newFakeLoxiLB(t, "")
	peer := gw.client(t)
	peer.IsAlive = true
	m, svc := addHarness(t, peer)
	withSecondPort(t, m, svc)

	_, deletesBefore, _ := gw.counts()
	released, err := m.ReleasePorts("llm", "web", []int32{8081})
	if err != nil || fmt.Sprint(released) != "map[8081:true]" {
		t.Fatalf("ReleasePorts = %v, %v; want 8081 released", released, err)
	}
	if _, deletes, _ := gw.counts(); deletes != deletesBefore+1 {
		t.Errorf("%d DELETEs, want the released port's rule", deletes-deletesBefore)
	}
	if got := cachedPorts(m); got != "[8080]" {
		t.Errorf("pairs cached on %s, want 8080 only", got)
	}

	before := posts(gw)
	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("sync of the Service still listing 8081 = %v", err)
	}
	if n := posts(gw) - before; n != 0 {
		t.Errorf("the sync POSTed %d rules, want none for the released port", n)
	}
	if got := cachedPorts(m); got != "[8080]" {
		t.Errorf("pairs cached on %s after the sync, want 8080 only", got)
	}

	if released, err := m.ReleasePorts("llm", "web", nil); err != nil || len(released) != 0 {
		t.Fatalf("ReleasePorts(none) = %v, %v", released, err)
	}
	if n := m.queue.Len(); n != 1 {
		t.Errorf("%d Services queued, want the one whose port is no longer released", n)
	}
	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("sync with 8081 no longer released = %v", err)
	}
	if got := cachedPorts(m); got != "[8080 8081]" {
		t.Errorf("pairs cached on %s, want 8081 back", got)
	}
}

// A peer that is down may still have the rule, and no delete reaches it: the
// port is not reported released until the peer is back.
func TestAPortIsNotReleasedWhileAPeerIsDown(t *testing.T) {
	up, down := newFakeLoxiLB(t, ""), newFakeLoxiLB(t, "")
	upPeer, downPeer := up.client(t), down.client(t)
	upPeer.IsAlive, downPeer.IsAlive = true, true
	m, svc := addHarness(t, upPeer, downPeer)
	withSecondPort(t, m, svc)

	downPeer.IsAlive = false
	_, upBefore, _ := up.counts()
	released, err := m.ReleasePorts("llm", "web", []int32{8081})
	if err != nil || len(released) != 0 {
		t.Fatalf("ReleasePorts with a peer down = %v, %v; want nothing released", released, err)
	}
	if _, deletes, _ := up.counts(); deletes != upBefore {
		t.Errorf("%d DELETEs while a peer is down, want none", deletes-upBefore)
	}

	downPeer.IsAlive = true
	released, err = m.ReleasePorts("llm", "web", []int32{8081})
	if err != nil || fmt.Sprint(released) != "map[8081:true]" {
		t.Fatalf("ReleasePorts with the peer back = %v, %v; want 8081 released", released, err)
	}
	for name, f := range map[string]*fakeLoxiLB{"up": up, "down": down} {
		if _, deletes, _ := f.counts(); deletes == 0 {
			t.Errorf("no DELETE on the %s peer", name)
		}
	}
}

// A Service with nothing cached has no rule on any peer: its ports are
// released at once, and its first sync sends none for them.
func TestAPortOfAServiceNotSyncedYetIsReleasedAtOnce(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	peer := gw.client(t)
	peer.IsAlive = true
	m, _ := addHarness(t, peer)

	released, err := m.ReleasePorts("llm", "web", []int32{8080})
	if err != nil || fmt.Sprint(released) != "map[8080:true]" {
		t.Fatalf("ReleasePorts = %v, %v; want 8080 released", released, err)
	}
	if err := m.addLoadBalancer(current(t, m)); err != nil {
		t.Fatalf("first sync = %v", err)
	}
	if n := posts(gw); n != 0 {
		t.Errorf("the first sync POSTed %d rules, want none for the released port", n)
	}
}
