package api

import (
	"context"
	"fmt"
	"net/http"
)

type SniCertModel struct {
	Hostname string `json:"hostname"`
}

func (sniCertModel *SniCertModel) GetKeyStruct() LoxiModel {
	return sniCertModel
}

type SniCertListModel struct {
	Item []SniCertModel `json:"sniAttr"`
}

func (sniCertListModel *SniCertListModel) GetKeyStruct() LoxiModel {
	return nil
}

type SniCertAPI struct {
	resource  string
	provider  string
	version   string
	deleteKey []string
	client    *RESTClient
	APICommonFunc
}

func newSniCertAPI(r *RESTClient) *SniCertAPI {
	return &SniCertAPI{
		resource:  "sni/certificates",
		deleteKey: []string{}, // TODO
		provider:  r.provider,
		version:   r.version,
		client:    r,
	}
}

func (l *SniCertModel) GetModel() LoxiModel {
	return &SniCertModel{}
}

func (l *SniCertModel) GetListModel() LoxiModel {
	return &SniCertListModel{}
}

func (l *SniCertAPI) Create(ctx context.Context, lbModel LoxiModel) error {
	resp := l.client.POST(l.resource).Body(lbModel).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (l *SniCertAPI) List(ctx context.Context) (*SniCertListModel, error) {
	sniListModel := &SniCertListModel{}

	resp := l.client.GET(l.resource).Do(ctx).UnMarshal(sniListModel)
	if resp.err != nil {
		fmt.Println(resp.err)
		return sniListModel, resp.err
	}

	return sniListModel, nil
}

func (l *SniCertAPI) Delete(ctx context.Context, lbModel LoxiModel) error {
	resp := l.client.DELETE(l.resource).Body(lbModel).Do(ctx)
	if resp.statusCode != http.StatusOK {
		if resp.err != nil {
			return resp.err
		}
	}

	return nil
}
