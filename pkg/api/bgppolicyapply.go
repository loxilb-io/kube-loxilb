package api

import (
	"context"
)

// BGPPolicyApply - Info related to a end-point config entry
type BGPPolicyApply struct {
	NeighIPAddress string   `json:"ipAddress,omitempty"`
	PolicyType     string   `json:"policyType,omitempty"`
	Policies       []string `json:"policies,omitempty"`
	RouteAction    string   `json:"routeAction,omitempty"`
}

type BGPPolicyApplyAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newBGPPolicyApplyAPI(r *RESTClient) *BGPPolicyApplyAPI {
	return &BGPPolicyApplyAPI{
		resource: "config/bgp/policy/apply",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (b *BGPPolicyApplyAPI) CreateBGPPolicyApply(ctx context.Context, Model LoxiModel) error {
	resp := b.client.POST(b.resource).Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (b *BGPPolicyApplyAPI) DeleteBGPPolicyApply(ctx context.Context, Model LoxiModel) error {
	resp := b.client.DELETE(b.resource).Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}
