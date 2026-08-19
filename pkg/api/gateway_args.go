package api

import (
	"slices"
)

// GatewayArgs - loxilb-inference-gateway-only serviceArguments that are neither
// AI routing nor mTLS: per-service limits, member timeouts, and TLS/HSTS policy
// that fits in a scalar.
//
// Embedded, like AIArgs, because these sit flat inside serviceArguments on the
// wire. Kept separate from AIArgs because they are a different feature with a
// different reason to exist, and because AIArgs must stay comparable - see
// GatewayTLSLists for what that buys.
//
// Everything here is passed through rather than judged. kube-loxilb can check
// that an annotation parses; it cannot check that a certId exists in loxilb's
// registry, and pretending otherwise would either reject valid configuration or
// wave through invalid configuration. loxilb decides, and its refusal comes
// back to the Service as an event.
type GatewayArgs struct {
	// ConnectionLimit - per-service ceiling on simultaneous connections across
	// all endpoints, enforced in eBPF. 0 means unlimited.
	ConnectionLimit uint32 `json:"connectionLimit,omitempty"`

	// --- member timeouts, in seconds ---

	TimeoutMemberConnect uint32 `json:"timeoutMemberConnect,omitempty"`
	TimeoutMemberData    uint32 `json:"timeoutMemberData,omitempty"`
	TimeoutTCPInspect    uint32 `json:"timeoutTcpInspect,omitempty"`

	// --- TLS policy ---

	// TLSCiphers - a cipher string, not a list. Colon-separated, OpenSSL style.
	TLSCiphers string `json:"tls_ciphers,omitempty"`

	// --- HSTS ---

	HSTSMaxAge            uint32 `json:"hsts_max_age,omitempty"`
	HSTSIncludeSubdomains bool   `json:"hsts_include_subdomains,omitempty"`
	HSTSPreload           bool   `json:"hsts_preload,omitempty"`

	// --- certificates already registered in loxilb ---

	// BackendCaCertID - names a CA certificate in loxilb's certId registry.
	// A different mechanism from the Secret-mounted MtlsBackend paths, and a
	// separate dataplane slot; the two do not collide.
	BackendCaCertID     string `json:"backend_ca_cert_id,omitempty"`
	BackendClientCertID string `json:"backend_client_cert_id,omitempty"`
}

// IsSet - whether any of these carry a value.
//
// A struct comparison on purpose. It stops compiling the moment a
// non-comparable field is added here, which is the same tripwire AIArgs has:
// a slice or map would also invalidate the shallow clone that
// StripGatewayFields relies on. When one is genuinely needed it goes in
// GatewayTLSLists instead.
func (g GatewayArgs) IsSet() bool { return g != GatewayArgs{} }

// GatewayTLSLists - the two gateway-only serviceArguments that are lists.
//
// Split out so GatewayArgs and AIArgs keep their comparison-based IsSet. The
// alternative, expanding those into field-by-field comparisons, quietly rots:
// every field added later has to be remembered, and forgetting one makes IsSet
// wrong rather than making it fail to build.
//
// Still embedded, so the wire stays flat.
type GatewayTLSLists struct {
	// TLSVersions - e.g. ["TLSv1.2","TLSv1.3"]. loxilb collapses the list to a
	// min/max range.
	TLSVersions []string `json:"tls_versions,omitempty"`
	// AlpnProtocols - e.g. ["h2","http/1.1"], advertised on listener and pool.
	AlpnProtocols []string `json:"alpn_protocols,omitempty"`
}

func (g GatewayTLSLists) IsSet() bool {
	return len(g.TLSVersions) > 0 || len(g.AlpnProtocols) > 0
}

// Equal - value equality, since == is unavailable here. Used by the reconcile
// loop to notice a changed annotation.
func (g GatewayTLSLists) Equal(other GatewayTLSLists) bool {
	return slices.Equal(g.TLSVersions, other.TLSVersions) &&
		slices.Equal(g.AlpnProtocols, other.AlpnProtocols)
}
