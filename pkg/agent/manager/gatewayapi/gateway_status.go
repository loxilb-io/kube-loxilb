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
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Gateway status: the addresses, Accepted and Programmed for the Gateway, and
// for each listener the route kinds it takes, how many routes attach to it,
// and Accepted, Conflicted, ResolvedRefs and Programmed.

// listenerConflict - why a listener cannot have its port: it shares the port
// and transport with a listener before it, with another protocol
// (ProtocolConflict) or with the same protocol and hostname
// (HostnameConflict). The earlier listener keeps the port.
type listenerConflict struct {
	reason  v1.ListenerConditionReason
	message string
}

// transportOf - the transport a listener's protocol runs on. Listeners on one
// port and different transports do not meet.
func transportOf(protocol v1.ProtocolType) string {
	if protocol == v1.UDPProtocolType {
		return "udp"
	}
	return "tcp"
}

func sameHostname(a, b *v1.Hostname) bool {
	if a == nil || b == nil {
		return (a == nil || *a == "") && (b == nil || *b == "")
	}
	return *a == *b
}

// listenerConflicts - the listeners of gw that conflict with an earlier one.
func listenerConflicts(gw *v1.Gateway) map[v1.SectionName]listenerConflict {
	conflicts := map[v1.SectionName]listenerConflict{}
	for i := range gw.Spec.Listeners {
		l := &gw.Spec.Listeners[i]
		for j := 0; j < i; j++ {
			earlier := &gw.Spec.Listeners[j]
			if _, lost := conflicts[earlier.Name]; lost {
				continue
			}
			if earlier.Port != l.Port || transportOf(earlier.Protocol) != transportOf(l.Protocol) {
				continue
			}
			if earlier.Protocol != l.Protocol {
				conflicts[l.Name] = listenerConflict{v1.ListenerReasonProtocolConflict,
					fmt.Sprintf("listener %s has port %d as %s", earlier.Name, l.Port, earlier.Protocol)}
				break
			}
			if sameHostname(earlier.Hostname, l.Hostname) {
				conflicts[l.Name] = listenerConflict{v1.ListenerReasonHostnameConflict,
					fmt.Sprintf("listener %s has port %d with the same hostname", earlier.Name, l.Port)}
				break
			}
		}
	}
	return conflicts
}

// listenerRefsResolved - the ResolvedRefs of a listener: the route kinds it
// names that it cannot take, and the certificates of an HTTPS listener.
//
// The certificate is read by loxilb-ingress from the Ingress's namespace,
// which is the route's, so a certificateRef into another namespace cannot
// be served. That a Secret exists is not checked here.
func listenerRefsResolved(gw *v1.Gateway, l *v1.Listener) (bool, v1.ListenerConditionReason, string) {
	if _, invalid := listenerRouteKinds(l); len(invalid) > 0 {
		kinds := make([]string, 0, len(invalid))
		for _, k := range invalid {
			kinds = append(kinds, string(k.Kind))
		}
		return false, v1.ListenerReasonInvalidRouteKinds,
			fmt.Sprintf("route kinds %s are not served on %s", strings.Join(kinds, ", "), l.Protocol)
	}

	if l.Protocol == v1.HTTPSProtocolType {
		if len(listenerCertificateSecrets(l)) == 0 {
			return false, v1.ListenerReasonInvalidCertificateRef, "no certificateRef names a Secret"
		}
		for _, ref := range l.TLS.CertificateRefs {
			if ref.Namespace != nil && string(*ref.Namespace) != gw.Namespace {
				return false, v1.ListenerReasonInvalidCertificateRef,
					fmt.Sprintf("certificateRef %s/%s is in another namespace, which is not served", *ref.Namespace, ref.Name)
			}
		}
	}
	return true, v1.ListenerReasonResolvedRefs, ""
}

