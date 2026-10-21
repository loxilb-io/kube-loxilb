package api

import (
	"context"
)

// JWTAuthProfileModel - a JWT auth profile as loxilb-inference-gateway takes it
// on POST /netlox/v1/config/ai/jwtauthprofile. The field names are the
// gateway's; a rule names the profile in jwt_auth_profile.
//
// A POST of an existing name replaces it. The same values again change
// nothing on the gateway; different ones restart the profile's key fetch, and
// Bearer requests checked against it answer 503 until that succeeds.
type JWTAuthProfileModel struct {
	Name                     string   `json:"name"`
	Issuer                   string   `json:"issuer"`
	JWKSURL                  string   `json:"jwks_url,omitempty"`
	Audiences                []string `json:"audiences,omitempty"`
	Algs                     []string `json:"algs,omitempty"`
	LeewaySec                int      `json:"leeway_sec,omitempty"`
	RefreshSec               int      `json:"refresh_sec,omitempty"`
	TenantClaim              string   `json:"tenant_claim,omitempty"`
	UserClaim                string   `json:"user_claim,omitempty"`
	ModelsClaim              string   `json:"models_claim,omitempty"`
	RolesClaim               string   `json:"roles_claim,omitempty"`
	ModelRolePrefix          string   `json:"model_role_prefix,omitempty"`
	UsernameClaim            string   `json:"username_claim,omitempty"`
	ModelAuthz               string   `json:"model_authz,omitempty"`
	DefaultTenant            string   `json:"default_tenant,omitempty"`
	ForwardIdentity          bool     `json:"forward_identity,omitempty"`
	AuthorizationPassthrough bool     `json:"authorization_passthrough,omitempty"`
	PriorityClass            string   `json:"priority_class,omitempty"`
}

func (p *JWTAuthProfileModel) GetKeyStruct() LoxiModel {
	return nil
}

// JWTAuthProfileAPI - the gateway's JWT auth profiles. Gateway only: plain
// loxilb has no such resource.
type JWTAuthProfileAPI struct {
	resource string
	client   *RESTClient
}

func newJWTAuthProfileAPI(r *RESTClient) *JWTAuthProfileAPI {
	return &JWTAuthProfileAPI{resource: "config/ai/jwtauthprofile", client: r}
}

// Create - add or replace a profile.
func (p *JWTAuthProfileAPI) Create(ctx context.Context, profile *JWTAuthProfileModel) error {
	return p.client.POST(p.resource).Body(profile).Do(ctx).err
}

// Delete - remove a profile by name. A profile the peer does not have answers
// 404, and one a rule still refers to answers 409.
func (p *JWTAuthProfileAPI) Delete(ctx context.Context, name string) error {
	return p.client.DELETE(p.resource).SubResource(name).Do(ctx).err
}
