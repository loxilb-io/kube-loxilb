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

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
)

// A Service generated from an InferencePool is served on its Gateway
// listener's address and port, which it shares with the other pools there
// (listenerShare), as old as its pool (ageOf) and told apart by model
// (routeOf). The label that marks one is something any Service can carry,
// so none of that follows from the label: the InferencePool manager is asked
// whether it made the Service, as it made it.

const (
	// ReasonPoolServiceRefused - the event reason for a Service that
	// carries the InferencePool label but was not made by the InferencePool
	// manager as it is.
	ReasonPoolServiceRefused = "PoolServiceRefused"
)

var (
	// ErrPoolServiceUntrusted - a Service carries the InferencePool label but
	// is not one the InferencePool manager made, or not as it made it.
	ErrPoolServiceUntrusted = errors.New("not a Service the InferencePool manager made")

	// errPoolsNotDecided - the InferencePool manager cannot tell yet: it has
	// not started, or not reconciled the pool since. Retried without an event.
	errPoolsNotDecided = errors.New("the InferencePool manager has not decided on the Service yet")
)

// PoolServiceCheck - whether a Service with the InferencePool label is one the
// InferencePool manager made, as it made it; decided is false while it cannot
// tell yet.
type PoolServiceCheck func(svc *corev1.Service) (generated, decided bool)

// NoPoolServices - the check when no InferencePool manager runs: no Service is
// a pool's.
func NoPoolServices(*corev1.Service) (generated, decided bool) {
	return false, true
}

// SetPoolServiceCheck - how to tell a pool's Service. Until it is given, none
// is decided on: the InferencePool manager starts after this one.
func (m *Manager) SetPoolServiceCheck(check PoolServiceCheck) {
	m.poolCheck.Store(check)
}

// checkPoolService - nil for a Service without the InferencePool label, or
// one the InferencePool manager made; why not otherwise.
func (m *Manager) checkPoolService(svc *corev1.Service) error {
	if svc.Labels[inferencePoolLabel] == "" {
		return nil
	}
	check, _ := m.poolCheck.Load().(PoolServiceCheck)
	if check == nil {
		return fmt.Errorf("service %s/%s: %w", svc.Namespace, svc.Name, errPoolsNotDecided)
	}
	generated, decided := check(svc)
	switch {
	case !decided:
		return fmt.Errorf("service %s/%s: %w", svc.Namespace, svc.Name, errPoolsNotDecided)
	case !generated:
		return fmt.Errorf("service %s/%s carries %s=%s: %w", svc.Namespace, svc.Name,
			inferencePoolLabel, svc.Labels[inferencePoolLabel], ErrPoolServiceUntrusted)
	}
	return nil
}

// trustedPool - whether a Service is one the InferencePool manager made.
func (m *Manager) trustedPool(svc *corev1.Service) bool {
	return svc.Labels[inferencePoolLabel] != "" && m.checkPoolService(svc) == nil
}

// claimsPool - whether a Service counts as a pool's when Services on one
// listener are weighed: it carries the label and has not been refused. One
// not decided on yet still counts, as before the InferencePool manager could
// be asked, so the verdicts do not turn over while it starts.
func (m *Manager) claimsPool(svc *corev1.Service) bool {
	if svc.Labels[inferencePoolLabel] == "" {
		return false
	}
	err := m.checkPoolService(svc)
	return err == nil || errors.Is(err, errPoolsNotDecided)
}

// refusedHere - whether a Service is refused for what it claims to be: a
// pool's or a gateway route's that is not. It takes no listener, so it has no
// say on one either. One not decided on yet is not refused.
func (m *Manager) refusedHere(svc *corev1.Service) bool {
	if svc.Labels[inferencePoolLabel] != "" && !m.claimsPool(svc) {
		return true
	}
	if svc.Labels[gatewayRouteLabel] == "" {
		return false
	}
	_, err := m.controllingGateway(svc)
	return err != nil && !errors.Is(err, errGatewaysNotSynced)
}