// attachedRoutes - how many routes attach to each listener of gw: the
// HTTPRoutes, and those of the route managers that count their own.
func (gm *GatewayManager) attachedRoutes(gw *v1.Gateway) map[v1.SectionName]int32 {
	counts := map[v1.SectionName]int32{}
	gm.routeSourcesMu.Lock()
	sources := append([]RouteSource(nil), gm.routeSources...)
	gm.routeSourcesMu.Unlock()
	for _, src := range sources {
		for name, n := range src.AttachedRoutes(gw) {
			counts[name] += n
		}
	}

	if gm.httpRouteLister == nil {
		return counts
	}
	routes, err := gm.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("gateway %s/%s: unable to list HTTPRoutes: %v", gw.Namespace, gw.Name, err)
		return counts
	}

	nsLabels := gm.namespaceLabels()
	for _, route := range routes {
		attaching := attachingRoute{Kind: kindHTTPRoute, Namespace: route.Namespace, Hostnames: route.Spec.Hostnames}
		seen := map[v1.SectionName]bool{}
		for _, ref := range route.Spec.ParentRefs {
			namespace, name, ok := gatewayOf(route.Namespace, ref)
			if !ok || namespace != gw.Namespace || name != gw.Name {
				continue
			}
			for _, la := range attachRoute(gw, ref, attaching, nsLabels).Listeners {
				if !seen[la.Listener.Name] {
					seen[la.Listener.Name] = true
					counts[la.Listener.Name]++
				}
			}
		}
	}
	return counts
}

// namespaceLabels - the labels of a namespace, read from the cache.
func (gm *GatewayManager) namespaceLabels() namespaceLabels {
	if gm.namespaceLister == nil {
		return nil
	}
	return func(name string) (labels.Set, error) {
		ns, err := gm.namespaceLister.Get(name)
		if err != nil {
			return nil, err
		}
		return labels.Set(ns.Labels), nil
	}
}

func setCondition(conditions *[]metav1.Condition, conditionType string, ok bool, reason, message string, generation int64) {
	c := condition(conditionType, ok, reason, message, generation)
	meta.SetStatusCondition(conditions, c)
}

// gatewayStatus - the status gw should have, with serviceErr what making its
// ingress service came to. A condition keeps its transition time while its
// status stays.
func (gm *GatewayManager) gatewayStatus(gw *v1.Gateway, serviceErr error) v1.GatewayStatus {
	status := *gw.Status.DeepCopy()

	status.Addresses = nil
	for _, address := range gw.Spec.Addresses {
		status.Addresses = append(status.Addresses, v1.GatewayStatusAddress(address))
	}
	addressed := len(status.Addresses) > 0

	// From the copy: the conditions are set in place.
	previous := map[v1.SectionName]v1.ListenerStatus{}
	for _, ls := range status.Listeners {
		previous[ls.Name] = ls
	}
	conflicts := listenerConflicts(gw)
	attached := gm.attachedRoutes(gw)
	group := v1.Group(v1.GroupName)

	status.Listeners = make([]v1.ListenerStatus, 0, len(gw.Spec.Listeners))
	var invalid []string
	for i := range gw.Spec.Listeners {
		l := &gw.Spec.Listeners[i]
		ls := previous[l.Name]
		ls.Name = l.Name
		ls.AttachedRoutes = attached[l.Name]

		admitted, _ := listenerRouteKinds(l)
		ls.SupportedKinds = make([]v1.RouteGroupKind, 0, len(admitted))
		for _, kind := range admitted {
			ls.SupportedKinds = append(ls.SupportedKinds, v1.RouteGroupKind{Group: &group, Kind: kind})
		}

		accepted := protocolRouteKinds(l.Protocol) != nil
		if accepted {
			setCondition(&ls.Conditions, string(v1.ListenerConditionAccepted), true, string(v1.ListenerReasonAccepted), "", gw.Generation)
		} else {
			setCondition(&ls.Conditions, string(v1.ListenerConditionAccepted), false, string(v1.ListenerReasonUnsupportedProtocol),
				fmt.Sprintf("protocol %s is not served", l.Protocol), gw.Generation)
		}

		conflict, conflicted := conflicts[l.Name]
		if conflicted {
			setCondition(&ls.Conditions, string(v1.ListenerConditionConflicted), true, string(conflict.reason), conflict.message, gw.Generation)
		} else {
			setCondition(&ls.Conditions, string(v1.ListenerConditionConflicted), false, string(v1.ListenerReasonNoConflicts), "", gw.Generation)
		}

		resolved, reason, message := listenerRefsResolved(gw, l)
		setCondition(&ls.Conditions, string(v1.ListenerConditionResolvedRefs), resolved, string(reason), message, gw.Generation)

		switch {
		case !accepted || conflicted || !resolved:
			setCondition(&ls.Conditions, string(v1.ListenerConditionProgrammed), false, string(v1.ListenerReasonInvalid),
				"the listener is not accepted, conflicts, or does not resolve its references", gw.Generation)
			invalid = append(invalid, string(l.Name))
		case !addressed:
			setCondition(&ls.Conditions, string(v1.ListenerConditionProgrammed), false, string(v1.ListenerReasonPending),
				"the gateway has no address yet", gw.Generation)
		default:
			setCondition(&ls.Conditions, string(v1.ListenerConditionProgrammed), true, string(v1.ListenerReasonProgrammed), "", gw.Generation)
		}
		status.Listeners = append(status.Listeners, ls)
	}

	switch {
	case len(invalid) == 0:
		setCondition(&status.Conditions, string(v1.GatewayConditionAccepted), true, string(v1.GatewayReasonAccepted), "", gw.Generation)
	case len(invalid) < len(gw.Spec.Listeners):
		setCondition(&status.Conditions, string(v1.GatewayConditionAccepted), true, string(v1.GatewayReasonListenersNotValid),
			"listeners not valid: "+strings.Join(invalid, ", "), gw.Generation)
	default:
		setCondition(&status.Conditions, string(v1.GatewayConditionAccepted), false, string(v1.GatewayReasonListenersNotValid),
			"no listener is valid", gw.Generation)
	}

	switch {
	case !addressed:
		setCondition(&status.Conditions, string(v1.GatewayConditionProgrammed), false, string(v1.GatewayReasonAddressNotAssigned),
			"no address is assigned", gw.Generation)
	case serviceErr != nil:
		setCondition(&status.Conditions, string(v1.GatewayConditionProgrammed), false, string(v1.GatewayReasonInvalid),
			fmt.Sprintf("the ingress service could not be made: %v", serviceErr), gw.Generation)
	default:
		setCondition(&status.Conditions, string(v1.GatewayConditionProgrammed), true, string(v1.GatewayReasonProgrammed), "", gw.Generation)
	}
	return status
}

