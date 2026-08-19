package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
)

// Operating statuses the gateway can report for a rule.
//
// deriveOperatingStatus is the only producer and returns exactly these four.
// The model's enum also lists ERROR, but nothing emits it, so a branch for it
// would be dead code - callers should treat anything unrecognised defensively
// instead.
const (
	OperatingStatusOnline    = "ONLINE"
	OperatingStatusDegraded  = "DEGRADED"
	OperatingStatusOffline   = "OFFLINE"
	OperatingStatusNoMonitor = "NO_MONITOR"
)

// LoadBalancerStatusModel - response of the per-rule status sub-resource.
type LoadBalancerStatusModel struct {
	// AdminStateUp - always its default here: kube-loxilb never sets the field
	// that would change it.
	AdminStateUp bool `json:"adminStateUp,omitempty"`
	// OperatingStatus - one of the four constants above.
	//
	// NO_MONITOR is checked before endpoint health, so a service that has not
	// enabled liveness reports NO_MONITOR and carries no health information at
	// all. OFFLINE covers both "no endpoints" and "every endpoint down"; from
	// Kubernetes both mean the same thing, no traffic.
	OperatingStatus string `json:"operatingStatus,omitempty"`
	// LastUpdated - stable while the rule is unchanged, by design, so it works
	// as a cheap change key.
	LastUpdated string `json:"lastUpdated,omitempty"`
}

func (l *LoadBalancerStatusModel) GetKeyStruct() LoxiModel {
	return nil
}

// Status - read the operating status of one rule.
//
// Gateway-only: plain upstream loxilb has no such sub-resource and answers 404,
// which is why callers must check the flavor before asking. A 404 also means
// "no such rule" on a gateway, so the status code is returned rather than
// folded into the model - the two cases need telling apart and a GET otherwise
// unmarshals an error body into zero values without complaint.
func (l *LoadBalancerAPI) Status(ctx context.Context, externalIP string, port uint16, protocol string) (*LoadBalancerStatusModel, error) {
	subResource := path.Join(
		"externalipaddress", externalIP,
		"port", strconv.Itoa(int(port)),
		"protocol", protocol,
		"status",
	)

	resp := l.client.GET(l.resource).SubResource(subResource).Do(ctx)
	if resp.err != nil {
		return nil, resp.err
	}
	if resp.statusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.statusCode, Message: resultMessage(resp.body, resp.statusCode)}
	}

	statusModel := &LoadBalancerStatusModel{}
	if resp = resp.UnMarshal(statusModel); resp.err != nil {
		return nil, resp.err
	}

	return statusModel, nil
}

// resultMessage - loxilb's own reason from an error body, or the status text.
func resultMessage(body []byte, statusCode int) string {
	var result LoxiResult
	if err := json.Unmarshal(body, &result); err == nil && result.Result != "" {
		return result.Result
	}

	return http.StatusText(statusCode)
}
