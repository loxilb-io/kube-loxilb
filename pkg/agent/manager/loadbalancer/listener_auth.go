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
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// ErrListenerAuthConflict - the Service shares a listener with an older one
// that declares another credential policy, and is not served there.
var ErrListenerAuthConflict = errors.New("an older Service on the same listener declares another credential policy")

// ErrListenerRouteConflict - the Service is generated from an InferencePool
// and shares a listener with an older pool's Service serving the same model,
// and is not served there.
var ErrListenerRouteConflict = errors.New("an older pool on the same listener serves the same model")

// listenerKey - one listener of loxilb-inference-gateway: address, port and
// protocol. The gateway checks credentials once per listener, before a request
// is matched to a rule by host, path or model.
type listenerKey struct {
	ip    string
	port  uint16
	proto string
}

func (l listenerKey) String() string {
	return fmt.Sprintf("%s:%d/%s", l.ip, l.port, l.proto)
}

// listenersOf - the listeners a Service is on or is headed for: the pairs it
// is cached with, and the addresses it names or was given - loadBalancerIP,
// staticIP, externalIPs, published ingress - on each of its ports.
//
// Several Services share a listener through an address named outright: a
// generated InferencePool Service names its Gateway's address, and the pools
// on one listener share its port (listenerShare), which the address pool hands
// to no other two Services. The declared addresses are
// read from the Service itself, so the answer does not depend on which
// Services the cache has seen since kube-loxilb started.
func (m *Manager) listenersOf(svc *corev1.Service) map[listenerKey]bool {
	listeners := map[listenerKey]bool{}

	if entry, ok := m.lbCache[GenKey(svc.Namespace, svc.Name)]; ok {
		for _, sp := range entry.LbServicePairs {
			listeners[listenerKey{ip: sp.ExternalIP, port: sp.Port, proto: sp.Protocol}] = true
		}
	}

	ips := m.getLBServiceExternalIPs(svc)
	if svc.Spec.LoadBalancerIP != "" {
		ips = append(ips, svc.Spec.LoadBalancerIP)
	}
	for _, ip := range ips {
		for _, port := range svc.Spec.Ports {
			listeners[listenerKey{ip: ip, port: uint16(port.Port), proto: strings.ToLower(string(port.Protocol))}] = true
		}
	}

	return listeners
}

// listenerPolicyOf - the credential policy a Service declares, as written. An
// unset policy and an explicit "disabled" differ, as on the gateway: the
// latter makes it own and strip X-Api-Key.
func listenerPolicyOf(svc *corev1.Service) [2]string {
	return [2]string{svc.Annotations[apiKeyAuthAnnotation], svc.Annotations[jwtAuthProfileAnnotation]}
}

// routeOf - what tells a pool's rule apart from the others on its listener:
// the model it serves. Every pool on a listener has the same host (the
// address) and path ("/"), so two pools naming the same model, or none, make
// the same rule. ok is false for a Service not generated from a pool, whose
// listener the address pool does not share.
func routeOf(svc *corev1.Service) (model string, ok bool) {
	if svc.Labels[inferencePoolLabel] == "" {
		return "", false
	}
	return svc.Annotations[modelNameAnnotation], true
}

// conflictOf - why two Services on one listener cannot both be served there,
// as the reason reported for the one that yields; "" when they can.
func conflictOf(a, b *corev1.Service) string {
	if listenerPolicyOf(a) != listenerPolicyOf(b) {
		return ReasonListenerAuthConflict
	}
	ra, oka := routeOf(a)
	rb, okb := routeOf(b)
	if oka && okb && ra == rb {
		return ReasonListenerRouteConflict
	}
	return ""
}

// declaresFullProxy - whether a Service's rules are fullproxy, the only mode
// whose rules the gateway holds to one credential policy per listener.
func (m *Manager) declaresFullProxy(svc *corev1.Service) bool {
	switch svc.Annotations[lbModeAnnotation] {
	case "fullproxy":
		return true
	case "":
		return api.LbMode(m.networkConfig.SetLBMode) == api.LBModeFullProxy
	default:
		return false
	}
}

// handledHere - whether this manager programs the Service at all; the same
// tests addLoadBalancer starts with.
func (m *Manager) handledHere(svc *corev1.Service) bool {
	if zone := svc.Annotations[zoneSelAnnotation]; zone != "" && zone != m.networkConfig.Zone {
		return false
	}
	if _, multus := svc.Annotations[LoxiMultusServiceAnnotation]; multus {
		return true
	}
	class := svc.Spec.LoadBalancerClass
	return class != nil && *class == m.networkConfig.LoxilbLoadBalancerClass
}

