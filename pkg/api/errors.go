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

// notFoundPhrases - the wordings loxilb classifies as 404, checked before the
// conflict wordings because the server checks them in that order too.
//
// This ordering is the whole point. "not-exists" contains "exist", so a plain
// substring test - which is what the duplicate check used to be - reads a
// missing resource as an existing one and silently treats a real 404 as an
// idempotent no-op. The two cases the wording cannot separate are exactly the
// two the status code separates cleanly.
var notFoundPhrases = []string{"not-exists", "not exists", "not found", "no such", "not such"}

// IsConflict - the peer is saying the resource is already there.
//
// 409 is authoritative. loxilb and loxilb-inference-gateway share one
// ResultErrorResponseErrorMessage helper, verified on both mains, and it sorts
// the "already there" wordings to 409 and the "not there" wordings to 404.
//
// The wording test below only covers a peer old enough not to make that
// distinction. It never applies to a 5xx, and it defers to the not-found
// family, so it cannot reproduce either of the failures the status code fixes.
func IsConflict(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	if apiErr.StatusCode == http.StatusConflict {
		return true
	}
	if apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode >= http.StatusInternalServerError {
		return false
	}

	message := strings.ToLower(apiErr.Message)
	for _, phrase := range notFoundPhrases {
		if strings.Contains(message, phrase) {
			return false
		}
	}

	return strings.Contains(message, "exist")
}

// IsClientError - loxilb rejected the request itself, so retrying it unchanged
// will fail the same way. Something has to change in the Service.
func IsClientError(err error) bool {
	code := StatusCodeOf(err)

	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}
