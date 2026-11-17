/*
 * Copyright (c) 2022 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loadbalancer

import (
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// Gateway-only service annotations that are neither AI routing nor mTLS.
const (
	connectionLimitAnnotation      = "loxilb.io/connection-limit"
	timeoutMemberConnectAnnotation = "loxilb.io/timeout-member-connect"
	timeoutMemberDataAnnotation    = "loxilb.io/timeout-member-data"
	timeoutTCPInspectAnnotation    = "loxilb.io/timeout-tcp-inspect"

	tlsCiphersAnnotation    = "loxilb.io/tls-ciphers"
	tlsVersionsAnnotation   = "loxilb.io/tls-versions"
	alpnProtocolsAnnotation = "loxilb.io/alpn-protocols"

	hstsMaxAgeAnnotation            = "loxilb.io/hsts-max-age"
	hstsIncludeSubdomainsAnnotation = "loxilb.io/hsts-include-subdomains"
	hstsPreloadAnnotation           = "loxilb.io/hsts-preload"

	backendCaCertIDAnnotation     = "loxilb.io/backend-ca-cert-id"
	backendClientCertIDAnnotation = "loxilb.io/backend-client-cert-id"
)

// gatewayArgAnnotations - every annotation handled here, in the order they are
// reported when a plain peer cannot honour them.
var gatewayArgAnnotations = []string{
	connectionLimitAnnotation,
	timeoutMemberConnectAnnotation, timeoutMemberDataAnnotation, timeoutTCPInspectAnnotation,
	tlsCiphersAnnotation, tlsVersionsAnnotation, alpnProtocolsAnnotation,
	hstsMaxAgeAnnotation, hstsIncludeSubdomainsAnnotation, hstsPreloadAnnotation,
	backendCaCertIDAnnotation, backendClientCertIDAnnotation,
}

// getGatewayArgs - build the gateway-only service arguments from annotations.
//
// Only the parse is checked here. Whether a cipher string is understood, a TLS
// version supported, or a certId present in loxilb's registry is not knowable
// from the request, so those go through untouched and loxilb's refusal comes
// back to the Service as an event.
func (m *Manager) getGatewayArgs(svc *corev1.Service) (api.GatewayArgs, api.GatewayTLSLists, error) {
	var args api.GatewayArgs
	var lists api.GatewayTLSLists

	var errs []error
	u32 := func(key string) uint32 {
		v, err := annoInt(svc, key)
		if err != nil {
			errs = append(errs, err)
			return 0
		}
		if v < 0 || v > math.MaxUint32 {
			errs = append(errs, fmt.Errorf("%s must be within 0..%d, got %d", key, uint32(math.MaxUint32), v))
			return 0
		}
		return uint32(v)
	}
	boolVal := func(key string) bool {
		v, err := annoBool(svc, key)
		if err != nil {
			errs = append(errs, err)
		}
		return v
	}

	args.ConnectionLimit = u32(connectionLimitAnnotation)
	args.TimeoutMemberConnect = u32(timeoutMemberConnectAnnotation)
	args.TimeoutMemberData = u32(timeoutMemberDataAnnotation)
	args.TimeoutTCPInspect = u32(timeoutTCPInspectAnnotation)

	args.TLSCiphers = svc.Annotations[tlsCiphersAnnotation]

	args.HSTSMaxAge = u32(hstsMaxAgeAnnotation)
	args.HSTSIncludeSubdomains = boolVal(hstsIncludeSubdomainsAnnotation)
	args.HSTSPreload = boolVal(hstsPreloadAnnotation)

	args.BackendCaCertID = svc.Annotations[backendCaCertIDAnnotation]
	args.BackendClientCertID = svc.Annotations[backendClientCertIDAnnotation]

	lists.TLSVersions = annoList(svc, tlsVersionsAnnotation)
	lists.AlpnProtocols = annoList(svc, alpnProtocolsAnnotation)

	if len(errs) > 0 {
		return api.GatewayArgs{}, api.GatewayTLSLists{}, errs[0]
	}

	return args, lists, nil
}

// annoList - a comma-separated annotation as a list, matching how
// loxilb.io/multus-nets and loxilb.io/secondaryIPs already read.
func annoList(svc *corev1.Service, key string) []string {
	raw := svc.Annotations[key]
	if raw == "" {
		return nil
	}

	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}

// gatewayArgsDowngradeNotice - which of these a plain upstream peer will drop.
//
// None of them has an upstream equivalent to translate to, unlike the probe
// path. They are additive hardening on a rule that still works without them, so
// the rule is programmed and the loss is reported rather than the peer being
// left with nothing.
func gatewayArgsDowngradeNotice(svc *corev1.Service, plainPeers []string) string {
	if len(plainPeers) == 0 {
		return ""
	}

	var dropped []string
	for _, key := range gatewayArgAnnotations {
		if svc.Annotations[key] != "" {
			dropped = append(dropped, key)
		}
	}
	if len(dropped) == 0 {
		return ""
	}

	return fmt.Sprintf("%s are loxilb-inference-gateway only and are not applied on %s; "+
		"the rule is still programmed there, without them",
		strings.Join(dropped, ", "), strings.Join(plainPeers, ", "))
}

// recordGatewayArgNotices - tell the operator which gateway-only service
// arguments a plain peer in the pool will not apply.
func (m *Manager) recordGatewayArgNotices(svc *corev1.Service) {
	notice := gatewayArgsDowngradeNotice(svc, m.plainLoxilbPeers())
	if notice == "" {
		return
	}

	klog.Infof("service %s/%s: %s", svc.Namespace, svc.Name, notice)
	m.recordServiceEvent(svc, corev1.EventTypeNormal, ReasonGatewayArgsDowngraded, notice)
}
