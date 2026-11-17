/*
* Copyright (c) 2026 NetLOX Inc
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

package gatewayapi

import (
	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// inferencePoolGroup / inferencePoolKind - the Gateway API Inference
	// Extension pool, referenced from a route's backendRefs the same way a
	// Service is.
	inferencePoolGroup = "inference.networking.k8s.io"
	inferencePoolKind  = "InferencePool"
)

// backendKind - what a backendRef actually points at.
//
// Gateway API lets a backendRef name any object, not just a Service, and the
// group/kind is how the route says which. Reading the name without reading
// those two fields turns "InferencePool vllm-pool" into "Service vllm-pool" -
// an object that does not exist - and the resulting Ingress or LB rule points
// at nothing while every reconcile reports success.
type backendKind int

const (
	// backendService - core Service, the default when group and kind are unset.
	backendService backendKind = iota
	// backendInferencePool - inference.networking.k8s.io/InferencePool.
	backendInferencePool
	// backendUnsupported - anything else. Must not be treated as a Service.
	backendUnsupported
)

func (b backendKind) String() string {
	switch b {
	case backendService:
		return "Service"
	case backendInferencePool:
		return inferencePoolKind
	default:
		return "unsupported"
	}
}

// classifyBackendRef - resolve a backendRef's group/kind to what it points at.
//
// Both fields are optional and both have defaults the API server does not
// materialise, so an unset pair means core/Service and has to be read that way
// here. "core" is accepted alongside "" because it is the spelling users reach
// for even though the API defines the core group as the empty string.
func classifyBackendRef(ref v1.BackendObjectReference) backendKind {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}

	kind := "Service"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	switch {
	case (group == "" || group == "core") && kind == "Service":
		return backendService
	case group == inferencePoolGroup && kind == inferencePoolKind:
		return backendInferencePool
	default:
		return backendUnsupported
	}
}

// backendRefDescription - group/kind as written, for log and event text.
func backendRefDescription(ref v1.BackendObjectReference) string {
	group := "core"
	if ref.Group != nil && *ref.Group != "" {
		group = string(*ref.Group)
	}

	kind := "Service"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	return group + "/" + kind + " " + string(ref.Name)
}
