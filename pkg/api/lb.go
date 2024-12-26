package api

import (
	"context"
	"fmt"
	"net/http"
)

type EpSelect uint

const (
	// LbSelRr - select the lb end-points based on round-robin
	LbSelRr EpSelect = iota
	// LbSelHash - select the lb end-points based on hashing
	LbSelHash
	// LbSelPrio - select the lb based on weighted round-robin
	LbSelPrio
	// LbSelRrPersist - persist connectons from same client
	LbSelRrPersist
	// LbSelLeastConnections - select client based on least connections
	LbSelLeastConnections
	// LbSelN2 - select client based on N2 interface contents
	LbSelN2
	// LbSelN3 - select client based on N2 interface contents
	LbSelN3
)

type LbMode int32
type LbOP int32

const (
	// LBOPAdd - Add te LB rule (replace if existing)
	LBOPAdd LbOP = iota
	// LBModeOneArm - Attach End-Points
	LBOPAttach
	// LBOPDetach - Detach End-Points
	LBOPDetach
)

type LoadBalancerListModel struct {
	Item []LoadBalancerModel `json:"lbAttr"`
}

func (lbListModel *LoadBalancerListModel) GetKeyStruct() LoxiModel {
	return nil
}

type LbAllowedSrcIPArg struct {
	// Prefix - Allowed Prefix
	Prefix string `json:"prefix"`
}

type LoadBalancerModel struct {
	Service      LoadBalancerService    `json:"serviceArguments"`
	SecondaryIPs []LoadBalancerSecIp    `json:"secondaryIPs"`
	SrcIPs       []LbAllowedSrcIPArg    `json:"allowedSources"`
	Endpoints    []LoadBalancerEndpoint `json:"endpoints"`
}

func (lbModel *LoadBalancerModel) GetKeyStruct() LoxiModel {
	return &lbModel.Service
}

type LoadBalancerService struct {
	ExternalIP   string   `json:"externalIP" key:"externalipaddress"`
	PrivateIP    string   `json:"privateIP" key:"privateipaddress"`
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
	Security     int32    `json:"security,omitempty"`
	Name         string   `json:"name,omitempty"`
	Oper         LbOP     `json:"oper,omitempty"`
	Host         string   `json:"host,omitempty"`
	PpV2         bool     `json:"proxyprotocolv2,omitempty"`
	Egress       bool     `json:"egress,omitempty"`
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

func (l *LoadBalancerAPI) List(ctx context.Context) (*LoadBalancerListModel, error) {
	lbListModel := &LoadBalancerListModel{}

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

func (l *LoadBalancerAPI) DeleteByName(ctx context.Context, name string) error {
	resp := l.client.DELETE(l.resource).SubResource("name").SubResource(name).Do(ctx)
	if resp.statusCode != http.StatusOK {
		if resp.err != nil {
			return resp.err
		}
	}

	return nil
}
