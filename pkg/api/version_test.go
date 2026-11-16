package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
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

// jsonError - fail the way a go-swagger server does: the status, an
// application/json Content-Type and a JSON body. The header has to be set
// before WriteHeader, which sends it; without it the client never mistakes
// the body for a model, and the failure goes unnoticed in a test.
func jsonError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%d,"message":%q}`, status, http.StatusText(status))
}

// A failure with a JSON body is still a failure. A GET's status went
// unchecked, so {"code":500,...} unmarshalled into an empty VersionModel and
// the peer read as plain loxilb until it next came back from down.
func TestDetectFlavorJSONErrorStaysUndetected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonError(w, status)
			}))
			defer srv.Close()

			c := newTestLoxiClient(t, srv)
			c.DetectFlavor(context.Background())

			if c.FlavorDetected() {
				t.Errorf("HTTP %d with a JSON body was taken for an answer (product %q)", status, c.Product())
			}
		})
	}
}

// The same failure on a peer already known as the gateway keeps that answer.
func TestRefreshFlavorJSONErrorKeepsPreviousAnswer(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var fail atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					jsonError(w, status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"product":"loxilb-inference-gateway","features":["epp"]}`))
			}))
			defer srv.Close()

			c := newTestLoxiClient(t, srv)
			c.DetectFlavor(context.Background())

			fail.Store(true)
			if c.RefreshFlavor(context.Background()) {
				t.Errorf("RefreshFlavor reported success for HTTP %d", status)
			}
			if !c.IsInferenceGateway() || !c.SupportsFeature(FeatureEndpointPicker) {
				t.Errorf("HTTP %d replaced the previous answer (product %q)", status, c.Product())
			}
		})
	}
}

// 404 is the exception: an upstream loxilb old enough to have no /version
// answers it, and has always been read as plain loxilb. Retrying would ask
// such a peer every health tick for good.
func TestDetectFlavorNotFoundIsPlainLoxilb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	if !c.RefreshFlavor(context.Background()) {
		t.Error("a 404 from /version is an answer, RefreshFlavor must report success")
	}
	if !c.FlavorDetected() || c.IsInferenceGateway() {
		t.Errorf("a 404 from /version must read as plain loxilb (detected %v, product %q)", c.FlavorDetected(), c.Product())
	}
}

// A JSON-bodied failure on the way up leaves the refresh pending, so the next
// tick asks again and the late answer is announced.
func TestOnHealthyRetriesAfterAJSONError(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int32
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			jsonError(w, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"product":"loxilb-inference-gateway","features":["epp"]}`))
	}))
	defer srv.Close()

	c := newTestLoxiClient(t, srv)
	if !c.onHealthy() {
		t.Fatal("coming up must announce the peer")
	}
	if c.FlavorDetected() {
		t.Fatalf("the version query failed, nothing should be cached (product %q)", c.Product())
	}

	before := calls.Load()
	c.onHealthy()
	if calls.Load() == before {
		t.Fatal("the next healthy tick did not ask /version again")
	}

	fail.Store(false)
	if !c.onHealthy() {
		t.Error("the late answer differs from the re-sync's (none), so the peer must be announced again")
	}
	if !c.IsInferenceGateway() {
		t.Errorf("the late answer was not stored (product %q)", c.Product())
	}
}
