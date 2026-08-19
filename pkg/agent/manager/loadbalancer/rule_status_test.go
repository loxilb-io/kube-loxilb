package loadbalancer

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// A malformed Error fails the whole UpdateStatus, and that call also publishes
// the ingress IP - so these have to satisfy Kubernetes' rules exactly.
func TestPortStatusErrorVocabularyIsValid(t *testing.T) {
	for _, value := range []string{portStatusDegraded, portStatusOffline, portStatusUnknown} {
		if errs := validation.IsQualifiedName(value); len(errs) > 0 {
			t.Errorf("%q is not a qualified name: %v", value, errs)
		}
		if len(value) > 316 {
			t.Errorf("%q is %d characters, over the 316 limit", value, len(value))
		}
		if !strings.HasPrefix(value, "loxilb.io/") {
			t.Errorf("%q is not namespaced to loxilb.io", value)
		}
	}
}

func TestWorstOperatingStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"all healthy", []string{api.OperatingStatusOnline, api.OperatingStatusOnline}, api.OperatingStatusOnline},
		{"one degraded wins over online", []string{api.OperatingStatusOnline, api.OperatingStatusDegraded}, api.OperatingStatusDegraded},
		{"offline wins over degraded", []string{api.OperatingStatusDegraded, api.OperatingStatusOffline}, api.OperatingStatusOffline},
		{"offline wins whatever the order", []string{api.OperatingStatusOffline, api.OperatingStatusOnline}, api.OperatingStatusOffline},
		{
			// no monitor is the absence of a verdict, so an actual verdict
			// wins - whichever order the peers answered in
			name:     "no monitor loses to online",
			statuses: []string{api.OperatingStatusNoMonitor, api.OperatingStatusOnline},
			want:     api.OperatingStatusOnline,
		},
		{
			name:     "and loses to it from the other side too",
			statuses: []string{api.OperatingStatusOnline, api.OperatingStatusNoMonitor},
			want:     api.OperatingStatusOnline,
		},
		{
			name:     "no monitor still loses to degraded",
			statuses: []string{api.OperatingStatusNoMonitor, api.OperatingStatusDegraded},
			want:     api.OperatingStatusDegraded,
		},
		{"no monitor alone", []string{api.OperatingStatusNoMonitor}, api.OperatingStatusNoMonitor},
		{"an unexpected value is not absorbed", []string{api.OperatingStatusOnline, "ERROR"}, "ERROR"},
		{"nothing reported", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := worstOperatingStatus(tt.statuses); got != tt.want {
				t.Errorf("worstOperatingStatus(%v) = %q, want %q", tt.statuses, got, tt.want)
			}
		})
	}
}

func TestPortStatusErrorFor(t *testing.T) {
	tests := []struct {
		status string
		want   *string
	}{
		{api.OperatingStatusOnline, nil},
		{api.OperatingStatusNoMonitor, nil},
		{"", nil},
		{api.OperatingStatusDegraded, &[]string{portStatusDegraded}[0]},
		{api.OperatingStatusOffline, &[]string{portStatusOffline}[0]},
		{"ERROR", &[]string{portStatusUnknown}[0]},
	}

	for _, tt := range tests {
		got := portStatusErrorFor(tt.status)
		switch {
		case tt.want == nil && got != nil:
			t.Errorf("%q produced %q, want nil", tt.status, *got)
		case tt.want != nil && got == nil:
			t.Errorf("%q produced nil, want %q", tt.status, *tt.want)
		case tt.want != nil && *got != *tt.want:
			t.Errorf("%q produced %q, want %q", tt.status, *got, *tt.want)
		}
	}
}

