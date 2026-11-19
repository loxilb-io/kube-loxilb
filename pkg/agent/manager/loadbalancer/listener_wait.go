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
	"net/http"
	"strings"

	"github.com/pkg/errors"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// A credential policy declared by several Services on one listener changes one
// Service at a time: the InferencePool manager updates the Services of its
// pools in turn, and each is synced on its own, its rule deleted and sent
// again with the new policy. The gateway holds one policy per listener and
// refuses a rule declaring another while a rule there still declares the old
// one (HTTP 400). So the first Services to change are refused until the
// others have changed as well, or yielded and taken their rules off.
//
// Such a refusal is waited out rather than backed off: the refused Service is
// queued again as soon as each Service whose rule held the old policy has had
// its sync, the sync that takes that rule off or sends it with the new
// policy. Nor is it reported while nothing went to any peer, being the change
// on its way, unless one of those Services has had its sync since and still
// holds the listener. A rule some peer took is reported as before: its
// retries go through retryPeers, which reports a refusal once, so one held
// back here would not be reported at all.

// errListenerWait - every peer refused the Service's rule while other
// Services' rules on its listener declared another credential policy, and
// those have not had their sync since.
var errListenerWait = errors.New("waiting for the other rules on the listener to take the same credential policy")

// listenerWait - the Services a refused Service waits for, by cache key, each
// with whether it has had a sync since the wait began.
type listenerWait map[string]bool

// declaresOtherPolicy - whether one of models is a fullproxy rule declaring
// another credential policy than lb.
func declaresOtherPolicy(models []api.LoadBalancerModel, lb api.LoadBalancerModel) bool {
	for _, other := range models {
		if other.Service.Mode == api.LBModeFullProxy &&
			(other.Service.APIKeyAuth != lb.Service.APIKeyAuth || other.Service.JwtAuthProfile != lb.Service.JwtAuthProfile) {
			return true
		}
	}
	return false
}

// policyHolders - the other Services with a rule on lb's listener, installed
// on the peer host or not known not to be, declaring another credential
// policy than lb.
func (m *Manager) policyHolders(self string, lb api.LoadBalancerModel, host string) []string {
	var keys []string
	for key, entry := range m.lbCache {
		if key == self {
			continue
		}
		for _, sp := range entry.LbServicePairs {
			if sp.ExternalIP != lb.Service.ExternalIP || sp.Port != lb.Service.Port ||
				!strings.EqualFold(sp.Protocol, lb.Service.Protocol) {
				continue
			}
			if st, known := sp.peers[host]; known && st.phase != peerInstalled {
				continue
			}
			if declaresOtherPolicy(sp.LbModelList, lb) {
				keys = append(keys, key)
				break
			}
		}
	}
	return keys
}

// listenerHolders - the Services the peers' refusals of lb wait for: on each
// peer that refused it, the other Services whose rules on its listener
// declare another policy. ok is false unless every refusal is a 400 with some
// such Service: any other is not about the listener's policy.
func (m *Manager) listenerHolders(self string, lb api.LoadBalancerModel, clients []*api.LoxiClient, errs []error) (holders []string, ok bool) {
	seen := map[string]bool{}
	for i, err := range errs {
		if err == nil {
			continue
		}
		if api.StatusCodeOf(err) != http.StatusBadRequest {
			return nil, false
		}
		keys := m.policyHolders(self, lb, clients[i].Host)
		if len(keys) == 0 {
			return nil, false
		}
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				holders = append(holders, key)
			}
		}
	}
	return holders, len(holders) > 0
}

// keepListenerWait - record what a Service's refused rules wait for; nothing
// once none does.
func (m *Manager) keepListenerWait(cacheKey string, wait listenerWait) {
	if len(wait) == 0 {
		delete(m.listenerWaits, cacheKey)
		return
	}
	if m.listenerWaits == nil {
		m.listenerWaits = map[string]listenerWait{}
	}
	m.listenerWaits[cacheKey] = wait
}

// wakeListenerWaiters - a Service has had its sync: the Services waiting for
// it are queued now rather than after their backoff.
func (m *Manager) wakeListenerWaiters(cacheKey string) {
	for key, wait := range m.listenerWaits {
		if _, ok := wait[cacheKey]; ok {
			wait[cacheKey] = true
			m.queueKey(key)
		}
	}
}
