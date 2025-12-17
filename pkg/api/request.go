package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
)

type LoxiRequest struct {
	method      string
	resource    string
	subResource []string
	queryArgs   map[string]string
	body        io.Reader
	contentType string
	err         error
	client      *RESTClient
}

type LoxiResult struct {
	Result string `json:"result"`
}

func NewLoxiRequest(method, resource string, client *RESTClient) *LoxiRequest {
	return &LoxiRequest{
		method:   method,
		resource: resource,
		client:   client,
	}
}

// URL make REST API url
func (l *LoxiRequest) URL() *url.URL {
	resourcePath := l.getPath()
	newURL := &url.URL{}
	if l.client.baseURL != nil {
		*newURL = *l.client.baseURL
	}
	newURL.Path = resourcePath

	if l.queryArgs != nil {
		q := newURL.Query()
		for k, v := range l.queryArgs {
			q.Add(k, v)
		}
		newURL.RawQuery = q.Encode()
	}

	return newURL
}

func (l *LoxiRequest) Body(obj LoxiModel) *LoxiRequest {
	if l.err != nil {
		return l
	}

	switch t := obj.(type) {
	case io.Reader:
		l.body = t
	case LoxiModel:
		newBody, err := json.Marshal(t)
		if err != nil {
			l.err = err
		}
		l.body = bytes.NewBuffer(newBody)
	default:
		what := reflect.TypeOf(t)
		l.err = fmt.Errorf("unknown type(%s) used for body: %+v", what.Name(), obj)
	}

	return l
}

func (l *LoxiRequest) Query(param map[string]string) *LoxiRequest {
	if l.err != nil {
		return l
	}

	if l.queryArgs == nil {
		l.queryArgs = make(map[string]string)
	}

	for key, value := range param {
		l.queryArgs[key] = value
	}

	return l
}

func (l *LoxiRequest) SubResource(subresources ...string) *LoxiRequest {
	l.subResource = append(l.subResource, subresources...)
	return l
}

func (l *LoxiRequest) Do(ctx context.Context) *LoxiResponse {
	result := LoxiResult{}
	if l.err != nil {
		return &LoxiResponse{err: l.err}
	}

	reqURL := l.URL()
	req, err := http.NewRequestWithContext(ctx, l.method, reqURL.String(), l.body)
	if err != nil {
		return &LoxiResponse{err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	if l.contentType != "" {
		req.Header.Set("Content-Type", l.contentType)
	}

	token, _ := GetAccessToken(l.client)
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	resp, err := l.client.Client.Do(req)
	if err != nil {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		return &LoxiResponse{statusCode: statusCode, err: err}
	}

	defer resp.Body.Close()
	respByte, err := io.ReadAll(resp.Body)
	if err != nil {
		return &LoxiResponse{statusCode: resp.StatusCode, err: err}
	}

	// For non-GET requests, validate response
	if req.Method != http.MethodGet {
		// Check HTTP status code first
		if resp.StatusCode != http.StatusOK {
			// Try to parse error message from response body
			if err := json.Unmarshal(respByte, &result); err == nil && result.Result != "" {
				return &LoxiResponse{statusCode: resp.StatusCode, body: respByte, err: errors.New(result.Result)}
			}
			return &LoxiResponse{statusCode: resp.StatusCode, body: respByte, err: errors.New(http.StatusText(resp.StatusCode))}
		}

		// Check result field for successful HTTP responses
		if err := json.Unmarshal(respByte, &result); err != nil {
			return &LoxiResponse{statusCode: resp.StatusCode, err: err}
		}

		if result.Result != "Success" {
			return &LoxiResponse{statusCode: resp.StatusCode, err: errors.New(result.Result)}
		}
	}

	return &LoxiResponse{
		statusCode:  resp.StatusCode,
		body:        respByte,
		err:         nil,
		contentType: resp.Header.Get("content-Type"),
	}
}

func (l *LoxiRequest) getPath() string {
	p := path.Join(l.client.provider, l.client.version)
	if len(l.resource) != 0 {
		p = path.Join(p, strings.ToLower(l.resource))
	}

	if len(l.subResource) != 0 {
		subP := path.Join(l.subResource...)
		p = path.Join(p, subP)
	}

	return p
}
