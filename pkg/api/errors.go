package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError - a rejection from loxilb, carrying both the status code and the
// reason loxilb gave.
//
// loxilb puts a generic category in the response's `message` and the specific
// reason in `result` - "kv-exact zmq mode requires pd_disagg_mode=true" and the
// like. Keeping the code alongside that text is what lets a caller tell a
// duplicate from a bad request, and a request the user must fix from one worth
// retrying, without reading the wording.
type APIError struct {
	// StatusCode - HTTP status, or 0 when the peer never answered.
	StatusCode int
	// Message - loxilb's own reason, or the status text when it sent none.
	Message string
}

func (e *APIError) Error() string {
	if e.StatusCode == 0 {
		return e.Message
	}

	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
}

// StatusCodeOf - the status loxilb answered with, or 0 if this is not a
// rejection from loxilb.
func StatusCodeOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}

	return 0
}

// IsConflict - the peer is saying the resource is already there.
//
// 409 is authoritative: loxilb and loxilb-inference-gateway both map every
// "exists" variant onto it through the same ResultErrorResponseErrorMessage
// helper, verified on both mains.
//
// The wording test is kept for peers that answer without a usable status, but
// never for a 5xx. Matching text alone - which is what this used to do - can
// swallow a genuine server failure whose message merely contains the word, and
// the set of distinct messages only grows.
func IsConflict(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	if apiErr.StatusCode == http.StatusConflict {
		return true
	}
	if apiErr.StatusCode >= http.StatusInternalServerError {
		return false
	}

	return strings.Contains(strings.ToLower(apiErr.Message), "exist")
}

// IsClientError - loxilb rejected the request itself, so retrying it unchanged
// will fail the same way. Something has to change in the Service.
func IsClientError(err error) bool {
	code := StatusCodeOf(err)

	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}
