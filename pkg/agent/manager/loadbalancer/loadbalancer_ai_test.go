package loadbalancer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/errors"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// fakeLoxiLB is a loxilb REST endpoint that records the bodies POSTed to
// /netlox/v1/config/loadbalancer and reports the given product on /version.
type fakeLoxiLB struct {
	srv *httptest.Server

	// gpuStatus is served from /config/gpu/status; nil makes the endpoint fail,
	// standing in for a peer that cannot answer.
	gpuStatus *api.GPUStatusModel

	mu     sync.Mutex
	bodies []string
}

func newFakeLoxiLB(t *testing.T, product string) *fakeLoxiLB {
	t.Helper()

	f := &fakeLoxiLB{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/netlox/v1/version":
			v := api.VersionModel{Version: "test", Product: product}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&v)

		case r.URL.Path == "/netlox/v1/config/gpu/status":
			if f.gpuStatus == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.gpuStatus)

		case r.URL.Path == "/netlox/v1/config/loadbalancer" && r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			f.mu.Lock()
			f.bodies = append(f.bodies, string(body))
			f.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"Success"}`))

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)

	return f
}

// client builds a LoxiClient for this fake with its flavor already detected.
func (f *fakeLoxiLB) client(t *testing.T) *api.LoxiClient {
	t.Helper()

	base, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	restClient, err := api.NewRESTClient(base, "netlox", "v1", f.srv.Client())
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	c := &api.LoxiClient{RestClient: restClient, Url: f.srv.URL, Host: base.Host}
	c.DetectFlavor(context.Background())
	if !c.FlavorDetected() {
		t.Fatal("flavor detection failed against the fake")
	}

	return c
}

func (f *fakeLoxiLB) lastBody(t *testing.T) string {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.bodies) == 0 {
		t.Fatal("no load-balancer POST was recorded")
	}
	return f.bodies[len(f.bodies)-1]
}

func aiLoadBalancerModel() api.LoadBalancerModel {
	return api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			Mode:       api.LBModeFullProxy,
			AIArgs: api.AIArgs{
				KvExactMode:  api.KvExactModeZmq,
				PDDisaggMode: true,
				SseMode:      true,
			},
		},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, EpRole: api.EpRolePrefill, NixlPort: 9001},
			{EndpointIP: "31.31.31.2", TargetPort: 8000, EpRole: api.EpRoleDecode},
		},
	}
}

// Acceptance criterion 8: a pool mixing a gateway with a plain loxilb must send
// the AI fields to the gateway, and the two request bodies must not contaminate
// each other through the shared endpoint backing array.
func TestInstallLBMixedPoolDoesNotContaminate(t *testing.T) {
	gateway := newFakeLoxiLB(t, api.ProductInferenceGateway)
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	lb := aiLoadBalancerModel()

	// The plain peer refuses an explicit AI request, which is the documented
	// behaviour; drive it with an AI-free model so the fan-out completes and
	// the gateway body can be checked for contamination afterwards.
	plainLB := aiLoadBalancerModel()
	plainLB.Service.AIArgs = api.AIArgs{}

	if err := m.installLB(gateway.client(t), lb, false); err != nil {
		t.Fatalf("installLB against gateway: %v", err)
	}
	if err := m.installLB(plain.client(t), plainLB, false); err != nil {
		t.Fatalf("installLB against plain loxilb: %v", err)
	}

	gwBody := gateway.lastBody(t)
	for _, want := range []string{`"kvExactMode":1`, `"pd_disagg_mode":true`, `"sse_mode":true`, `"ep_role":1`, `"nixl_port":9001`, `"ep_role":2`} {
		if !strings.Contains(gwBody, want) {
			t.Errorf("gateway body missing %s\n%s", want, gwBody)
		}
	}

	plainBody := plain.lastBody(t)
	for _, unwanted := range []string{"kvExactMode", "pd_disagg_mode", "sse_mode", "ep_role", "nixl_port"} {
		if strings.Contains(plainBody, unwanted) {
			t.Errorf("plain loxilb body carries gateway-only key %q\n%s", unwanted, plainBody)
		}
	}

	// The source model must survive both fan-out legs untouched.
	if lb.Endpoints[0].EpRole != api.EpRolePrefill || lb.Endpoints[0].NixlPort != 9001 {
		t.Errorf("source endpoint[0] was mutated: %+v", lb.Endpoints[0])
	}
	if !lb.Service.AIArgs.IsSet() {
		t.Error("source AIArgs was cleared by the fan-out")
	}
}

// Stripping for a plain peer must not disturb a gateway request issued from the
// same model, whichever order the two run in.
func TestInstallLBStripOrderIndependence(t *testing.T) {
	for _, plainFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "plain-first", false: "gateway-first"}[plainFirst], func(t *testing.T) {
			gateway := newFakeLoxiLB(t, api.ProductInferenceGateway)
			plain := newFakeLoxiLB(t, "")

			m := &Manager{}
			lb := aiLoadBalancerModel()
			noAI := aiLoadBalancerModel()
			noAI.Service.AIArgs = api.AIArgs{}
			// share the endpoint backing array, as the real fan-out does
			noAI.Endpoints = lb.Endpoints

			run := []func(){
				func() {
					if err := m.installLB(plain.client(t), noAI, false); err != nil {
						t.Errorf("installLB plain: %v", err)
					}
				},
				func() {
					if err := m.installLB(gateway.client(t), lb, false); err != nil {
						t.Errorf("installLB gateway: %v", err)
					}
				},
			}
			if !plainFirst {
				run[0], run[1] = run[1], run[0]
			}
			run[0]()
			run[1]()

			if body := gateway.lastBody(t); !strings.Contains(body, `"ep_role":1`) {
				t.Errorf("gateway lost ep_role after the plain leg stripped a shared slice\n%s", body)
			}
			if body := plain.lastBody(t); strings.Contains(body, "ep_role") {
				t.Errorf("plain loxilb received ep_role\n%s", body)
			}
		})
	}
}

// Acceptance criterion 4: asking a plain loxilb for AI routing is refused, not
// silently downgraded.
func TestInstallLBRefusesAIOnPlainLoxilb(t *testing.T) {
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	err := m.installLB(plain.client(t), aiLoadBalancerModel(), false)

	if err == nil {
		t.Fatal("installLB accepted an AI rule on plain loxilb")
	}
	if !strings.Contains(err.Error(), "plain loxilb") {
		t.Errorf("error = %q, want it to name the flavor mismatch", err)
	}

	plain.mu.Lock()
	defer plain.mu.Unlock()
	if len(plain.bodies) != 0 {
		t.Errorf("a rule was still POSTed to plain loxilb: %v", plain.bodies)
	}
}

// A non-AI rule must reach a plain peer byte-identically to before this change.
func TestInstallLBPlainRuleUnchanged(t *testing.T) {
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.2",
			Port:       80,
			Protocol:   "tcp",
		},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1},
		},
	}

	if err := m.installLB(plain.client(t), lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}

	body := plain.lastBody(t)
	for _, unwanted := range []string{"kvExactMode", "pd_disagg_mode", "sse_mode", "ep_role", "nixl_port", "AIArgs"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("plain payload gained %q\n%s", unwanted, body)
		}
	}
}

// gpuLB - a sel=gpuaware rule.
func gpuLoadBalancerModel() api.LoadBalancerModel {
	return api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			Mode:       api.LBModeFullProxy,
			Sel:        api.LbSelGPUAware,
		},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1},
		},
	}
}

// GPU-aware selection is armed instance-wide, not by the rule. A peer with it
// disarmed accepts sel=9 and then selects as CHWBL, so the rule must be refused
// rather than programmed into a mode it will not run.
func TestInstallLBRefusesGPUAwareWhenDisarmed(t *testing.T) {
	tests := []struct {
		name   string
		status *api.GPUStatusModel
	}{
		{"monitoring off", &api.GPUStatusModel{Enabled: false, RoutingMode: "standard_chwbl"}},
		{"enabled but still routing as chwbl", &api.GPUStatusModel{Enabled: true, RoutingMode: "standard_chwbl"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
			gw.gpuStatus = tt.status

			m := &Manager{}
			err := m.installLB(gw.client(t), gpuLoadBalancerModel(), false)

			if err == nil {
				t.Fatal("installLB programmed a gpuaware rule into a disarmed peer")
			}
			if !errors.Is(err, ErrGPUMonitoringDisabled) {
				t.Errorf("error %q does not wrap ErrGPUMonitoringDisabled", err)
			}

			gw.mu.Lock()
			defer gw.mu.Unlock()
			if len(gw.bodies) != 0 {
				t.Errorf("a rule was still POSTed: %v", gw.bodies)
			}
		})
	}
}

func TestInstallLBAllowsGPUAwareWhenArmed(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.gpuStatus = &api.GPUStatusModel{Enabled: true, RoutingMode: "gpu_aware", WorkerCount: 4}

	m := &Manager{}
	if err := m.installLB(gw.client(t), gpuLoadBalancerModel(), false); err != nil {
		t.Fatalf("installLB: %v", err)
	}

	if body := gw.lastBody(t); !strings.Contains(body, `"sel":9`) {
		t.Errorf("gateway body missing sel=9\n%s", body)
	}
}

// Armed but tracking nothing is a warning, not a refusal: the telemetry feed
// may simply not have run yet.
func TestInstallLBAllowsGPUAwareWithNoWorkersYet(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.gpuStatus = &api.GPUStatusModel{Enabled: true, RoutingMode: "gpu_aware", WorkerCount: 0}

	m := &Manager{}
	if err := m.installLB(gw.client(t), gpuLoadBalancerModel(), false); err != nil {
		t.Fatalf("installLB refused an armed peer that is merely idle: %v", err)
	}
	gw.lastBody(t)
}

// A diagnostic that cannot be read must not take down a rule that would have
// worked.
func TestInstallLBAllowsGPUAwareWhenStatusUnreadable(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.gpuStatus = nil // endpoint returns 500

	m := &Manager{}
	if err := m.installLB(gw.client(t), gpuLoadBalancerModel(), false); err != nil {
		t.Fatalf("installLB refused on an unreadable status: %v", err)
	}
	gw.lastBody(t)
}

// The pre-flight is scoped to sel=gpuaware, so every other rule costs nothing.
func TestInstallLBSkipsGPUCheckForOtherSelectors(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)
	gw.gpuStatus = nil // any query would fail loudly in the handler

	m := &Manager{}
	lb := gpuLoadBalancerModel()
	lb.Service.Sel = api.LbSelCHWBL

	if err := m.installLB(gw.client(t), lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}
	if body := gw.lastBody(t); !strings.Contains(body, `"sel":8`) {
		t.Errorf("gateway body missing sel=8\n%s", body)
	}
}
