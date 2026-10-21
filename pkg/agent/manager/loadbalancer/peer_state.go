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
	"sync"

	"github.com/pkg/errors"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// peerPhase - where one peer stands with the rules of one Service port.
//
// A rule is fanned out to every peer, and peers answer differently: one has a
// JWT auth profile the other does not have yet, one runs a build without a
// feature the rule needs, one is restarting. Recording the answer per peer is
// what lets the ones that did not take the rule be tried again without
// touching the ones that did.
type peerPhase int

const (
	// peerInstalled - the peer has the rule: it answered 200, or 409 for one
	// it already had.
	peerInstalled peerPhase = iota + 1
	// peerPending - the peer should have the rule and does not: it refused it,
	// failed, or did not answer. Tried again while the peer is up.
	peerPending
	// peerHeld - kube-loxilb did not send the rule to this peer on purpose
	// (isPeerRefusal): it is plain loxilb, or lacks a feature the rule needs.
	// Neither a failure to retry with backoff nor a rule gone missing. Looked
	// at again on each sync, since the reason can go away - an upgraded peer.
	peerHeld
)

// peerState - one peer's standing with one Service port's rules.
type peerState struct {
	phase peerPhase
	// err - why the peer is pending or held; nil once installed.
	err error
}

// errPeersPending - the Service is programmed, but a peer that is up does not
// have all of its rules yet. Returned so the worker comes back with backoff;
// nothing about the Service needs fixing.
var errPeersPending = errors.New("rules pending on some loxilb peers")

// peerStateOf - what installLB's answer means for that peer.
func peerStateOf(err error) peerState {
	switch {
	case err == nil:
		return peerState{phase: peerInstalled}
	case isPeerRefusal(err):
		return peerState{phase: peerHeld, err: err}
	default:
		return peerState{phase: peerPending, err: err}
	}
}

// installOnPeers - send one rule to every peer at once. The answers come back
// in the order of clients.
func (m *Manager) installOnPeers(clients []*api.LoxiClient, lb api.LoadBalancerModel, prefLocal bool) []error {
	errs := make([]error, len(clients))

	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *api.LoxiClient) {
			defer wg.Done()
			errs[i] = m.installLB(c, lb, prefLocal)
		}(i, c)
	}
	wg.Wait()

	return errs
}

// peerStatesOf - the per-peer outcome of one fan-out.
func peerStatesOf(clients []*api.LoxiClient, errs []error) map[string]peerState {
	states := make(map[string]peerState, len(clients))
	for i, c := range clients {
		states[c.Host] = peerStateOf(errs[i])
	}

	return states
}

// peersPending - whether a peer that is up still lacks a rule it should have.
//
// A peer that is down is left out: the re-sync when it comes back covers it,
// and counting it would keep every Service retrying for as long as it is down.
func peersPending(clients []*api.LoxiClient, states map[string]peerState) bool {
	for _, c := range clients {
		if st, ok := states[c.Host]; ok && st.phase == peerPending && c.IsAlive {
			return true
		}
	}

	return false
}

// retryPeers - send a Service's rules again to each peer that is up and does
// not have them: pending ones, held ones whose reason may have gone, and ones
// not tried since they came back up. Peers that have them are not contacted,
// so a Service in its steady state costs no requests here.
//
// What changes is reported the way the first install reports it, once per
// change rather than on every pass. Reports whether a peer that is up still
// lacks a rule it should have.
func (m *Manager) retryPeers(cacheKey string) bool {
	entry, ok := m.lbCache[cacheKey]
	if !ok {
		return false
	}

	clients := m.LoxiClients.Clients
	pending := false
	for _, sp := range entry.LbServicePairs {
		if sp.peers == nil {
			sp.peers = make(map[string]peerState)
		}

		for _, c := range clients {
			prev, known := sp.peers[c.Host]
			if (known && prev.phase == peerInstalled) || !c.IsAlive {
				continue
			}

			var err error
			for _, lb := range sp.LbModelList {
				if err = m.installLB(c, lb, entry.PrefLocal); err != nil {
					break
				}
			}

			next := peerStateOf(err)
			sp.peers[c.Host] = next
			m.reportPeerChange(cacheKey, c, sp, prev, known, next)
			if next.phase == peerPending {
				pending = true
			}
		}
	}

	return pending
}

// reportPeerChange - say on the Service what a retry changed for one peer.
//
// Only a change is reported: a held peer is looked at on every sync and a
// pending one on every retry, and the same warning each time would bury the
// rest of the Service's events.
func (m *Manager) reportPeerChange(cacheKey string, c *api.LoxiClient, sp *LbServicePairEntry,
	prev peerState, known bool, next peerState) {
	if known && prev.phase == next.phase && sameError(prev.err, next.err) {
		return
	}

	switch next.phase {
	case peerInstalled:
		klog.Infof("loxilb-lb(%s): rules of %s installed", c.Host, cacheKey)
		for _, lb := range sp.LbModelList {
			m.reportResyncDowngrade(cacheKey, c, lb)
		}
	case peerHeld:
		klog.Infof("loxilb-lb(%s): rules of %s not sent: %v", c.Host, cacheKey, next.err)
		m.reportResyncRefusal(cacheKey, next.err)
	case peerPending:
		klog.Infof("loxilb-lb(%s): rules of %s not installed yet: %v", c.Host, cacheKey, next.err)
		if api.IsClientError(next.err) {
			m.reportResyncRejection(cacheKey, c, next.err)
		}
	}
}

func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Error() == b.Error()
}
