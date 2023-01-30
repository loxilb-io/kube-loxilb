package api

import (
	"context"
	"fmt"
)

type HealthCheckAPI struct {
	resource  string
	deleteKey []string
	client    *RESTClient
	APICommonFunc
}

func newHealthCheckAPI(r *RESTClient) *HealthCheckAPI {
	return &HealthCheckAPI{
		resource:  "",
		deleteKey: []string{},
		client:    r,
	}
}

func (h *HealthCheckAPI) Get(ctx context.Context, name string) (LoxiModel, error) {
	resp := h.client.GET(h.resource).SubResource(name).Do(ctx)
	if resp.err != nil {
		fmt.Println(resp.err)
		return nil, resp.err
	}

	return nil, nil
}

func (h *HealthCheckAPI) List(ctx context.Context) (LoxiModel, error) {
	return nil, nil
}

func (h *HealthCheckAPI) Create(ctx context.Context, lbModel LoxiModel) error {
	return nil
}

func (h *HealthCheckAPI) Delete(ctx context.Context, lbModel LoxiModel) error {
	return nil
}