// updateGatewayStatus - write the status gw should have, when it differs
// from what gw has. A conflict is tried once more against what the API
// server holds.
func (gm *GatewayManager) updateGatewayStatus(ctx context.Context, gw *v1.Gateway, serviceErr error) error {
	err := gm.putGatewayStatus(ctx, gw, serviceErr)
	if errors.IsConflict(err) {
		fresh, getErr := gm.sigsClient.GatewayV1().Gateways(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
		if getErr == nil {
			err = gm.putGatewayStatus(ctx, fresh, serviceErr)
		}
	}
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("gateway %s/%s: update status: %w", gw.Namespace, gw.Name, err)
	}
	return nil
}

func (gm *GatewayManager) putGatewayStatus(ctx context.Context, gw *v1.Gateway, serviceErr error) error {
	next := gm.gatewayStatus(gw, serviceErr)
	if reflect.DeepEqual(gw.Status, next) {
		return nil
	}
	updated := gw.DeepCopy()
	updated.Status = next
	_, err := gm.sigsClient.GatewayV1().Gateways(updated.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

// CountRoutesOf - count the routes of src on the listeners of each Gateway,
// and have a change to one of them queue the Gateways it names. The route
// managers of TCPRoutes and UDPRoutes start after this one, once their CRDs
// are known to serve the version they watch.
func (gm *GatewayManager) CountRoutesOf(src RouteSource) {
	gm.routeSourcesMu.Lock()
	gm.routeSources = append(gm.routeSources, src)
	gm.routeSourcesMu.Unlock()
	src.NotifyGateways(gm.enqueueGatewaysOf)
}

// enqueueGatewaysOf - queue the Gateways a route's parentRefs name.
func (gm *GatewayManager) enqueueGatewaysOf(routeNamespace string, refs []v1.ParentReference) {
	for _, ref := range refs {
		namespace, name, ok := gatewayOf(routeNamespace, ref)
		if !ok {
			continue
		}
		if gw, err := gm.gatewayLister.Gateways(namespace).Get(name); err == nil {
			gm.enqueue(gw)
		}
	}
}
