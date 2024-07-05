package api

import (
	"context"
)

type BGPPolicyDefinitionAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newBGPPolicyDefinition(r *RESTClient) *BGPPolicyDefinitionAPI {
	return &BGPPolicyDefinitionAPI{
		resource: "config/bgp/policy/definitions",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (b *BGPPolicyDefinitionAPI) CreateBGPPolicyDefinition(ctx context.Context, Model LoxiModel) error {
	resp := b.client.POST(b.resource).Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (b *BGPPolicyDefinitionAPI) DeleteBGPPolicyDefinition(ctx context.Context, DefinedName string) error {
	resp := b.client.DELETE(b.resource + "/" + DefinedName).Do(ctx)
	if resp.err != nil {
		return resp.err
	}

	return nil
}
