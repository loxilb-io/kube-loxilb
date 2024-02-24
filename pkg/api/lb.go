package api

import (
	"context"
	"fmt"
	"net/http"
)

type EpSelect uint
type LbMode int32
type LoadBalancerListModel struct {
	Item []LoadBalancerModel `json:"lbAttr"`
}

func (lbListModel *LoadBalancerListModel) GetKeyStruct() LoxiModel {
	return nil
}

type LoadBalancerModel struct {
	Service      LoadBalancerService    `json:"serviceArguments"`
	SecondaryIPs []LoadBalancerSecIp    `json:"secondaryIPs"`
	Endpoints    []LoadBalancerEndpoint `json:"endpoints"`
}

func (lbModel *LoadBalancerModel) GetKeyStruct() LoxiModel {
	return &lbModel.Service
}

type LoadBalancerService struct {
	ExternalIP   string   `json:"externalIP" key:"externalipaddress"`
	Port         uint16   `json:"port" key:"port"`
	Protocol     string   `json:"protocol" key:"protocol"`
	Sel          EpSelect `json:"sel"`
	Mode         LbMode   `json:"mode"`
	BGP          bool     `json:"BGP" options:"bgp"`
	Monitor      bool     `json:"Monitor"`
	Timeout      uint32   `json:"inactiveTimeOut"`
	Block        uint16   `json:"block" options:"block"`
	Managed      bool     `json:"managed,omitempty"`
	ProbeType    string   `json:"probetype"`
	ProbePort    uint16   `json:"probeport"`
	ProbeReq     string   `json:"probereq"`
	ProbeResp    string   `json:"proberesp"`
	ProbeRetries int32    `json:"probeRetries,omitempty"`
	ProbeTimeout uint32   `json:"probeTimeout,omitempty"`
	Name         string   `json:"name,omitempty"`
}

func (lbService *LoadBalancerService) GetKeyStruct() LoxiModel {
	return lbService
}

type LoadBalancerEndpoint struct {
	EndpointIP string `json:"endpointIP"`
	TargetPort uint16 `json:"targetPort"`
	Weight     uint8  `json:"weight"`
	State      string `json:"state"`
	Counter    string `json:"counter"`
}

type LoadBalancerSecIp struct {
	SecondaryIP string `json:"secondaryIP"`
}

type LoadBalancerAPI struct {
	resource  string
	provider  string
	version   string
	deleteKey []string
	client    *RESTClient
	APICommonFunc
}

func newLoadBalancerAPI(r *RESTClient) *LoadBalancerAPI {
	return &LoadBalancerAPI{
		resource:  "config/loadbalancer",
		deleteKey: []string{"externalipaddress", "port", "protocol"},
		provider:  r.provider,
		version:   r.version,
		client:    r,
	}
}

func (l *LoadBalancerAPI) GetModel() LoxiModel {
	return &LoadBalancerModel{}
}

func (l *LoadBalancerAPI) GetListModel() LoxiModel {
	return &LoadBalancerListModel{}
}

func (l *LoadBalancerAPI) Get(ctx context.Context, name string) (LoxiModel, error) {
	lbModel := l.GetModel()

	resp := l.client.GET(l.resource).SubResource(name).Do(ctx).UnMarshal(lbModel)
	if resp.err != nil {
		fmt.Println(resp.err)
		return lbModel, resp.err
	}

	return lbModel, nil
}

func (l *LoadBalancerAPI) List(ctx context.Context) (LoxiModel, error) {
	lbListModel := l.GetListModel()

	resp := l.client.GET(l.resource).SubResource("all").Do(ctx).UnMarshal(lbListModel)
	if resp.err != nil {
		fmt.Println(resp.err)
		return lbListModel, resp.err
	}

	return lbListModel, nil
}

func (l *LoadBalancerAPI) Create(ctx context.Context, lbModel LoxiModel) error {
	resp := l.client.POST(l.resource).Body(lbModel).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (l *LoadBalancerAPI) Delete(ctx context.Context, lbModel LoxiModel) error {
	subresources, err := l.MakeDeletedSubResource(l.deleteKey, lbModel)
	if err != nil {
		return err
	}

	queryParam, err := l.MakeQueryParam(lbModel)
	if err != nil {
		return err
	}

	resp := l.client.DELETE(l.resource).SubResource(subresources).Query(queryParam).Body(lbModel).Do(ctx)
	if resp.statusCode != http.StatusOK {
		if resp.err != nil {
			return resp.err
		}
	}

	return nil
}