// ageOf - how old a Service counts as when Services on one listener disagree.
// A Service generated from an InferencePool counts as old as its pool: the
// pool manager re-creates the Service if it is deleted, and the pool is what
// the operator wrote.
func (m *Manager) ageOf(svc *corev1.Service) metav1.Time {
	if pool := m.poolOf(svc); pool != nil {
		return pool.CreationTimestamp
	}
	return svc.CreationTimestamp
}

// olderThan - a precedes b; the name breaks a tie, so every sync agrees.
func (m *Manager) olderThan(a, b *corev1.Service) bool {
	ta, tb := m.ageOf(a), m.ageOf(b)
	if !ta.Equal(&tb) {
		return ta.Before(&tb)
	}
	return GenKey(a.Namespace, a.Name) < GenKey(b.Namespace, b.Name)
}

// listenerVerdict - how a Service stands on the listeners it shares.
type listenerVerdict struct {
	// yieldTo - the older Service that holds a listener this one shares and
	// conflicts on; nil if none.
	yieldTo  *corev1.Service
	listener listenerKey
	// reason - what the conflict is: ReasonListenerAuthConflict or
	// ReasonListenerRouteConflict.
	reason string
	// outranks - the younger Services that conflict with this one on a
	// listener where this one holds.
	outranks []*corev1.Service
}

// judgeListeners - compare a fullproxy Service with the others on its
// listeners.
//
// On each listener the policy of the oldest fullproxy Service holds; a
// Service declaring another one yields. Of the pools on a listener that serve
// the same model, the oldest is served and the others yield. This follows
// what the Services declare, not what is installed, so neither the order the
// cache filled in after a restart nor the order rules reached loxilb decides.
func (m *Manager) judgeListeners(svc *corev1.Service) listenerVerdict {
	var verdict listenerVerdict
	if !m.declaresFullProxy(svc) || m.serviceLister == nil {
		return verdict
	}

	mine := m.listenersOf(svc)
	if len(mine) == 0 {
		return verdict
	}
	policy := listenerPolicyOf(svc)

	all, err := m.serviceLister.List(labels.Everything())
	if err != nil {
		return verdict
	}

	// The oldest fullproxy Service on each of this one's listeners.
	oldest := map[listenerKey]*corev1.Service{}
	var rivals []*corev1.Service
	rivalOn := map[*corev1.Service][]listenerKey{}
	for _, other := range all {
		if other.Namespace == svc.Namespace && other.Name == svc.Name {
			continue
		}
		if other.DeletionTimestamp != nil || !m.handledHere(other) || !m.declaresFullProxy(other) {
			continue
		}
		for l := range m.listenersOf(other) {
			if !mine[l] {
				continue
			}
			rivalOn[other] = append(rivalOn[other], l)
			if cur, ok := oldest[l]; !ok || m.olderThan(other, cur) {
				oldest[l] = other
			}
		}
		if len(rivalOn[other]) > 0 {
			rivals = append(rivals, other)
		}
	}

	yield := func(to *corev1.Service, l listenerKey, reason string) {
		if verdict.yieldTo == nil || m.olderThan(to, verdict.yieldTo) {
			verdict.yieldTo, verdict.listener, verdict.reason = to, l, reason
		}
	}
	for l, first := range oldest {
		if m.olderThan(first, svc) && listenerPolicyOf(first) != policy {
			yield(first, l, ReasonListenerAuthConflict)
		}
	}
	// The same model is a conflict with any older pool on the listener, not
	// only the oldest: unlike a policy, it does not follow from agreeing with
	// the oldest.
	for _, other := range rivals {
		if m.olderThan(other, svc) && conflictOf(other, svc) == ReasonListenerRouteConflict {
			yield(other, rivalOn[other][0], ReasonListenerRouteConflict)
		}
	}
	if verdict.yieldTo != nil {
		return verdict
	}

	// This one holds wherever it conflicts with a rival - it is the oldest
	// where the policies differ, and older than a rival serving its model -
	// so the rival yields there.
	for _, other := range rivals {
		switch conflictOf(svc, other) {
		case ReasonListenerAuthConflict:
			for _, l := range rivalOn[other] {
				if first, ok := oldest[l]; !ok || m.olderThan(svc, first) {
					verdict.outranks = append(verdict.outranks, other)
					break
				}
			}
		case ReasonListenerRouteConflict:
			if m.olderThan(svc, other) {
				verdict.outranks = append(verdict.outranks, other)
			}
		}
	}

	return verdict
}

