package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// deleteRecorder - answers every DELETE with Success and keeps the URL, which
// is the whole point of these tests: loxilb identifies the rule by the URL, so
// the URL is the contract.
type deleteRecorder struct {
	path  string
	query map[string]string
}

func (d *deleteRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		d.path = r.URL.Path
		d.query = map[string]string{}
		for k, v := range r.URL.Query() {
			d.query[k] = v[0]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"Success"}`))
	}))
}

func inferenceRule() *LoadBalancerModel {
	return &LoadBalancerModel{
		Service: LoadBalancerService{
			ExternalIP:    "12.12.12.100",
			Host:          "12.12.12.100",
			PathPrefix:    "/",
			PathMatchMode: PathMatchModePrefix,
			Port:          8080,
			Protocol:      "tcp",
			Name:          "llm_vllm-qwen3-inference:llb1",
			Sel:           LbSelCHWBL,
			Mode:          LBModeFullProxy,
			AIArgs:        AIArgs{ModelName: "qwen3-32b"},
		},
	}
}

// An ordinary service keeps the URL it has always used. loxilb's path-less
// delete route sets HostUrl to "" before it looks the rule up, which is right
// for a rule that was created without one.
func TestDeleteUsesPlainFormWithoutHost(t *testing.T) {
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()

	lb := &LoadBalancerModel{
		Service: LoadBalancerService{
			ExternalIP: "10.0.0.1",
			Port:       8080,
			Protocol:   "tcp",
			Name:       "default_iperf:llb1",
		},
	}

	if err := newTestLoxiClient(t, srv).LoadBalancer().Delete(context.Background(), lb); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := "/netlox/v1/config/loadbalancer/externalipaddress/10.0.0.1/port/8080/protocol/tcp"
	if rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	if _, ok := rec.query["path_prefix"]; ok {
		t.Errorf("path_prefix must not be sent for a rule with no host: %v", rec.query)
	}
}

// A rule with a host is keyed by it. Deleting it through the path-less form
// looks up a different key and loxilb answers 404 no-rule, so the /hosturl/
// form carries the host and the two path fields that go with it.
func TestDeleteUsesHostURLFormWithHost(t *testing.T) {
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()

	lb := inferenceRule()
	lb.Service.ModelName = "" // a pool that selects on cache locality but names no model

	if err := newTestLoxiClient(t, srv).LoadBalancer().Delete(context.Background(), lb); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := "/netlox/v1/config/loadbalancer/hosturl/12.12.12.100/externalipaddress/12.12.12.100/port/8080/protocol/tcp"
	if rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	if rec.query["path_prefix"] != "/" || rec.query["path_match_mode"] != "prefix" {
		t.Errorf("query = %v, want path_prefix=/ path_match_mode=prefix", rec.query)
	}
}

// model_name is part of the key too. It rides on the same URL as a query
// param, which is what tells two rules sharing a VIP and port apart.
func TestDeleteSendsModelName(t *testing.T) {
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()

	if err := newTestLoxiClient(t, srv).LoadBalancer().Delete(context.Background(), inferenceRule()); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := "/netlox/v1/config/loadbalancer/hosturl/12.12.12.100/externalipaddress/12.12.12.100/port/8080/protocol/tcp"
	if rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	if rec.query["model_name"] != "qwen3-32b" {
		t.Errorf("query = %v, want model_name=qwen3-32b", rec.query)
	}
}

// An empty model name must not be sent: a rule that names no model is matched
// by the request that carries no model_name at all.
func TestDeleteOmitsEmptyModelName(t *testing.T) {
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()

	lb := inferenceRule()
	lb.Service.ModelName = ""

	if err := newTestLoxiClient(t, srv).LoadBalancer().Delete(context.Background(), lb); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := rec.query["model_name"]; ok {
		t.Errorf("model_name must not be sent when the rule names no model: %v", rec.query)
	}
}

// The delete key list is shared by every call, so the hosturl prefix must not
// leak from one delete into the next.
func TestDeleteDoesNotMutateSharedDeleteKey(t *testing.T) {
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()

	client := newTestLoxiClient(t, srv).LoadBalancer()

	hosted := inferenceRule()
	hosted.Service.ModelName = ""
	if err := client.Delete(context.Background(), hosted); err != nil {
		t.Fatalf("Delete hosted: %v", err)
	}

	plain := &LoadBalancerModel{
		Service: LoadBalancerService{ExternalIP: "10.0.0.1", Port: 8080, Protocol: "tcp"},
	}
	if err := client.Delete(context.Background(), plain); err != nil {
		t.Fatalf("Delete plain: %v", err)
	}

	want := "/netlox/v1/config/loadbalancer/externalipaddress/10.0.0.1/port/8080/protocol/tcp"
	if rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
}
