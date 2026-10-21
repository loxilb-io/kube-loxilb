package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestJWTAuthProfileCreatePostsTheGatewayBody(t *testing.T) {
	var got map[string]any
	c := readBackClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/netlox/v1/config/ai/jwtauthprofile" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"Success"}`))
	})

	err := c.JWTAuthProfile().Create(context.Background(), &JWTAuthProfileModel{
		Name: "kc", Issuer: "https://id.example/realms/inference", Audiences: []string{"inference-api"},
		RolesClaim: "realm_access.roles", ForwardIdentity: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for key, want := range map[string]any{
		"name": "kc", "issuer": "https://id.example/realms/inference", "roles_claim": "realm_access.roles",
		"forward_identity": true,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	if _, ok := got["jwks_url"]; ok {
		t.Error("an unset field went out")
	}
}

// The two refusals a delete can meet keep their status, since they mean
// different things: already gone, and still in use.
func TestJWTAuthProfileDeleteKeepsTheStatus(t *testing.T) {
	for status, check := range map[int]func(error) bool{
		http.StatusNotFound: IsNotFound,
		http.StatusConflict: func(err error) bool { return StatusCodeOf(err) == http.StatusConflict },
	} {
		c := readBackClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/netlox/v1/config/ai/jwtauthprofile/kc" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"result":"jwt auth profile kc is referenced by rule(s): r1"}`))
		})
		if err := c.JWTAuthProfile().Delete(context.Background(), "kc"); !check(err) {
			t.Errorf("HTTP %d: Delete = %v", status, err)
		}
	}
}