// settleListeners - act on the verdict for a Service about to be synced.
// Reports whether it yields, in which case the sync goes no further.
//
// A yielding Service is not sent to any peer, and what it already has on
// them is deleted: the gateway would otherwise apply one of the two policies
// to every rule on the listener, whichever was installed first, or hold one
// rule for two pools serving the same model, whichever was sent last. Its peers are
// held, so the retry of a later sync puts the rules back once nothing on the
// listener disagrees. Services it outranks that have not yielded yet, and
// Services that yielded to it but no longer disagree, are queued to be
// judged again.
func (m *Manager) settleListeners(svc *corev1.Service, cacheKey string) (bool, error) {
	verdict := m.judgeListeners(svc)

	if m.yielding == nil {
		m.yielding = map[string]string{}
	}

	if verdict.yieldTo == nil {
		if rival, was := m.yielding[cacheKey]; was {
			klog.Infof("service %s: no longer yields its listeners to %s", cacheKey, rival)
			delete(m.yielding, cacheKey)
		}
		outranked := map[string]bool{}
		for _, other := range verdict.outranks {
			key := GenKey(other.Namespace, other.Name)
			outranked[key] = true
			if m.yielding[key] != cacheKey {
				m.queueKey(key)
			}
		}
		for key, rival := range m.yielding {
			if rival == cacheKey && !outranked[key] {
				m.queueKey(key)
			}
		}
		return false, nil
	}

	rivalKey := GenKey(verdict.yieldTo.Namespace, verdict.yieldTo.Name)
	cause := ErrListenerAuthConflict
	if verdict.reason == ReasonListenerRouteConflict {
		cause = ErrListenerRouteConflict
	}
	if m.yielding[cacheKey] != rivalKey {
		m.yielding[cacheKey] = rivalKey
		message := fmt.Sprintf("listener %s is shared with Service %s, which is older and declares another "+
			"api_key_auth or jwt_auth_profile; the gateway checks one policy for every rule on a listener, "+
			"so this Service is not served there until the two agree", verdict.listener, rivalKey)
		if verdict.reason == ReasonListenerRouteConflict {
			model, _ := routeOf(svc)
			message = fmt.Sprintf("listener %s is shared with Service %s, which is older and serves the same model "+
				"(%q); the gateway would hold one rule for both, so this Service is not served there until "+
				"one of them changes or goes", verdict.listener, rivalKey, model)
		}
		klog.Errorf("service %s: %s", cacheKey, message)
		m.recordServiceWarning(svc, verdict.reason, message)
	}

	entry, ok := m.lbCache[cacheKey]
	if !ok {
		return true, nil
	}
	if hasInstalledPeer(entry) {
		if err := m.deleteLoadBalancer(svc.Namespace, svc.Name, false); err != nil {
			return true, err
		}
	}
	held := peerState{phase: peerHeld, err: errors.Wrapf(cause, "yields to %s", rivalKey)}
	for _, sp := range entry.LbServicePairs {
		sp.peers = make(map[string]peerState, len(m.LoxiClients.Clients))
		for _, c := range m.LoxiClients.Clients {
			sp.peers[c.Host] = held
		}
	}

	return true, nil
}

// hasInstalledPeer - whether any peer may hold one of the entry's rules: one
// recorded as installed, or one with no record yet.
func hasInstalledPeer(entry *LbCacheEntry) bool {
	for _, sp := range entry.LbServicePairs {
		if len(sp.LbModelList) == 0 {
			continue
		}
		if len(sp.peers) == 0 {
			return true
		}
		for _, st := range sp.peers {
			if st.phase == peerInstalled {
				return true
			}
		}
	}
	return false
}

// releaseYielders - a Service is gone: the ones that yielded to it are judged
// again.
func (m *Manager) releaseYielders(cacheKey string) {
	delete(m.yielding, cacheKey)
	for key, rival := range m.yielding {
		if rival == cacheKey {
			m.queueKey(key)
		}
	}
}

// queueKey - queue a Service by its cache key.
func (m *Manager) queueKey(cacheKey string) {
	if m.queue == nil {
		return
	}
	namespace, name, err := cache.SplitMetaNamespaceKey(cacheKey)
	if err != nil {
		return
	}
	m.queue.Add(LbCacheKey{Namespace: namespace, Name: name})
}
