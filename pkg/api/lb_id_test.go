package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func idService() LoadBalancerService {
	return LoadBalancerService{
		Name: "llm_vllm:llb-inst0", ExternalIP: "192.168.0.13", Port: 8080, Protocol: "tcp",
		Host: "192.168.0.13", PathPrefix: "/", PathMatchMode: PathMatchModePrefix,
		AIArgs: AIArgs{ModelName: "qwen3"},
	}
}

var uuidV5 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRuleIDIsAStableUUIDv5(t *testing.T) {
	s := idService()
	id := RuleID(&s)
	if !uuidV5.MatchString(id) {
		t.Errorf("RuleID = %q, not a version 5 UUID", id)
	}
	if again := RuleID(&s); again != id {
		t.Errorf("RuleID changed between calls: %q then %q", id, again)
	}
}

// Every part of the key gives the rule its own id: the ports of one Service
// share a name, and a rule whose host, path or model changed is another rule.
func TestRuleIDDiffersByEveryPartOfTheKey(t *testing.T) {
	base := idService()
	seen := map[string]string{RuleID(&base): "base"}
	for name, change := range map[string]func(*LoadBalancerService){
		"name":       func(s *LoadBalancerService) { s.Name = "llm_other:llb-inst0" },
		"externalIP": func(s *LoadBalancerService) { s.ExternalIP = "192.168.0.14" },
		"port":       func(s *LoadBalancerService) { s.Port = 8081 },
		"protocol":   func(s *LoadBalancerService) { s.Protocol = "udp" },
		"host":       func(s *LoadBalancerService) { s.Host = "" },
		"path":       func(s *LoadBalancerService) { s.PathPrefix = "/v1" },
		"match":      func(s *LoadBalancerService) { s.PathMatchMode = "" },
		"model":      func(s *LoadBalancerService) { s.ModelName = "llama" },
	} {
		s := idService()
		change(&s)
		id := RuleID(&s)
		if other, dup := seen[id]; dup {
			t.Errorf("changing %s gives the same id as %s", name, other)
		}
		seen[id] = name
	}
}

// Nothing sets the id on a rule for plain loxilb, so its payload is unchanged.
func TestUnsetIDStaysOffTheWire(t *testing.T) {
	s := idService()
	payload, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), `"id"`) {
		t.Errorf("payload carries an id it was not given\n%s", payload)
	}
}

func readBackClient(t *testing.T, handler http.HandlerFunc) *LoxiClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := NewRESTClient(base, "netlox", "v1", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return &LoxiClient{RestClient: rc, Url: srv.URL, Host: base.Host}
}

func TestGetByIDReadsTheCheckedFields(t *testing.T) {
	c := readBackClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/netlox/v1/config/loadbalancer/id/abc" {
			t.Errorf("GET %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"serviceArguments":{"id":"abc","externalIP":"192.168.0.13","port":8080,` +
			`"protocol":"tcp","mode":4,"security":0,"egress":false,"host":"192.168.0.13","path_prefix":"/",` +
			`"model_name":"qwen3","kvEngineType":"sglang","api_key_auth":"jwt","jwt_auth_profile":"kc",` +
			`"backend_protocol":"http1","halfCloseEffective":{"mode":"x"}},"endpoints":[{"endpointIP":"10.0.0.1"}]}`))
	})

	got, err := c.LoadBalancer().GetByID(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	want := RuleReadBack{ID: "abc", ExternalIP: "192.168.0.13", Port: 8080, Protocol: "tcp", Mode: LBModeFullProxy,
		Host: "192.168.0.13", PathPrefix: "/", ModelName: "qwen3", KvEngineType: "sglang",
		APIKeyAuth: "jwt", JwtAuthProfile: "kc"}
	if *got != want {
		t.Errorf("GetByID = %+v, want %+v", *got, want)
	}
}

// A rule not stored under the id is a 404 the caller can tell apart, not a
// zero-valued rule.
func TestGetByIDNotFound(t *testing.T) {
	c := readBackClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	got, err := c.LoadBalancer().GetByID(context.Background(), "abc")
	if !IsNotFound(err) || got != nil {
		t.Errorf("GetByID = %+v, %v; want nil and a 404", got, err)
	}
}