func statusSvc() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{Port: 8080, Protocol: corev1.ProtocolTCP},
				{Port: 9090, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// Every declared port gets an entry, which is what the field's contract asks
// for, and worst-of never crosses port boundaries.
func TestPortStatusesFor(t *testing.T) {
	collected := []ruleStatus{
		{externalIP: "10.0.0.1", port: 8080, protocol: "tcp", status: api.OperatingStatusOffline},
		{externalIP: "10.0.0.1", port: 9090, protocol: "tcp", status: api.OperatingStatusOnline},
		// a different VIP must not leak into this one
		{externalIP: "10.0.0.2", port: 9090, protocol: "tcp", status: api.OperatingStatusOffline},
	}

	ports := portStatusesFor(statusSvc(), "10.0.0.1", collected)

	if len(ports) != 2 {
		t.Fatalf("got %d entries, want one per declared port", len(ports))
	}
	if ports[0].Port != 8080 || ports[0].Error == nil || *ports[0].Error != portStatusOffline {
		t.Errorf("port 8080 = %+v, want offline", ports[0])
	}
	if ports[1].Port != 9090 || ports[1].Error != nil {
		t.Errorf("port 9090 = %+v, want no error - the offline report was for another VIP", ports[1])
	}
}

// A port nothing reported on still gets an entry, with no error.
func TestPortStatusesForCoversUnreportedPorts(t *testing.T) {
	ports := portStatusesFor(statusSvc(), "10.0.0.1", []ruleStatus{
		{externalIP: "10.0.0.1", port: 8080, protocol: "tcp", status: api.OperatingStatusDegraded},
	})

	if len(ports) != 2 {
		t.Fatalf("got %d entries, want 2", len(ports))
	}
	if ports[1].Error != nil {
		t.Errorf("an unreported port carries %q, want nil", *ports[1].Error)
	}
}

func TestPortStatusesEqual(t *testing.T) {
	degraded := portStatusDegraded
	offline := portStatusOffline

	base := []corev1.PortStatus{{Port: 80, Protocol: corev1.ProtocolTCP, Error: &degraded}}

	if !portStatusesEqual(base, []corev1.PortStatus{{Port: 80, Protocol: corev1.ProtocolTCP, Error: &degraded}}) {
		t.Error("identical lists compared unequal")
	}
	if portStatusesEqual(base, []corev1.PortStatus{{Port: 80, Protocol: corev1.ProtocolTCP, Error: &offline}}) {
		t.Error("a changed error compared equal")
	}
	if portStatusesEqual(base, []corev1.PortStatus{{Port: 80, Protocol: corev1.ProtocolTCP}}) {
		t.Error("dropping the error compared equal")
	}
	if portStatusesEqual(base, nil) {
		t.Error("a different length compared equal")
	}
}

// statusManager wires a Manager around the given peers with one programmed
// rule on 10.0.0.1:8080/tcp.
func statusManager(t *testing.T, svc *corev1.Service, peers ...*api.LoxiClient) (*Manager, *record.FakeRecorder) {
	t.Helper()

	recorder := record.NewFakeRecorder(16)
	m := &Manager{
		kubeClient:    k8sfake.NewSimpleClientset(svc),
		eventRecorder: recorder,
		LoxiClients:   &api.LoxiClientPool{Clients: peers},
		lbCache: LbCacheTable{
			GenKey(svc.Namespace, svc.Name): &LbCacheEntry{
				LbServicePairs: map[string]*LbServicePairEntry{
					GenSPKey("10.0.0.1", 8080, "tcp"): {
						ExternalIP: "10.0.0.1", Port: 8080, Protocol: "tcp",
					},
				},
			},
		},
	}

	return m, recorder
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The status sub-resource does not exist on plain loxilb, so asking would get a
// 404 for every service and read as ill health. Plain peers are not asked.
func TestSyncRuleStatusSkipsPlainPeers(t *testing.T) {
	plain := newFakeLoxiLB(t, "")
	plain.ruleStatus = nil // any query would 404

	svc := statusSvc()
	m, recorder := statusManager(t, svc, plain.client(t))

	m.syncRuleStatus(svc, GenKey(svc.Namespace, svc.Name))

	plain.mu.Lock()
	queries := plain.statusQueries
	plain.mu.Unlock()

	if queries != 0 {
		t.Errorf("a plain peer was queried %d times", queries)
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("plain peers produced events: %v", events)
	}
}

// A missing rule is not a degraded one. It gets its own report.
func TestSyncRuleStatusReportsMissingRuleSeparately(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.ruleStatus = nil // 404

	svc := statusSvc()
	m, recorder := statusManager(t, svc, gw.client(t))

	m.syncRuleStatus(svc, GenKey(svc.Namespace, svc.Name))

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], ReasonRuleMissing) {
		t.Fatalf("events = %v, want one %s", events, ReasonRuleMissing)
	}

	cur, err := m.kubeClient.CoreV1().Services(svc.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	for _, ing := range cur.Status.LoadBalancer.Ingress {
		if len(ing.Ports) != 0 {
			t.Errorf("a missing rule was recorded as a port status: %+v", ing.Ports)
		}
	}
}

// The whole point: a degraded rule becomes visible on the Service, once.
func TestSyncRuleStatusWritesAndOnlyOnChange(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.ruleStatus = &api.LoadBalancerStatusModel{OperatingStatus: api.OperatingStatusDegraded}

	svc := statusSvc()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}}
	m, recorder := statusManager(t, svc, gw.client(t))
	key := GenKey(svc.Namespace, svc.Name)

	m.syncRuleStatus(svc, key)

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], ReasonRuleUnhealthy) {
		t.Fatalf("events = %v, want one %s", events, ReasonRuleUnhealthy)
	}

	cur, err := m.kubeClient.CoreV1().Services(svc.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	ports := cur.Status.LoadBalancer.Ingress[0].Ports
	if len(ports) != 2 {
		t.Fatalf("ports = %+v, want one entry per declared port", ports)
	}
	if ports[0].Error == nil || *ports[0].Error != portStatusDegraded {
		t.Errorf("port 8080 error = %v, want %s", ports[0].Error, portStatusDegraded)
	}

	// unchanged on the next few reconciles: no events, no further writes
	before := len(m.kubeClient.(*k8sfake.Clientset).Actions())
	for i := 0; i < 3; i++ {
		m.syncRuleStatus(svc, key)
	}
	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("an unchanged status produced %v", events)
	}
	after := m.kubeClient.(*k8sfake.Clientset).Actions()
	for _, action := range after[before:] {
		if action.GetVerb() == "update" {
			t.Errorf("an unchanged status still wrote to the API server")
		}
	}

	// recovery is worth saying too
	gw.ruleStatus = &api.LoadBalancerStatusModel{OperatingStatus: api.OperatingStatusOnline}
	m.syncRuleStatus(svc, key)
	events = drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], ReasonRuleHealthy) {
		t.Errorf("events = %v, want one %s", events, ReasonRuleHealthy)
	}
}

// Worst-of across peers, and the event names which peer.
func TestSyncRuleStatusTakesTheWorstPeer(t *testing.T) {
	healthy := newFakeLoxiLB(t, api.ProductInferenceGateway)
	healthy.ruleStatus = &api.LoadBalancerStatusModel{OperatingStatus: api.OperatingStatusOnline}
	broken := newFakeLoxiLB(t, api.ProductInferenceGateway)
	broken.ruleStatus = &api.LoadBalancerStatusModel{OperatingStatus: api.OperatingStatusOffline}

	svc := statusSvc()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}}
	brokenClient := broken.client(t)
	m, recorder := statusManager(t, svc, healthy.client(t), brokenClient)

	m.syncRuleStatus(svc, GenKey(svc.Namespace, svc.Name))

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], ReasonRuleUnhealthy) {
		t.Fatalf("events = %v, want one %s", events, ReasonRuleUnhealthy)
	}
	if !strings.Contains(events[0], brokenClient.Host) {
		t.Errorf("the event does not name the failing peer: %s", events[0])
	}
	if strings.Contains(events[0], "ONLINE") {
		t.Errorf("the event reports the healthy peer instead of the worst: %s", events[0])
	}
}
