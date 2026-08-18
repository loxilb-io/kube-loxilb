package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// newTestLoxiClient builds a LoxiClient pointed at srv without starting the
// health-check goroutine that NewLoxiClient would.
func newTestLoxiClient(t *testing.T, srv *httptest.Server) *LoxiClient {
	t.Helper()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	restClient, err := NewRESTClient(base, "netlox", "v1", srv.Client())
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}

	return &LoxiClient{RestClient: restClient, Host: base.Host}
}

func versionServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/netlox/v1/version" {
			t.Errorf("unexpected version path %q, want /netlox/v1/version", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestDetectFlavorInferenceGateway(t *testing.T) {
	srv := versionServer(t, `{"version":"0.9.8.6","buildInfo":"x","product":"loxilb-inference-gateway"}`)
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	if c.FlavorDetected() {
		t.Fatal("flavor must be undetected before the first version query")
	}

	c.DetectFlavor(context.Background())

	if !c.FlavorDetected() {
		t.Fatal("flavor must be detected after a successful version query")
	}
	if !c.IsInferenceGateway() {
		t.Errorf("IsInferenceGateway() = false, want true (product %q)", c.Product())
	}
}

// A `product`-less response is the documented signal for plain upstream loxilb.
func TestDetectFlavorPlainLoxilb(t *testing.T) {
	srv := versionServer(t, `{"version":"0.9.8.8","buildInfo":"x"}`)
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	c.DetectFlavor(context.Background())

	if !c.FlavorDetected() {
		t.Fatal("an answered version query counts as detected even without `product`")
	}
	if c.IsInferenceGateway() {
		t.Errorf("IsInferenceGateway() = true for a product-less response, want false")
	}
	if c.Product() != "" {
		t.Errorf("Product() = %q, want empty", c.Product())
	}
}

// A failed query must stay undetected so a later health tick retries, and must
// report as non-gateway meanwhile.
func TestDetectFlavorQueryFailureStaysUndetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	c.DetectFlavor(context.Background())

	if c.FlavorDetected() {
		t.Error("a failed version query must leave the flavor undetected")
	}
	if c.IsInferenceGateway() {
		t.Error("an undetected peer must report as non-gateway")
	}
}

// Detection is cached: once answered, later ticks must not re-query.
func TestDetectFlavorQueriesOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"product":"loxilb-inference-gateway"}`))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	for i := 0; i < 3; i++ {
		c.DetectFlavor(context.Background())
	}

	if calls != 1 {
		t.Errorf("version endpoint queried %d times, want 1", calls)
	}
}
