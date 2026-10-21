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

	corev1 "k8s.io/api/core/v1"
	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	infv1lister "sigs.k8s.io/gateway-api-inference-extension/client-go/listers/api/v1"
)

// inferencePoolLabel - the label the InferencePool manager puts on each
// Service it generates, naming the pool. The same as poolOwnerLabel in
// gatewayapi, which this package does not import.
const inferencePoolLabel = "loxilb.io/inference-pool"

// poolWarningReasons - the warnings about what a pool asks for. They are
// repeated on the pool, since that is the object the operator wrote and
// looks at; the generated Service is ours.
var poolWarningReasons = map[string]bool{
	ReasonInvalidInferenceConfig: true,
	ReasonAuthNotSupported:       true,
	ReasonLoxiLBRejected:         true,
	ReasonListenerAuthConflict:   true,
	ReasonProfileNotFound:        true,
	ReasonProfileNotProgrammed:   true,
}

// SetInferencePoolLister - the InferencePools, for the Services generated from
// them. Given once the inference extension is on, while the manager may
// already be running.
func (m *Manager) SetInferencePoolLister(lister infv1lister.InferencePoolLister) {
	m.poolLister.Store(lister)
}

// poolOf - the InferencePool a Service was generated from, or nil: the Service
// is not a generated one, the extension is off, or the pool is not in the
// cache (yet).
func (m *Manager) poolOf(svc *corev1.Service) *infv1.InferencePool {
	name := svc.Labels[inferencePoolLabel]
	if name == "" {
		return nil
	}
	lister, _ := m.poolLister.Load().(infv1lister.InferencePoolLister)
	if lister == nil {
		return nil
	}
	pool, err := lister.InferencePools(svc.Namespace).Get(name)
	if err != nil {
		return nil
	}

	return pool
}

// recordPoolWarning - repeat a Service's warning on the InferencePool behind
// it, naming the Service.
//
// Recorded against a reference rather than the pool itself: the recorder's
// scheme does not know the InferencePool type and would drop the event.
func (m *Manager) recordPoolWarning(svc *corev1.Service, reason, message string) {
	if !poolWarningReasons[reason] || m.eventRecorder == nil {
		return
	}
	pool := m.poolOf(svc)
	if pool == nil {
		return
	}

	ref := &corev1.ObjectReference{
		APIVersion:      infv1.SchemeGroupVersion.String(),
		Kind:            "InferencePool",
		Namespace:       pool.Namespace,
		Name:            pool.Name,
		UID:             pool.UID,
		ResourceVersion: pool.ResourceVersion,
	}
	m.eventRecorder.Event(ref, corev1.EventTypeWarning, reason, fmt.Sprintf("Service %s: %s", svc.Name, message))
}
