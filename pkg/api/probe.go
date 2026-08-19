package api

import (
	"fmt"
	"strconv"
	"strings"
)

// EndpointProbe - loxilb-inference-gateway-only health-monitor fields on an
// endpoint. Upstream loxilb has none of them.
//
// Like the P/D role fields these sit flat inside each endpoints[] object, so
// this must stay embedded. Every field is a value type with omitempty, which
// keeps an unconfigured endpoint byte-identical to before and keeps the shallow
// clone in stripGatewayEndpointFields correct.
//
// Precedence is the gateway's, not ours: a field set here wins, and the older
// probereq / proberesp annotations stay as the escape hatch. The gateway says
// so in its own spec ("probeReq/probeResp retained as the escape hatch") and
// implements it in rules.go - urlPath beats probereq, expectedCodes replaces
// the proberesp substring match.
type EndpointProbe struct {
	// HTTPMethod - probe method. Empty means the gateway's default, GET.
	HTTPMethod string `json:"httpMethod,omitempty"`
	// URLPath - probe path, e.g. "/healthz". Empty falls back to probereq,
	// then to "/".
	URLPath string `json:"urlPath,omitempty"`
	// ExpectedCodes - Octavia expected_codes: "200", "200,202" or "200-204".
	// Empty means "200", and replaces the proberesp substring match when set.
	ExpectedCodes string `json:"expectedCodes,omitempty"`
	// HTTPVersion - "1.0" or "1.1". At "1.1" a Host header is sent, carrying
	// DomainName when set and the member address otherwise.
	HTTPVersion string `json:"httpVersion,omitempty"`
	// DomainName - TLS SNI for HTTPS monitors, and the Host header at
	// HTTPVersion 1.1.
	DomainName string `json:"domainName,omitempty"`
}

// IsSet - whether any health-monitor field carries a value.
func (p EndpointProbe) IsSet() bool { return p != EndpointProbe{} }

// SwitchesProberMode - whether this configuration flips the gateway's prober
// out of legacy mode.
//
// The prober does not merge old and new configuration field by field: it picks
// a mode, and any one of these switches the whole probe over. In the new mode
// the response check becomes a status-code match against ExpectedCodes,
// defaulting to "200", and proberesp is not consulted at all. So adding one of
// these fields silently retires a body-substring check the operator never
// touched.
//
// Mirrors the condition in the gateway's rules.go. Note the asymmetry:
// httpVersion only counts at "1.1" - an explicit "1.0" on its own leaves the
// legacy prober in place.
func (p EndpointProbe) SwitchesProberMode() bool {
	return p.ExpectedCodes != "" ||
		p.HTTPMethod != "" ||
		p.URLPath != "" ||
		p.HTTPVersion == "1.1" ||
		p.DomainName != ""
}

// EffectiveExpectedCodes - the codes the gateway will actually match once the
// prober is in structured mode.
func (p EndpointProbe) EffectiveExpectedCodes() string {
	if p.ExpectedCodes != "" {
		return p.ExpectedCodes
	}

	return "200"
}

// HTTP methods a health monitor may use. The gateway does not constrain this,
// but an unlisted method in a probe is a typo far more often than an intent,
// and a mistyped method silently fails every check.
var probeHTTPMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {},
	"DELETE": {}, "OPTIONS": {}, "TRACE": {}, "CONNECT": {},
}

// Validate - reject values the gateway would take literally and then fail on.
func (p EndpointProbe) Validate() error {
	if p.HTTPMethod != "" {
		if _, ok := probeHTTPMethods[p.HTTPMethod]; !ok {
			return fmt.Errorf("httpMethod %q is not an HTTP method (use upper case, e.g. GET)", p.HTTPMethod)
		}
	}

	if p.URLPath != "" && !strings.HasPrefix(p.URLPath, "/") {
		return fmt.Errorf("urlPath %q must start with \"/\"", p.URLPath)
	}

	switch p.HTTPVersion {
	case "", "1.0", "1.1":
	default:
		return fmt.Errorf("httpVersion %q must be \"1.0\" or \"1.1\"", p.HTTPVersion)
	}

	if strings.ContainsAny(p.DomainName, " \t/") {
		return fmt.Errorf("domainName %q must be a bare host name", p.DomainName)
	}

	return validateExpectedCodes(p.ExpectedCodes)
}

// validateExpectedCodes - Octavia expected_codes syntax: a single code, a
// comma-separated list, or a single inclusive range.
func validateExpectedCodes(codes string) error {
	if codes == "" {
		return nil
	}

	malformed := fmt.Errorf("expectedCodes %q must be a code, a list or a range - \"200\", \"200,202\" or \"200-204\"", codes)

	parseCode := func(s string) (int, error) {
		v, err := strconv.Atoi(s)
		if err != nil || v < 100 || v > 599 {
			return 0, malformed
		}
		return v, nil
	}

	if lo, hi, isRange := strings.Cut(codes, "-"); isRange {
		if strings.Contains(codes, ",") {
			return malformed
		}
		low, err := parseCode(lo)
		if err != nil {
			return err
		}
		high, err := parseCode(hi)
		if err != nil {
			return err
		}
		if low > high {
			return fmt.Errorf("expectedCodes %q is an empty range", codes)
		}
		return nil
	}

	for _, code := range strings.Split(codes, ",") {
		if _, err := parseCode(code); err != nil {
			return err
		}
	}

	return nil
}
