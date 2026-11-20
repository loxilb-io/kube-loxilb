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
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// A Gateway's ingress Service carries the ports the Gateway serves through
// loxilb-ingress, each an L4 rule to the ingress controller's node port. When
// the Gateway's direct routes take a port over, that rule has to go first:
// the gateway serves an L4 rule ahead of the host and path rules on the same
// address and port, and once the port is off the Service its node port is
// closed, so new connections would be sent nowhere. The Gateway manager
// releases the port (ReleasePorts) and takes it off the Service only once its
// rule is off every peer.

// ReleasePorts - the TCP ports of a Service the Gateway manager has handed to
// its direct routes, all of them each time. Their rules come off every peer,
// and are not sent while the Service still lists the ports. A port it no
// longer names is sent again.
//
// Reports which of ports no peer has a rule for now. A peer that is down may
// still have one, and no delete reaches it: while one is down, a port with
// rules is not reported, and its rules are deleted once it is back.
func (m *Manager) ReleasePorts(namespace, name string, ports []int32) (map[int32]bool, error) {
	m.lbCacheMu.Lock()
	defer m.lbCacheMu.Unlock()

	cacheKey := GenKey(namespace, name)
	next := make(map[int32]bool, len(ports))
	for _, p := range ports {
		next[p] = true
	}
	for p := range m.releasedPorts[cacheKey] {
		if !next[p] {
			// Its rule goes back on the next sync.
			m.queueKey(cacheKey)
			break
		}
	}
	m.keepReleasedPorts(cacheKey, next)

	released := make(map[int32]bool, len(next))
	for p := range next {
		released[p] = true
	}
	entry, ok := m.lbCache[cacheKey]
	if !ok {
		return released, nil
	}
	held := func() bool {
		some := false
		for _, sp := range entry.LbServicePairs {
			if releasedPair(next, sp) {
				delete(released, int32(sp.Port))
				some = true
			}
		}
		return some
	}
	if !held() {
		return released, nil
	}
	for _, c := range m.LoxiClients.Clients {
		if !c.IsAlive {
			return released, nil
		}
	}

	err := m.dropPairs(cacheKey, func(_ string, sp *LbServicePairEntry) bool { return !releasedPair(next, sp) },
		"is released to the Gateway's direct routes")
	for p := range next {
		released[p] = true
	}
	held()
	return released, err
}

// releasedPair - whether a pair is on one of the released TCP ports.
func releasedPair(ports map[int32]bool, sp *LbServicePairEntry) bool {
	return ports[int32(sp.Port)] && strings.EqualFold(sp.Protocol, string(corev1.ProtocolTCP))
}

// keepReleasedPorts - record the ports released for a Service; nothing once
// none are.
func (m *Manager) keepReleasedPorts(cacheKey string, ports map[int32]bool) {
	if len(ports) == 0 {
		delete(m.releasedPorts, cacheKey)
		return
	}
	if m.releasedPorts == nil {
		m.releasedPorts = map[string]map[int32]bool{}
	}
	m.releasedPorts[cacheKey] = ports
}

// withoutReleasedPorts - the Service as this manager serves it: without the
// ports released to a Gateway's direct routes.
func (m *Manager) withoutReleasedPorts(svc *corev1.Service) *corev1.Service {
	released := m.releasedPorts[GenKey(svc.Namespace, svc.Name)]
	if len(released) == 0 {
		return svc
	}
	served := svc.DeepCopy()
	served.Spec.Ports = nil
	for _, p := range svc.Spec.Ports {
		if (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) && released[p.Port] {
			continue
		}
		served.Spec.Ports = append(served.Spec.Ports, p)
	}
	return served
}

// InstalledPorts - which of the TCP ports of a Service every peer that is up
// has the rules of. A port still released has none to have, nor has a port
// not synced yet; and while no peer is up, none is installed.
//
// A port that goes back from a Gateway's direct routes to its ingress Service
// keeps its direct rules until then: the gateway serves the L4 rule ahead of
// them once it has it, and not before.
func (m *Manager) InstalledPorts(namespace, name string, ports []int32) map[int32]bool {
	m.lbCacheMu.Lock()
	defer m.lbCacheMu.Unlock()

	installed := map[int32]bool{}
	cacheKey := GenKey(namespace, name)
	entry, ok := m.lbCache[cacheKey]
	if !ok {
		return installed
	}
	var alive []string
	for _, c := range m.LoxiClients.Clients {
		if c.IsAlive {
			alive = append(alive, c.Host)
		}
	}
	if len(alive) == 0 {
		return installed
	}

	asked := map[int32]bool{}
	for _, p := range ports {
		if !m.releasedPorts[cacheKey][p] {
			asked[p] = true
		}
	}
	missing := map[int32]bool{}
	for _, sp := range entry.LbServicePairs {
		port := int32(sp.Port)
		if !asked[port] || !strings.EqualFold(sp.Protocol, string(corev1.ProtocolTCP)) {
			continue
		}
		installed[port] = true
		for _, host := range alive {
			if st, ok := sp.peers[host]; !ok || st.phase != peerInstalled {
				missing[port] = true
			}
		}
	}
	for p := range missing {
		delete(installed, p)
	}
	return installed
}

// SetSyncedHook - fn is called each time a Service's sync ends, with its
// namespace and name. It is called with the manager's cache locked, and must
// not call back into the manager.
func (m *Manager) SetSyncedHook(fn func(namespace, name string)) {
	m.syncedHook.Store(fn)
}

func (m *Manager) callSyncedHook(namespace, name string) {
	if fn, ok := m.syncedHook.Load().(func(namespace, name string)); ok && fn != nil {
		fn(namespace, name)
	}
}
