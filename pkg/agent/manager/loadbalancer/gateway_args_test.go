package loadbalancer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func gwArgsAnnotations() map[string]string {
	return map[string]string{
		connectionLimitAnnotation:       "5000",
		timeoutMemberConnectAnnotation:  "5",
		timeoutMemberDataAnnotation:     "50",
		timeoutTCPInspectAnnotation:     "10",
		tlsCiphersAnnotation:            "ECDHE-RSA-AES256-GCM-SHA384:ECDHE-RSA-AES128-GCM-SHA256",
		tlsVersionsAnnotation:           "TLSv1.2,TLSv1.3",
		alpnProtocolsAnnotation:         "h2, http/1.1",
		hstsMaxAgeAnnotation:            "31536000",
		hstsIncludeSubdomainsAnnotation: "true",
		hstsPreloadAnnotation:           "yes",
		backendCaCertIDAnnotation:       "ca-1",
		backendClientCertIDAnnotation:   "client-1",
	}
}

func TestGetGatewayArgs(t *testing.T) {
	m := managerWithMode(0)

	args, lists, err := m.getGatewayArgs(aiSvc(gwArgsAnnotations()))
	if err != nil {
		t.Fatalf("getGatewayArgs: %v", err)
	}

	want := api.GatewayArgs{
		ConnectionLimit:      5000,
		TimeoutMemberConnect: 5,
		TimeoutMemberData:    50,
		TimeoutTCPInspect:    10,
		TLSCiphers:           "ECDHE-RSA-AES256-GCM-SHA384:ECDHE-RSA-AES128-GCM-SHA256",
		HSTSMaxAge:           31536000,
		// yes/no spelling works here as it does for the older annotations
		HSTSIncludeSubdomains: true,
		HSTSPreload:           true,
		BackendCaCertID:       "ca-1",
		BackendClientCertID:   "client-1",
	}
	if args != want {
		t.Errorf("args = %+v\nwant   %+v", args, want)
	}

	// whitespace after the comma is trimmed
	if !lists.Equal(api.GatewayTLSLists{
		TLSVersions:   []string{"TLSv1.2", "TLSv1.3"},
		AlpnProtocols: []string{"h2", "http/1.1"},
	}) {
		t.Errorf("lists = %+v", lists)
	}
}

func TestGetGatewayArgsEmpty(t *testing.T) {
	m := managerWithMode(0)

	args, lists, err := m.getGatewayArgs(aiSvc(map[string]string{lbModeAnnotation: "fullproxy"}))
	if err != nil {
		t.Fatalf("getGatewayArgs: %v", err)
	}
	if args.IsSet() || lists.IsSet() {
		t.Errorf("a service with none of these produced %+v %+v", args, lists)
	}
}

// Only the parse is checked. Whether a cipher string or certId is meaningful is
// loxilb's call, and its refusal comes back as an event.
func TestGetGatewayArgsRejectsOnlyMalformedValues(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantErr     string
	}{
		{
			name:        "non-integer",
			annotations: map[string]string{connectionLimitAnnotation: "lots"},
			wantErr:     `loxilb.io/connection-limit: "lots" is not an integer`,
		},
		{
			name:        "negative",
			annotations: map[string]string{hstsMaxAgeAnnotation: "-1"},
			wantErr:     "loxilb.io/hsts-max-age must be within 0..4294967295",
		},
		{
			name:        "above uint32",
			annotations: map[string]string{timeoutMemberDataAnnotation: "4294967296"},
			wantErr:     "must be within 0..4294967295",
		},
		{
			name:        "non-boolean",
			annotations: map[string]string{hstsPreloadAnnotation: "sure"},
			wantErr:     `loxilb.io/hsts-preload: "sure" is not a boolean`,
		},
		{
			// nonsense to loxilb, but not something kube-loxilb can know
			name:        "an unknown TLS version passes through",
			annotations: map[string]string{tlsVersionsAnnotation: "TLSv9.9"},
		},
		{
			name:        "a certId that may not exist passes through",
			annotations: map[string]string{backendCaCertIDAnnotation: "probably-not-there"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managerWithMode(0)

			args, lists, err := m.getGatewayArgs(aiSvc(tt.annotations))

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("getGatewayArgs = %v, want nil", err)
				}
				if !args.IsSet() && !lists.IsSet() {
					t.Error("a pass-through value was dropped instead of sent")
				}
				return
			}
			if err == nil {
				t.Fatalf("getGatewayArgs = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("getGatewayArgs = %q, want it to contain %q", err, tt.wantErr)
			}
			if args.IsSet() || lists.IsSet() {
				t.Errorf("a rejected config still returned values: %+v %+v", args, lists)
			}
		})
	}
}

