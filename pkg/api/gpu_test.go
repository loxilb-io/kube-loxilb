package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An error answer is not a status. Read as one, a 404 or 500 with a JSON
// body became Enabled=false: GPU-aware routing "disarmed", a gpuaware rule
// refused and a regression reported for a peer that only failed to answer.
func TestGPUStatusErrorAnswers(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonError(w, status)
			}))
			defer srv.Close()

			model, err := newTestLoxiClient(t, srv).GPU().Status(context.Background())
			if err == nil {
				t.Fatalf("HTTP %d was read as a status: %+v", status, model)
			}
			if got := StatusCodeOf(err); got != status {
				t.Errorf("StatusCodeOf(%v) = %d, want %d", err, got, status)
			}
		})
	}
}

func TestGPUStatusReadsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/netlox/v1/config/gpu/status" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"routing_mode":"gpu_aware","worker_count":2}`))
	}))
	defer srv.Close()

	model, err := newTestLoxiClient(t, srv).GPU().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !model.GPUArmed() || model.WorkerCount != 2 {
		t.Errorf("got %+v, want armed with 2 workers", model)
	}
}
