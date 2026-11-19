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
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRoute status: under each parentRef that names a Gateway of this
// controller's, whether the route is accepted there and whether its
// backendRefs resolve. Entries other controllers write are left as they are.

// routeParentStatuses - the parent statuses this controller reports for the
// route, one for each parent it came to.
func (h *HTTPRouteManager) routeParentStatuses(route *v1.HTTPRoute, parents []parentAttachment) []v1.RouteParentStatus {
	resolved, resolvedReason, resolvedMessage := h.backendRefsResolved(route)
	controller := v1.GatewayController(h.gatewayProvider)

	statuses := make([]v1.RouteParentStatus, 0, len(parents))
	for _, parent := range parents {
		accepted, reason, message := parentAccepted(parent)
		next := []metav1.Condition{
			condition(string(v1.RouteConditionAccepted), accepted, reason, message, route.Generation),
			condition(string(v1.RouteConditionResolvedRefs), resolved, resolvedReason, resolvedMessage, route.Generation),
		}
		statuses = append(statuses, v1.RouteParentStatus{
			ParentRef:      *parent.Ref.DeepCopy(),
			ControllerName: controller,
			Conditions:     carryTransitionTimes(ourRouteConditions(route.Status.Parents, controller, parent.Ref), next),
		})
	}
	return statuses
}

// parentAccepted - the Accepted condition for one parent.
//
// A route that attaches only to listeners that cannot serve it - an HTTPS
// listener without a hostname, say - is not accepted, with UnsupportedValue:
// it attaches by the spec, but nothing serves it. One that some of its
// listeners serve is accepted, and the message names the others.
func parentAccepted(parent parentAttachment) (bool, string, string) {
	a := parent.Attachment
	switch {
	case !a.Accepted():
		return false, string(a.Reason), a.Message
	case len(parent.Unserved) == len(a.Listeners):
		return false, string(v1.RouteReasonUnsupportedValue), strings.Join(parent.Unserved, "; ")
	case len(parent.Unserved) > 0 || len(parent.Notes) > 0:
		return true, string(v1.RouteReasonAccepted), "Accepted; " + strings.Join(append(append([]string(nil), parent.Unserved...), parent.Notes...), "; ")
	}
	return true, string(v1.RouteReasonAccepted), ""
}

// backendRefsResolved - the ResolvedRefs condition of the route, the same
// under every parent: false, with the reason of the first backendRef that
// does not resolve and a message naming each, when one does not.
func (h *HTTPRouteManager) backendRefsResolved(route *v1.HTTPRoute) (bool, string, string) {
	var reason v1.RouteConditionReason
	var problems []string
	note := func(r v1.RouteConditionReason, problem string) {
		if reason == "" {
			reason = r
		}
		if !slices.Contains(problems, problem) {
			problems = append(problems, problem)
		}
	}

	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			ref := backendRef.BackendObjectReference
			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}

			switch classifyBackendRef(ref) {
			case backendService:
				if !h.referenceGrants.Permits(
					grantReference{Group: v1.GroupName, Kind: kindHTTPRoute, Namespace: route.Namespace},
					grantReference{Group: "", Kind: "Service", Namespace: namespace, Name: string(ref.Name)},
				) {
					note(v1.RouteReasonRefNotPermitted,
						fmt.Sprintf("no ReferenceGrant in namespace %s permits Service %s", namespace, ref.Name))
					continue
				}
				if h.serviceLister == nil {
					continue
				}
				if _, err := h.serviceLister.Services(namespace).Get(string(ref.Name)); errors.IsNotFound(err) {
					note(v1.RouteReasonBackendNotFound, fmt.Sprintf("Service %s/%s does not exist", namespace, ref.Name))
				}
			case backendInferencePool:
				if namespace != route.Namespace {
					note(v1.RouteReasonRefNotPermitted,
						fmt.Sprintf("InferencePool %s/%s is in another namespace, which is not served", namespace, ref.Name))
					continue
				}
				if !h.servesInferencePools {
					note(v1.RouteReasonInvalidKind,
						fmt.Sprintf("InferencePool %s is not served: the inference extension is not enabled", ref.Name))
				}
			default:
				note(v1.RouteReasonInvalidKind, fmt.Sprintf("%s is not a kind this controller serves", backendRefDescription(ref)))
			}
		}
	}

	if reason == "" {
		return true, string(v1.RouteReasonResolvedRefs), ""
	}
	return false, string(reason), strings.Join(problems, "; ")
}

