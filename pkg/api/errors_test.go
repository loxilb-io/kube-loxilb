package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// what loxilb and the gateway both answer for a duplicate rule
			name: "409",
			err:  &APIError{StatusCode: http.StatusConflict, Message: "lbrule-exist error"},
			want: true,
		},
		{
			name: "409 with wording that says nothing about existing",
			err:  &APIError{StatusCode: http.StatusConflict, Message: "cant modify rule kv engine type"},
			want: true,
		},
		{
			// the failure the old substring test would have swallowed
			name: "500 whose wording happens to contain the word",
			err:  &APIError{StatusCode: http.StatusInternalServerError, Message: "datapath sync failed: map does not exist"},
			want: false,
		},
		{
			name: "400 is a real rejection",
			err:  &APIError{StatusCode: http.StatusBadRequest, Message: "kv-exact zmq mode requires pd_disagg_mode=true"},
			want: false,
		},
		{
			// a peer too old to map exists onto 409
			name: "4xx that only says it in words",
			err:  &APIError{StatusCode: http.StatusBadRequest, Message: "lbrule-exist error"},
			want: true,
		},
		{
			// "not-exists" contains "exist" - the case a substring test cannot
			// tell from a duplicate, and the server sorts to 404
			name: "404 not-exists is a missing resource, not a duplicate",
			err:  &APIError{StatusCode: http.StatusNotFound, Message: "lbrule not-exists error"},
			want: false,
		},
		{
			name: "a 4xx that says not found in words",
			err:  &APIError{StatusCode: http.StatusBadRequest, Message: "no such rule"},
			want: false,
		},
		{
			name: "a transport error is not a conflict",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "wrapped",
			err:  fmt.Errorf("create: %w", &APIError{StatusCode: http.StatusConflict, Message: "exists"}),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsConflict(tt.err); got != tt.want {
				t.Errorf("IsConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsClientError(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest: true, http.StatusUnauthorized: true,
		http.StatusForbidden: true, http.StatusNotFound: true, http.StatusConflict: true,
		http.StatusInternalServerError: false, http.StatusServiceUnavailable: false,
		http.StatusOK: false,
	}
	for code, want := range cases {
		if got := IsClientError(&APIError{StatusCode: code}); got != want {
			t.Errorf("IsClientError(%d) = %v, want %v", code, got, want)
		}
	}

	if IsClientError(errors.New("connection refused")) {
		t.Error("a transport error is not a client error")
	}
}

// The reason loxilb gave has to survive to the caller, since that is the whole
// point of reporting it.
func TestAPIErrorKeepsTheReason(t *testing.T) {
	err := &APIError{StatusCode: http.StatusBadRequest, Message: "pd-disagg requires mode=fullproxy"}

	if got := err.Error(); got != "pd-disagg requires mode=fullproxy (HTTP 400)" {
		t.Errorf("Error() = %q", got)
	}
	if got := StatusCodeOf(fmt.Errorf("wrapped: %w", err)); got != http.StatusBadRequest {
		t.Errorf("StatusCodeOf = %d, want 400", got)
	}
	if got := StatusCodeOf(errors.New("x")); got != 0 {
		t.Errorf("StatusCodeOf of a plain error = %d, want 0", got)
	}
}
