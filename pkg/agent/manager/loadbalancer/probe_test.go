package loadbalancer

import (
	"strings"
	"testing"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func TestGetEndpointProbe(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        api.EndpointProbe
		wantErr     string
	}{
		{
			name:        "nothing set stays empty",
			annotations: map[string]string{lbModeAnnotation: "fullproxy"},
		},
		{
			name: "full configuration",
			annotations: map[string]string{
				probeMethodAnnotation:        "HEAD",
				probePathAnnotation:          "/healthz",
				probeExpectedCodesAnnotation: "200-204",
				probeHTTPVersionAnnotation:   "1.1",
				probeDomainAnnotation:        "api.example.com",
			},
			want: api.EndpointProbe{
				HTTPMethod: "HEAD", URLPath: "/healthz", ExpectedCodes: "200-204",
				HTTPVersion: "1.1", DomainName: "api.example.com",
			},
		},
		{
			// domainName is SNI always but the Host header only at 1.1, so a
			// domain with no version silently gets SNI only. Default it.
			name:        "a domain implies http/1.1",
			annotations: map[string]string{probeDomainAnnotation: "api.example.com"},
			want:        api.EndpointProbe{HTTPVersion: "1.1", DomainName: "api.example.com"},
		},
		{
			name: "an explicit 1.0 still wins over the implication",
			annotations: map[string]string{
				probeDomainAnnotation:      "api.example.com",
				probeHTTPVersionAnnotation: "1.0",
			},
			want: api.EndpointProbe{HTTPVersion: "1.0", DomainName: "api.example.com"},
		},
		{
			name:        "1.1 without a domain is left alone",
			annotations: map[string]string{probeHTTPVersionAnnotation: "1.1"},
			want:        api.EndpointProbe{HTTPVersion: "1.1"},
		},
		{
			name:        "mistyped method",
			annotations: map[string]string{probeMethodAnnotation: "get"},
			wantErr:     `httpMethod "get" is not an HTTP method`,
		},
		{
			name:        "path without a leading slash",
			annotations: map[string]string{probePathAnnotation: "healthz"},
			wantErr:     `urlPath "healthz" must start with "/"`,
		},
		{
			name:        "unsupported http version",
			annotations: map[string]string{probeHTTPVersionAnnotation: "2"},
			wantErr:     `httpVersion "2" must be "1.0" or "1.1"`,
		},
		{
			name:        "domain that is really a url",
			annotations: map[string]string{probeDomainAnnotation: "https://api.example.com/x"},
			wantErr:     "must be a bare host name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getEndpointProbe(aiSvc(tt.annotations))

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("getEndpointProbe = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("getEndpointProbe = %q, want it to contain %q", err, tt.wantErr)
				}
				if got.IsSet() {
					t.Errorf("a rejected config still returned a probe: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("getEndpointProbe: %v", err)
			}
			if got != tt.want {
				t.Errorf("probe = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateExpectedCodes(t *testing.T) {
	valid := []string{"", "200", "200,202", "200,202,204", "200-204", "100", "599"}
	for _, codes := range valid {
		if err := (api.EndpointProbe{ExpectedCodes: codes}).Validate(); err != nil {
			t.Errorf("expectedCodes %q rejected: %v", codes, err)
		}
	}

	invalid := []string{"20", "1000", "abc", "200-", "-204", "204-200", "200,abc", "200-204,206", "0"}
	for _, codes := range invalid {
		if err := (api.EndpointProbe{ExpectedCodes: codes}).Validate(); err == nil {
			t.Errorf("expectedCodes %q accepted, want rejected", codes)
		}
	}
}

// Clearing urlPath for a plain peer would reproduce exactly the failure the
// field exists to avoid: a rule that still probes, and probes "/". The path is
// carried across to probereq instead, and the rule is still programmed -
// refusing would leave that peer with no rule at all.
func TestInstallLBTranslatesProbePathForPlainLoxilb(t *testing.T) {
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ExternalIP: "10.0.0.1", Port: 8080, Protocol: "tcp"},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1,
				EndpointProbe: api.EndpointProbe{
					URLPath: "/healthz", HTTPMethod: "HEAD", ExpectedCodes: "200-204",
					HTTPVersion: "1.1", DomainName: "api.example.com",
				}},
		},
	}

	if err := m.installLB(plain.client(t), lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}

	body := plain.lastBody(t)
	if !strings.Contains(body, `"probereq":"/healthz"`) {
		t.Errorf("probe path was not carried across to probereq\n%s", body)
	}
	for _, unwanted := range []string{"urlPath", "httpMethod", "expectedCodes", "httpVersion", "domainName"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("plain payload carries gateway-only key %q\n%s", unwanted, body)
		}
	}

	// the source model must be untouched, as ever
	if lb.Service.ProbeReq != "" {
		t.Errorf("translation leaked into the source model: %q", lb.Service.ProbeReq)
	}
	if lb.Endpoints[0].URLPath != "/healthz" {
		t.Error("translation cleared the source endpoint")
	}
}

// urlPath wins over a probereq the operator also set, which is the precedence
// the gateway applies.
func TestTranslatedProbePathWinsOverProbeReq(t *testing.T) {
	m := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ProbeReq: "/old"},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", EndpointProbe: api.EndpointProbe{URLPath: "/healthz"}},
		},
	}

	api.StripGatewayFields(&m)

	if m.Service.ProbeReq != "/healthz" {
		t.Errorf("probereq = %q, want /healthz", m.Service.ProbeReq)
	}
}