// ourRouteConditions - the conditions this controller last wrote under ref,
// among a route's parent statuses.
func ourRouteConditions(parents []v1.RouteParentStatus, controller v1.GatewayController, ref v1.ParentReference) []metav1.Condition {
	for _, parent := range parents {
		if parent.ControllerName == controller && reflect.DeepEqual(parent.ParentRef, ref) {
			return parent.Conditions
		}
	}
	return nil
}

// mergeRouteParents - existing with this controller's entries replaced by
// ours: one for a parentRef it no longer serves goes.
func mergeRouteParents(existing []v1.RouteParentStatus, controller v1.GatewayController, ours []v1.RouteParentStatus) []v1.RouteParentStatus {
	merged := make([]v1.RouteParentStatus, 0, len(existing)+len(ours))
	for _, parent := range existing {
		if parent.ControllerName != controller {
			merged = append(merged, parent)
		}
	}
	return append(merged, ours...)
}

// updateRouteStatus - write ours into the route's status.
//
// A conflict - the cache behind a write a moment ago, typically this
// manager's own - is tried once more against what the API server holds. A
// route that has gone has nothing left to report, and one this process may
// not write the status of is said so once per reconcile rather than retried:
// that takes a ClusterRole with httproutes/status.
func (h *HTTPRouteManager) updateRouteStatus(ctx context.Context, route *v1.HTTPRoute, ours []v1.RouteParentStatus) error {
	err := h.putRouteParents(ctx, route, ours)
	if errors.IsConflict(err) {
		fresh, getErr := h.sigsClient.GatewayV1().HTTPRoutes(route.Namespace).Get(ctx, route.Name, metav1.GetOptions{})
		if getErr == nil {
			err = h.putRouteParents(ctx, fresh, recarryRouteParents(fresh.Status.Parents, ours))
		}
	}

	switch {
	case err == nil, errors.IsNotFound(err):
		return nil
	case errors.IsForbidden(err):
		klog.Warningf("HTTPRoute %s/%s: not allowed to write its status - the ClusterRole needs httproutes/status: %v",
			route.Namespace, route.Name, err)
		return nil
	}
	return fmt.Errorf("HTTPRoute %s/%s: update status: %w", route.Namespace, route.Name, err)
}

// putRouteParents - one status write of ours merged into route, skipped when
// it would change nothing.
func (h *HTTPRouteManager) putRouteParents(ctx context.Context, route *v1.HTTPRoute, ours []v1.RouteParentStatus) error {
	merged := mergeRouteParents(route.Status.Parents, v1.GatewayController(h.gatewayProvider), ours)
	// No parents at all reads back as nil while merging builds an empty
	// slice; both are the same status.
	if reflect.DeepEqual(route.Status.Parents, merged) || (len(route.Status.Parents) == 0 && len(merged) == 0) {
		return nil
	}

	updated := route.DeepCopy()
	updated.Status.Parents = merged
	_, err := h.sigsClient.GatewayV1().HTTPRoutes(updated.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

// recarryRouteParents - ours with the transition times worked out again
// against the parent statuses the API server holds.
func recarryRouteParents(fresh []v1.RouteParentStatus, ours []v1.RouteParentStatus) []v1.RouteParentStatus {
	out := make([]v1.RouteParentStatus, len(ours))
	for i, parent := range ours {
		parent.Conditions = carryTransitionTimes(ourRouteConditions(fresh, parent.ControllerName, parent.ParentRef),
			append([]metav1.Condition(nil), parent.Conditions...))
		out[i] = parent
	}
	return out
}

// enqueueServiceRoutes - queue the HTTPRoutes that name a Service as a
// backend, whose ResolvedRefs it decides by being there or not.
func (h *HTTPRouteManager) enqueueServiceRoutes(obj interface{}) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		svc, ok = deletedState.Obj.(*corev1.Service)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
			return
		}
	}

	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("service %s/%s: unable to list HTTPRoutes: %v", svc.Namespace, svc.Name, err)
		return
	}
	for _, route := range routes {
		if routeNamesService(route, svc.Namespace, svc.Name) {
			h.queue.Add(HTTPRouteQueueEntry{route.Namespace, route.Name})
		}
	}
}

// routeNamesService - whether a backendRef of the route names the Service.
func routeNamesService(route *v1.HTTPRoute, namespace, name string) bool {
	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			ref := backendRef.BackendObjectReference
			if classifyBackendRef(ref) != backendService || string(ref.Name) != name {
				continue
			}
			refNamespace := route.Namespace
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}
			if refNamespace == namespace {
				return true
			}
		}
	}
	return false
}
