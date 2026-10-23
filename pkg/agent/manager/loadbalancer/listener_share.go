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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// listenerAddrOf - for a Service generated from an InferencePool, the address
// of the Gateway it is served through, named in loadBalancerIP; "" for any
// other Service.
//
// Every pool on a Gateway listener is served on that listener's address and
// port, and the gateway tells their rules apart by model. The address pool
// hands an address and port to one Service, so the pools after the first
// share what the first was given (listenerShare).
func listenerAddrOf(svc *corev1.Service) string {
	if svc.Labels[inferencePoolLabel] == "" {
		return ""
	}
	return svc.Spec.LoadBalancerIP
}

// listenerShare - the pair another pool on the same Gateway listener holds for
// port and proto, for a Service generated from an InferencePool to share; nil
// if there is none, or the Service is not one.
//
// A shared pair is not allocated again: the address and port stay reserved
// with the pool that got them, and go back to the address pool with the last
// Service on them (portInUse).
func (m *Manager) listenerShare(self string, entry *LbCacheEntry, port int32, proto string) *LbServicePairEntry {
	if entry.ListenerAddr == "" {
		return nil
	}
	for key, other := range m.lbCache {
		if key == self || other.ListenerAddr != entry.ListenerAddr {
			continue
		}
		for _, sp := range other.LbServicePairs {
			if sp.Port == uint16(port) && sp.Protocol == proto {
				klog.V(4).Infof("service %s shares %s:%d/%s with %s, on the same Gateway listener",
					self, sp.ExternalIP, port, proto, key)
				return sp
			}
		}
	}
	return nil
}

// portInUse - whether a Service other than self is still cached on the
// address, port and protocol, so the reservation stays with it.
func (m *Manager) portInUse(self, ip string, port uint16, proto string) bool {
	spKey := GenSPKey(ip, port, proto)
	for key, entry := range m.lbCache {
		if key == self {
			continue
		}
		if _, ok := entry.LbServicePairs[spKey]; ok {
			return true
		}
	}
	return false
}