// The lists are the first slices in the service payload. Stripping must detach
// them by reassignment, not by clearing shared backing arrays.
func TestStripClearsGatewayArgsWithoutTouchingTheSource(t *testing.T) {
	src := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP:  "10.0.0.1",
			Port:        8080,
			GatewayArgs: api.GatewayArgs{ConnectionLimit: 5000, HSTSMaxAge: 31536000},
			GatewayTLSLists: api.GatewayTLSLists{
				TLSVersions:   []string{"TLSv1.2", "TLSv1.3"},
				AlpnProtocols: []string{"h2"},
			},
		},
	}

	stripped := src
	api.StripGatewayFields(&stripped)

	if stripped.Service.GatewayArgs.IsSet() || stripped.Service.GatewayTLSLists.IsSet() {
		t.Errorf("gateway args survived stripping: %+v %+v",
			stripped.Service.GatewayArgs, stripped.Service.GatewayTLSLists)
	}
	if !src.Service.GatewayArgs.IsSet() || !src.Service.GatewayTLSLists.IsSet() {
		t.Error("stripping mutated the source model")
	}
	if len(src.Service.TLSVersions) != 2 || src.Service.TLSVersions[0] != "TLSv1.2" {
		t.Errorf("the source list was corrupted: %v", src.Service.TLSVersions)
	}
}

// Embedded, so the wire stays flat and the casing is the gateway's.
func TestGatewayArgsSerializeFlat(t *testing.T) {
	svc := api.LoadBalancerService{
		ExternalIP:  "10.0.0.1",
		Port:        8080,
		GatewayArgs: api.GatewayArgs{ConnectionLimit: 5000, TimeoutMemberData: 50, HSTSPreload: true},
		GatewayTLSLists: api.GatewayTLSLists{
			TLSVersions:   []string{"TLSv1.3"},
			AlpnProtocols: []string{"h2", "http/1.1"},
		},
	}

	body, err := json.Marshal(&svc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(body)

	for _, want := range []string{
		`"connectionLimit":5000`, `"timeoutMemberData":50`, `"hsts_preload":true`,
		`"tls_versions":["TLSv1.3"]`, `"alpn_protocols":["h2","http/1.1"]`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s\n%s", want, payload)
		}
	}
	for _, nested := range []string{`"GatewayArgs"`, `"GatewayTLSLists"`} {
		if strings.Contains(payload, nested) {
			t.Errorf("the struct nested instead of flattening\n%s", payload)
		}
	}
}

// Unset fields must leave no trace, so a plain rule is unchanged.
func TestUnsetGatewayArgsProduceNoWireFields(t *testing.T) {
	svc := api.LoadBalancerService{ExternalIP: "10.0.0.2", Port: 80}

	body, err := json.Marshal(&svc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		"connectionLimit", "timeoutMember", "timeoutTcpInspect", "tls_ciphers",
		"tls_versions", "alpn_protocols", "hsts_", "backend_ca_cert_id", "backend_client_cert_id",
	} {
		if strings.Contains(string(body), key) {
			t.Errorf("payload carries %q when unset\n%s", key, body)
		}
	}
}

func TestGatewayArgsDowngradeNotice(t *testing.T) {
	svc := aiSvc(gwArgsAnnotations())

	if got := gatewayArgsDowngradeNotice(svc, nil); got != "" {
		t.Errorf("no plain peers, but produced: %s", got)
	}

	notice := gatewayArgsDowngradeNotice(svc, []string{"10.0.0.9"})
	for _, want := range []string{connectionLimitAnnotation, hstsPreloadAnnotation, backendCaCertIDAnnotation, "10.0.0.9"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q\n%s", want, notice)
		}
	}

	if got := gatewayArgsDowngradeNotice(aiSvc(nil), []string{"10.0.0.9"}); got != "" {
		t.Errorf("nothing set, but produced: %s", got)
	}
}
