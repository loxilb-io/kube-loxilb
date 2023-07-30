package api

import (
	"context"
)

type BGPNeigh struct {
	// BGP Neighbor IP address
	IPAddress string `json:"ipAddress,omitempty"`
	// Remote AS number
	RemoteAs int64 `json:"remoteAs,omitempty"`
}

type BGPGlobalConfig struct {
	// Local AS number
	LocalAs int64 `json:"localAs,omitempty"`
	// BGP Router ID
	RouterID string `json:"routerId,omitempty"`
	// Set Next hop self option
	SetNHSelf bool `json:"setNextHopSelf,omitempty"`
	// Listen Port
	ListenPort uint16 `json:"listenPort,omitempty"`
}

type BGPAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newBGPAPI(r *RESTClient) *BGPAPI {
	return &BGPAPI{
		resource: "config/bgp",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (bgpNeighModel *BGPNeigh) GetKeyStruct() LoxiModel {
	return bgpNeighModel
}

func (bgpGlobalModel *BGPGlobalConfig) GetKeyStruct() LoxiModel {
	return bgpGlobalModel
}

func (b *BGPAPI) CreateGlobalConfig(ctx context.Context, Model LoxiModel) error {
	resp := b.client.POST(b.resource + "/global").Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (b *BGPAPI) CreateNeigh(ctx context.Context, Model LoxiModel) error {
	resp := b.client.POST(b.resource + "/neigh").Body(Model).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}
