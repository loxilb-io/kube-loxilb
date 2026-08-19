package loadbalancer

import (
	"strings"
	"testing"

	"github.com/pkg/errors"

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

// The health-monitor fields exist only in the gateway. Sending them to plain
// loxilb would drop them and probe with the older probereq configuration, which
// can mark healthy endpoints down - refuse instead.
func TestInstallLBRefusesProbeFieldsOnPlainLoxilb(t *testing.T) {
	plain := newFakeLoxiLB(t, "")

	m := &Manager{}
	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{ExternalIP: "10.0.0.1", Port: 8080, Protocol: "tcp"},
		Endpoints: []api.LoadBalancerEndpoint{
			{EndpointIP: "31.31.31.1", TargetPort: 8000, Weight: 1,
				EndpointProbe: api.EndpointProbe{URLPath: "/healthz"}},
		},
	}

	err := m.installLB(plain.client(t), lb, false)
	if err == nil {
		t.Fatal("installLB accepted gateway-only probe fields on plain loxilb")
	}
	if !errors.Is(err, ErrInferenceGatewayRequired) {
		t.Errorf("error %q does not wrap ErrInferenceGatewayRequired", err)
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
