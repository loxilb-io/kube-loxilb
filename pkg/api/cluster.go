package api

import (
	"context"
)

type CIStatusModel struct {
	// Instance name
	Instance string `json:"instance,omitempty"`
	// Current Cluster Instance State
	State string `json:"state,omitempty"`
	// Instance Virtual IP address
	Vip string `json:"vip,omitempty"`
}

type CiStatusAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newCiStatusAPI(r *RESTClient) *CiStatusAPI {
	return &CiStatusAPI{
		resource: "config/cistate",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (ciModel *CIStatusModel) GetKeyStruct() LoxiModel {
	return ciModel
}

func (c *CiStatusAPI) Create(ctx context.Context, Model LoxiModel) error {
	resp := c.client.POST(c.resource).Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}
