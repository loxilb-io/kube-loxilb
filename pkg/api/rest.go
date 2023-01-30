package api

import (
	"net/http"
	"net/url"
)

type RESTClient struct {
	baseURL  *url.URL
	provider string
	version  string
	Client   *http.Client
}

func NewRESTClient(baseURL *url.URL, provider, version string, client *http.Client) (*RESTClient, error) {
	newBaseURL := *baseURL

	return &RESTClient{
		baseURL:  &newBaseURL,
		provider: provider,
		version:  version,
		Client:   client,
	}, nil
}

func (r *RESTClient) GET(resource string) *LoxiRequest {
	return r.CreateRequest(http.MethodGet, resource)
}

func (r *RESTClient) POST(resource string) *LoxiRequest {
	return r.CreateRequest(http.MethodPost, resource)
}

func (r *RESTClient) DELETE(resource string) *LoxiRequest {
	return r.CreateRequest(http.MethodDelete, resource)
}

func (r *RESTClient) CreateRequest(method string, resource string) *LoxiRequest {
	return NewLoxiRequest(method, resource, r)
}

func (r *RESTClient) GetBaseURL() string {
	return r.baseURL.String()
}