// With no path set there is nothing to carry, and probereq must be left alone.
func TestTranslationLeavesProbeReqAloneWithoutPath(t *testing.T) {
	m := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ProbeReq: "/old"},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", EndpointProbe: api.EndpointProbe{DomainName: "api.example.com"}},
		},
	}

	api.StripGatewayFields(&m)

	if m.Service.ProbeReq != "/old" {
		t.Errorf("probereq = %q, want it untouched", m.Service.ProbeReq)
	}
}

func TestInstallLBSendsProbeFieldsToGateway(t *testing.T) {
	gw := newFakeLoxiLB(t, api.ProductInferenceGateway)

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ExternalIP: "10.0.0.1", Port: 8080, Protocol: "tcp"},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1,
				EndpointProbe: api.EndpointProbe{
					HTTPMethod: "HEAD", URLPath: "/healthz", ExpectedCodes: "200-204",
					HTTPVersion: "1.1", DomainName: "api.example.com",
				}},
		},
	}

	if err := m.installLB(gw.client(t), lb, false); err != nil {
		t.Fatalf("installLB: %v", err)
	}

	body := gw.lastBody(t)
	for _, want := range []string{
		`"httpMethod":"HEAD"`, `"urlPath":"/healthz"`, `"expectedCodes":"200-204"`,
		`"httpVersion":"1.1"`, `"domainName":"api.example.com"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gateway body missing %s\n%s", want, body)
		}
	}
}

// Stripping for a plain peer must clear the probe block too, and must not
// corrupt the gateway's copy of the same model.
func TestStripClearsProbeFields(t *testing.T) {
	src := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ExternalIP: "10.0.0.1", Port: 8080},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000,
				EndpointProbe: api.EndpointProbe{URLPath: "/healthz", ExpectedCodes: "200"}},
		},
	}

	stripped := src
	api.StripGatewayFields(&stripped)

	if stripped.Endpoints[0].EndpointProbe.IsSet() {
		t.Errorf("probe block survived stripping: %+v", stripped.Endpoints[0].EndpointProbe)
	}
	if !src.Endpoints[0].EndpointProbe.IsSet() {
		t.Error("stripping mutated the source model")
	}
}

// Any one of the five fields switches the gateway's prober into status-code
// mode. httpVersion is the asymmetric one: only "1.1" counts.
func TestSwitchesProberMode(t *testing.T) {
	tests := []struct {
		name  string
		probe api.EndpointProbe
		want  bool
	}{
		{"empty", api.EndpointProbe{}, false},
		{"expected codes", api.EndpointProbe{ExpectedCodes: "200"}, true},
		{"method", api.EndpointProbe{HTTPMethod: "HEAD"}, true},
		{"path", api.EndpointProbe{URLPath: "/healthz"}, true},
		{"domain", api.EndpointProbe{DomainName: "api.example.com"}, true},
		{"http 1.1", api.EndpointProbe{HTTPVersion: "1.1"}, true},
		{"http 1.0 alone does not switch", api.EndpointProbe{HTTPVersion: "1.0"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.probe.SwitchesProberMode(); got != tt.want {
				t.Errorf("SwitchesProberMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Adding a probe annotation retires a proberesp body check that the operator
// never touched. Say so.
func TestProbeModeNotice(t *testing.T) {
	svc := aiSvc(map[string]string{probeRespAnnotation: "OK"})

	notice := probeModeNotice(svc, api.EndpointProbe{DomainName: "api.example.com", HTTPVersion: "1.1"})
	if notice == "" {
		t.Fatal("no notice for proberesp retired by the probe annotations")
	}
	for _, want := range []string{probeRespAnnotation, `"OK"`, "HTTP 200"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q\n%s", want, notice)
		}
	}

	if got := probeModeNotice(svc, api.EndpointProbe{HTTPVersion: "1.0"}); got != "" {
		t.Errorf("http/1.0 alone does not switch the prober, but produced: %s", got)
	}
	if got := probeModeNotice(aiSvc(nil), api.EndpointProbe{URLPath: "/healthz"}); got != "" {
		t.Errorf("no proberesp set, but produced: %s", got)
	}

	explicit := probeModeNotice(svc, api.EndpointProbe{URLPath: "/x", ExpectedCodes: "200-204"})
	if !strings.Contains(explicit, "HTTP 200-204") {
		t.Errorf("notice does not name the configured codes\n%s", explicit)
	}
}

func TestProbeDowngradeNotice(t *testing.T) {
	probe := api.EndpointProbe{
		URLPath: "/healthz", HTTPMethod: "HEAD", DomainName: "api.example.com", HTTPVersion: "1.1",
	}

	if got := probeDowngradeNotice(nil, probe); got != "" {
		t.Errorf("no plain peers, but produced: %s", got)
	}

	notice := probeDowngradeNotice([]string{"10.0.0.9"}, probe)
	for _, want := range []string{probeMethodAnnotation, probeDomainAnnotation, probeHTTPVersionAnnotation, "10.0.0.9", probeReqAnnotation} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q\n%s", want, notice)
		}
	}
	// the path is honoured on plain peers, so it must not be listed as dropped
	if strings.Contains(notice, probePathAnnotation) {
		t.Errorf("the probe path is translated, not dropped\n%s", notice)
	}

	// a path-only configuration loses nothing at all
	if got := probeDowngradeNotice([]string{"10.0.0.9"}, api.EndpointProbe{URLPath: "/healthz"}); got != "" {
		t.Errorf("a path-only probe loses nothing, but produced: %s", got)
	}
}
