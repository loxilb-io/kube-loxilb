package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
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

func TestDetectFlavorFeatures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"epp listed", `{"product":"loxilb-inference-gateway","features":["epp"]}`, true},
		{"epp among others", `{"product":"loxilb-inference-gateway","features":["foo","epp"]}`, true},
		{"build predating the field", `{"product":"loxilb-inference-gateway"}`, false},
		{"empty list", `{"product":"loxilb-inference-gateway","features":[]}`, false},
		{"other features only", `{"product":"loxilb-inference-gateway","features":["epp/v2"]}`, false},
		{"plain loxilb", `{"version":"0.9.8.8"}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := versionServer(t, tt.body)
			defer srv.Close()

			c := newTestLoxiClient(t, srv)
			if c.SupportsFeature(FeatureEndpointPicker) {
				t.Fatal("an undetected peer must support nothing")
			}
			c.DetectFlavor(context.Background())

			if got := c.SupportsFeature(FeatureEndpointPicker); got != tt.want {
				t.Errorf("SupportsFeature(%q) = %v, want %v", FeatureEndpointPicker, got, tt.want)
			}
		})
	}
}

// A peer coming back up may be a different binary: RefreshFlavor replaces the
// cached answer, which DetectFlavor alone never does.
func TestRefreshFlavorPicksUpAnUpgrade(t *testing.T) {
	body := `{"product":"loxilb-inference-gateway"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	c.DetectFlavor(context.Background())
	if c.SupportsFeature(FeatureEndpointPicker) {
		t.Fatal("the first build does not list epp")
	}

	body = `{"product":"loxilb-inference-gateway","features":["epp"]}`
	c.DetectFlavor(context.Background())
	if c.SupportsFeature(FeatureEndpointPicker) {
		t.Fatal("DetectFlavor must not re-query once it has an answer")
	}

	c.RefreshFlavor(context.Background())
	if !c.SupportsFeature(FeatureEndpointPicker) {
		t.Error("RefreshFlavor must replace the cached answer")
	}
}

// A failed re-query keeps what the peer said before. Forgetting it would make a
// peer that is merely down read as plain loxilb and refuse every gateway rule.
func TestRefreshFlavorFailureKeepsPreviousAnswer(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"product":"loxilb-inference-gateway","features":["epp"]}`))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	c.DetectFlavor(context.Background())

	fail = true
	c.RefreshFlavor(context.Background())

	if !c.IsInferenceGateway() || !c.SupportsFeature(FeatureEndpointPicker) {
		t.Errorf("a failed refresh forgot the previous answer (product %q)", c.Product())
	}
}

// onHealthy drives the flavor through a peer coming up: asked once on the way
// up, retried while that failed, and the peer announced again only when the
// late answer differs from what the first re-sync used.
func TestOnHealthyRetriesAFailedRefreshAndReannounces(t *testing.T) {
	var mu sync.Mutex
	fail := true
	body := `{"product":"loxilb-inference-gateway","features":["epp"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	set := func(f bool) { mu.Lock(); fail = f; mu.Unlock() }

	if !c.onHealthy() {
		t.Fatal("coming up must announce the peer")
	}
	if c.FlavorDetected() {
		t.Fatal("the version query failed, nothing should be cached")
	}
	if c.onHealthy() {
		t.Error("a retry that fails again must not announce")
	}

	set(false)
	if !c.onHealthy() {
		t.Error("the late answer differs from the re-sync's (none), so the peer must be announced again")
	}
	if !c.SupportsFeature(FeatureEndpointPicker) {
		t.Error("the late answer was not stored")
	}
	if c.onHealthy() {
		t.Error("with nothing pending a healthy tick must not announce")
	}
}

// A late answer identical to the cached one needs no second re-sync.
func TestOnHealthyQuietWhenTheLateAnswerIsUnchanged(t *testing.T) {
	var mu sync.Mutex
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"product":"loxilb-inference-gateway","features":["epp"]}`))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	c.DetectFlavor(context.Background())

	// Seen down, then back up while /version still fails.
	mu.Lock()
	fail = true
	mu.Unlock()
	if !c.onHealthy() {
		t.Fatal("coming up must announce the peer")
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	if c.onHealthy() {
		t.Error("the late answer equals the one the re-sync used; no second announcement")
	}
}
