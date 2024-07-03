package api

import (
	"context"
)

// BGPPolicyDefinedSets - Info related to a end-point config entry
type BGPPolicyDefinedSets struct {
	Name        string   `json:"name"`
	DefinedType string   `json:"definedType"`
	List        []string `json:"List,omitempty"`
	PrefixList  []Prefix `json:"prefixList,omitempty"`
}

// Prefix - Info related to goBGP Policy Prefix
type Prefix struct {
	IpPrefix        string `json:"ipPrefix"`
	MasklengthRange string `json:"masklengthRange"`
}

type BGPPolicyDefinedSetsAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newBGPPolicyDefinedSetsAPI(r *RESTClient) *BGPPolicyDefinedSetsAPI {
	return &BGPPolicyDefinedSetsAPI{
		resource: "config/bgp/policy/definedsets",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (b *BGPPolicyDefinedSetsAPI) CreateBGPPolicyDefinedSets(ctx context.Context, DefinedType string, Model LoxiModel) error {
	resp := b.client.POST(b.resource + "/" + DefinedType).Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (b *BGPPolicyDefinedSetsAPI) DeleteBGPPolicyDefinedSets(ctx context.Context, DefinedType, DefinedName string) error {
	resp := b.client.DELETE(b.resource + "/" + DefinedType + "/" + DefinedName).Do(ctx)
	if resp.err != nil {
		return resp.err
	}

	return nil
}
