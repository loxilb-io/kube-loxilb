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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// The status of TCPRoutes and UDPRoutes, and their count on the listeners of
// a Gateway.

// routeParentStatuses - the parent statuses this controller reports for a
// route the plan is of.
func (m *l4RouteManager) routeParentStatuses(route *l4Route, plan l4Plan) []v1.RouteParentStatus {
	controller := v1.GatewayController(m.gatewayProvider)

	resolvedReason := string(plan.resolved.reason)
	if plan.resolved.ok {
		resolvedReason = string(v1.RouteReasonResolvedRefs)
	}

	statuses := make([]v1.RouteParentStatus, 0, len(plan.parents))
	for _, parent := range plan.parents {
		next := []metav1.Condition{
			condition(string(v1.RouteConditionAccepted), parent.accepted, string(parent.reason), parent.message, route.Generation),
			condition(string(v1.RouteConditionResolvedRefs), plan.resolved.ok, resolvedReason, plan.resolved.message, route.Generation),
		}
		statuses = append(statuses, v1.RouteParentStatus{
			ParentRef:      *parent.ref.DeepCopy(),
			ControllerName: controller,
			Conditions:     carryTransitionTimes(ourRouteConditions(route.Parents, controller, parent.ref), next),
		})
	}
	return statuses
}

// updateStatus - write this controller's parent statuses into the route's,
// keeping other controllers' entries, when that changes anything.
//
// A conflict - the cache behind a write a moment ago, typically this
// manager's own - is tried once more against what the API server holds. A
// route that has gone has nothing to report, and one whose status this
// process may not write is said so rather than retried: that takes a
// ClusterRole with tcproutes/status and udproutes/status.
func (m *l4RouteManager) updateStatus(ctx context.Context, route *l4Route, plan l4Plan) error {
	if m.putStatus == nil {
		return nil
	}
	controller := v1.GatewayController(m.gatewayProvider)
	ours := m.routeParentStatuses(route, plan)

	put := func(current *l4Route, ours []v1.RouteParentStatus) error {
		merged := mergeRouteParents(current.Parents, controller, ours)
		if reflect.DeepEqual(current.Parents, merged) || (len(current.Parents) == 0 && len(merged) == 0) {
			return nil
		}
		return m.putStatus(ctx, m.sigsClient, current, merged)
	}

	err := put(route, ours)
	if errors.IsConflict(err) && m.fetch != nil {
		fresh, getErr := m.fetch(ctx, m.sigsClient, route.Namespace, route.Name)
		if getErr == nil {
			err = put(fresh, recarryRouteParents(fresh.Parents, ours))
		}
	}

	switch {
	case err == nil, errors.IsNotFound(err):
		return nil
	case errors.IsForbidden(err):
		klog.Warningf("%s %s/%s: not allowed to write its status - the ClusterRole needs %s/status: %v",
			m.kind, route.Namespace, route.Name, m.resource(), err)
		return nil
	}
	return fmt.Errorf("%s %s/%s: update status: %w", m.kind, route.Namespace, route.Name, err)
}

// resource - the API resource of the routes, as a ClusterRole names it.
func (m *l4RouteManager) resource() string {
	if m.kind == kindUDPRoute {
		return "udproutes"
	}
	return "tcproutes"
}

// enqueueServiceRoutes - queue the routes that name a Service as a backend.
func (m *l4RouteManager) enqueueServiceRoutes(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		klog.Errorf("%s: received unexpected object: %v", m.name, obj)
		return
	}

	routes, err := m.list(metav1.NamespaceAll)
	if err != nil {
		klog.Errorf("%s: unable to list routes: %v", m.name, err)
		return
	}
	for _, route := range routes {
		for _, ref := range route.BackendRefs {
			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			if classifyBackendRef(ref.BackendObjectReference) == backendService &&
				namespace == svc.Namespace && string(ref.Name) == svc.Name {
				m.queue.Add(l4RouteKey{route.Namespace, route.Name})
				break
			}
		}
	}
}

// RouteSource - a route manager whose routes the Gateway manager counts on
// each listener, in its attachedRoutes.
type RouteSource interface {
	// AttachedRoutes - how many of its routes each listener of gw serves.
	AttachedRoutes(gw *v1.Gateway) map[v1.SectionName]int32
	// NotifyGateways - have fn told the parents of each route that comes,
	// changes or goes. Called before the informers start.
	NotifyGateways(fn func(routeNamespace string, refs []v1.ParentReference))
}

// AttachedRoutes - how many routes each listener of gw serves: those a route
// attaches to on the Gateway it is served on.
func (m *l4RouteManager) AttachedRoutes(gw *v1.Gateway) map[v1.SectionName]int32 {
	counts := map[v1.SectionName]int32{}
	routes, err := m.list(metav1.NamespaceAll)
	if err != nil {
		klog.Errorf("%s: unable to list routes: %v", m.name, err)
		return counts
	}
	for _, route := range routes {
		gateway, listeners, _ := m.attachments(route)
		if gateway == nil || gateway.Namespace != gw.Namespace || gateway.Name != gw.Name {
			continue
		}
		seen := map[v1.SectionName]bool{}
		for _, l := range listeners {
			if !seen[l.Name] {
				seen[l.Name] = true
				counts[l.Name]++
			}
		}
	}
	return counts
}

func (m *l4RouteManager) NotifyGateways(fn func(routeNamespace string, refs []v1.ParentReference)) {
	m.onRouteChange = fn
}
