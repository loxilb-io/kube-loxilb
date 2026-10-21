package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionProgrammed - every loxilb-inference-gateway peer that enforces
// JWT auth profiles has this one, as specified.
const ConditionProgrammed = "Programmed"

// FinalizerName - held while the profile may still be on a peer.
const FinalizerName = "jwtauthprofile.loxilb.io/peers"

// JWTAuthProfile - a JWT auth profile, registered by kube-loxilb on every
// loxilb-inference-gateway peer that enforces them.
//
// The object's name is the profile's name: the value of a Service's or an
// InferencePool's loxilb.io/jwt-auth-profile annotation, at most 63
// characters. A rule naming it is not sent to a peer until the profile is
// there.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type JWTAuthProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JWTAuthProfileSpec   `json:"spec"`
	Status JWTAuthProfileStatus `json:"status,omitempty"`
}

// JWTAuthProfileSpec - the profile as the gateway takes it on
// POST /netlox/v1/config/ai/jwtauthprofile, one field for each. Unset fields
// take the gateway's defaults.
type JWTAuthProfileSpec struct {
	// Issuer - the exact iss a token must carry; an http(s) URL. Unless
	// JWKSURL is set, it is also the base of OIDC discovery.
	Issuer string `json:"issuer"`
	// JWKSURL - where the signing keys are, instead of discovering them.
	JWKSURL string `json:"jwksUrl,omitempty"`
	// Audiences - accepted aud and azp values; one has to match. Empty skips
	// the audience check.
	Audiences []string `json:"audiences,omitempty"`
	// Algs - accepted signature algorithms. Gateway default RS256, ES256.
	Algs []string `json:"algs,omitempty"`
	// LeewaySec - clock skew allowed on exp, nbf and iat. Gateway default 30.
	LeewaySec int32 `json:"leewaySec,omitempty"`
	// RefreshSec - how often the signing keys are fetched again. Gateway
	// default 3600.
	RefreshSec int32 `json:"refreshSec,omitempty"`

	// TenantClaim - claim with the tenant. Gateway default "tenant_id".
	TenantClaim string `json:"tenantClaim,omitempty"`
	// DefaultTenant - tenant of a token without one; empty refuses such a
	// token, since a request nobody can be charged for is not metered.
	DefaultTenant string `json:"defaultTenant,omitempty"`
	// UserClaim - claim with the user. Gateway default "sub".
	UserClaim string `json:"userClaim,omitempty"`
	// UsernameClaim - claim with the user's name. Gateway default
	// "preferred_username".
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// ModelsClaim - claim listing the models the token may use; unset
	// derives them from roles.
	ModelsClaim string `json:"modelsClaim,omitempty"`
	// RolesClaim - claim with the roles. Gateway default
	// "realm_access.roles".
	RolesClaim string `json:"rolesClaim,omitempty"`
	// ModelRolePrefix - prefix of the roles that name a model. Gateway
	// default "model:".
	ModelRolePrefix string `json:"modelRolePrefix,omitempty"`
	// ModelAuthz - "claims-required" (default: no model list, no access) or
	// "allow-all".
	ModelAuthz string `json:"modelAuthz,omitempty"`

	// ForwardIdentity - pass the verified identity upstream in X-Auth-*
	// headers.
	ForwardIdentity bool `json:"forwardIdentity,omitempty"`
	// AuthorizationPassthrough - leave the client's Authorization header on
	// the upstream request instead of stripping it.
	AuthorizationPassthrough bool `json:"authorizationPassthrough,omitempty"`
	// PriorityClass - "standard", "critical" or "sheddable".
	PriorityClass string `json:"priorityClass,omitempty"`
}

// JWTAuthProfileStatus - where the profile stands on the peers.
type JWTAuthProfileStatus struct {
	// Conditions - Programmed: True once every peer that enforces JWT auth
	// profiles has the profile as specified.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Peers - each such peer's answer to the last registration.
	Peers []JWTAuthProfilePeer `json:"peers,omitempty"`
}

// JWTAuthProfilePeer - one peer's answer.
type JWTAuthProfilePeer struct {
	// Host - the peer.
	Host string `json:"host"`
	// Programmed - the peer has the profile as specified.
	Programmed bool `json:"programmed"`
	// LastError - why it does not, when it does not.
	LastError string `json:"lastError,omitempty"`
}

// JWTAuthProfileList - a list of JWTAuthProfiles.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type JWTAuthProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JWTAuthProfile `json:"items"`
}
