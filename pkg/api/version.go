package api

import (
	"context"
)

// ProductInferenceGateway - the `product` value reported by loxilb-inference-gateway.
//
// Flavor detection convention (loxilb-inference-gateway commit f5ba913b): the
// gateway stamps this string into GET /netlox/v1/version, while upstream loxilb
// -- and gateway builds predating the field -- omit `product` entirely. Clients
// MUST treat its absence as plain upstream loxilb.
const ProductInferenceGateway = "loxilb-inference-gateway"

// FeatureEndpointPicker - the `features` entry of a gateway build that accepts
// and applies the eppEndpoint/eppFailureMode/eppTimeoutMs/eppPlaintext rule
// fields. Builds without it drop those fields silently, so it is checked before
// a rule naming an Endpoint Picker is sent.
const FeatureEndpointPicker = "epp"

// VersionModel - response of GET /netlox/v1/version.
type VersionModel struct {
	Version   string `json:"version,omitempty"`
	BuildInfo string `json:"buildInfo,omitempty"`
	// Product - API flavor identifier. Empty for upstream loxilb.
	Product string `json:"product,omitempty"`
	// Features - gateway-only capabilities this build implements. Absent on
	// upstream loxilb and on gateway builds that predate the field, which
	// therefore support none of them.
	Features []string `json:"features,omitempty"`
}

func (v *VersionModel) GetKeyStruct() LoxiModel {
	return nil
}

type VersionAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newVersionAPI(r *RESTClient) *VersionAPI {
	return &VersionAPI{
		resource: "version",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (v *VersionAPI) GetModel() LoxiModel {
	return &VersionModel{}
}

// Get - fetch version/flavor information. The endpoint takes no sub-resource,
// so the name argument is ignored; it exists to keep the LoxiAPI-ish shape.
func (v *VersionAPI) Get(ctx context.Context, name string) (*VersionModel, error) {
	versionModel := &VersionModel{}

	resp := v.client.GET(v.resource).Do(ctx).UnMarshal(versionModel)
	if resp.err != nil {
		return nil, resp.err
	}

	return versionModel, nil
}
