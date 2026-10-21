package api

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// ruleIDNamespace - the UUIDv5 namespace of the ids kube-loxilb gives its rules
// (ff9cab26-8749-4d8e-978c-2d2d48089422).
var ruleIDNamespace = [16]byte{
	0xff, 0x9c, 0xab, 0x26, 0x87, 0x49, 0x4d, 0x8e,
	0x97, 0x8c, 0x2d, 0x2d, 0x48, 0x08, 0x94, 0x22,
}

// RuleID - the stable id of a rule on loxilb-inference-gateway: a UUIDv5 of
// everything the gateway keys a rule by, and its name.
//
// The name alone is not enough: every port of a Service shares it, and the
// gateway refuses an id already bound to another rule. The key alone is not
// enough either: a change of host, path or model makes a different rule, and
// a peer that was down meanwhile still has the old one under the old id.
// Hashing the whole key gives each rule its own id and lets the same rule get
// the same id on every sync and after a restart.
func RuleID(s *LoadBalancerService) string {
	name := strings.Join([]string{
		s.Name, s.ExternalIP, strconv.Itoa(int(s.Port)), s.Protocol,
		s.Host, s.PathPrefix, string(s.PathMatchMode), s.ModelName,
	}, "\x00")

	h := sha1.New()
	h.Write(ruleIDNamespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var u [16]byte
	copy(u[:], sum)
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// RuleReadBack - the fields of a rule read back from a gateway that are
// checked against what was sent: its key, the ones the gateway will not change
// in place, and the credential policy.
//
// Deliberately not LoadBalancerService: the gateway's answer carries far more
// than kube-loxilb sends, and one field typed differently there would fail the
// whole read for fields nobody checks.
type RuleReadBack struct {
	ID             string `json:"id"`
	ExternalIP     string `json:"externalIP"`
	Port           uint16 `json:"port"`
	Protocol       string `json:"protocol"`
	Host           string `json:"host"`
	PathPrefix     string `json:"path_prefix"`
	ModelName      string `json:"model_name"`
	Mode           LbMode `json:"mode"`
	Security       int32  `json:"security"`
	Egress         bool   `json:"egress"`
	KvEngineType   string `json:"kvEngineType"`
	APIKeyAuth     string `json:"api_key_auth"`
	JwtAuthProfile string `json:"jwt_auth_profile"`
}

type ruleReadBackModel struct {
	Service RuleReadBack `json:"serviceArguments"`
}

func (r *ruleReadBackModel) GetKeyStruct() LoxiModel {
	return nil
}

// GetByID - read one rule from loxilb-inference-gateway by its id.
//
// A rule not stored under the id answers 404, returned as such: a GET
// otherwise unmarshals the error body into zero values without complaint.
func (l *LoadBalancerAPI) GetByID(ctx context.Context, id string) (*RuleReadBack, error) {
	resp := l.client.GET(l.resource).SubResource(path.Join("id", id)).Do(ctx)
	if resp.err != nil {
		return nil, resp.err
	}
	if resp.statusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.statusCode, Message: resultMessage(resp.body, resp.statusCode)}
	}

	model := &ruleReadBackModel{}
	if resp = resp.UnMarshal(model); resp.err != nil {
		return nil, resp.err
	}

	return &model.Service, nil
}
