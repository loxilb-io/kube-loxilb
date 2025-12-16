package api

import (
	"context"
	"strconv"

	"k8s.io/klog/v2"
)

type BGPNeigh struct {
	// BGP Neighbor IP address
	IPAddress string `json:"ipAddress,omitempty"`
	// Remote AS number
	RemoteAs int64 `json:"remoteAs,omitempty"`
	// Remote Connect Port (default 179)
	RemotePort int64 `json:"remotePort,omitempty"`
	// Enable multi-hop peering (if needed)
	SetMultiHop bool `json:"setMultiHop,omitempty"`
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
	klog.Infof("[BGP] CreateGlobalConfig called with Model: %+v", Model)
	resp := b.client.POST(b.resource + "/global").Body(Model).Do(ctx)
	klog.Infof("[BGP] CreateGlobalConfig response - StatusCode: %d, Body: %s, Error: %v",
		resp.statusCode, string(resp.body), resp.err)
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

func (b *BGPAPI) DeleteNeigh(ctx context.Context, neighIP string, remoteAs int) error {
	resp := b.client.DELETE(b.resource + "/neigh/" + neighIP).
		Query(map[string]string{"remoteAs": strconv.Itoa(remoteAs)}).Do(ctx)

	if resp.err != nil {
		return resp.err
	}

	return nil
}
