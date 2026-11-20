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

package gatewayapi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A port goes back from the direct routes to the ingress service when it can
// be served directly no more: a listener on it is HTTPS now, a peer is not
// the gateway. Its direct rules serve it until the Ingress path can:
//
//   - The port is on the ingress service again, and its listeners get their
//     Ingresses, but its L4 rule is held back (ReleasePorts) until every
//     loxilb-ingress pod that reports has installed them. The gateway serves
//     the L4 rule ahead of the direct rules, and a pod sent a request for an
//     Ingress it has not installed refuses it.
//   - Then the L4 rule is sent, and the direct rules go once every peer that
//     is up has it (InstalledPorts).
//
// A loxilb-ingress pod that reports carries the label
// loxilb.io/programmed-report=v1, and writes the generation of each Ingress
// it has installed on the Ingress, under loxilb.io/programmed.<pod name>. A
// pod without the label is not waited for, and none is waited for longer
// than ingressProgrammedTimeout: the port goes on, with a warning.

const (
	programmedReportLabel = "loxilb.io/programmed-report"
	programmedReportV1    = "v1"

	ingressProgrammedTimeout = 30 * time.Second

	// reasonIngressNotProgrammed - the Gateway event of a port whose L4 rule
	// was sent before loxilb-ingress said it had the port's Ingresses.
	reasonIngressNotProgrammed = "IngressNotProgrammed"
)

// IngressesOn - the Ingresses the routes on gw's listeners on a port have
// there, once the ingress service carries the port.
type IngressesOn func(gw *v1.Gateway, port v1.PortNumber) ([]*netv1.Ingress, error)

// InstalledPorts - which of the TCP ports of a Service every peer that is up
// has the rules of.
type InstalledPorts func(namespace, name string, ports []int32) map[int32]bool

// portReturn - a port on its way back to the ingress service.
type portReturn struct {
	// since - when it began to wait for loxilb-ingress.
	since time.Time
	// sent - its L4 rule may go to the peers: loxilb-ingress has its
	// Ingresses, or was waited for long enough. It is not held back again
	// for a later change of them.
	sent bool
}

// BringPortsBack - with routes served directly, keep a port's direct rules
// until its L4 rule serves it once it goes back to the ingress service: hold
// that rule back until loxilb-ingress has the port's Ingresses, as
// ingressesOn has them, and keep the direct rules until installed says every
// peer has it. Called before the informers start.
func (gm *GatewayManager) BringPortsBack(kubeInformerFactory informers.SharedInformerFactory, ingressesOn IngressesOn, installed InstalledPorts) {
	ingressInformer := kubeInformerFactory.Networking().V1().Ingresses()
	gm.ingressLister = ingressInformer.Lister()
	gm.ingressListerSynced = ingressInformer.Informer().HasSynced
	gm.ingressesOn = ingressesOn
	gm.installedPorts = installed

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: gm.kubeClient.CoreV1().Events("")})
	gm.eventRecorder = broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kube-loxilb"})

	// A loxilb-ingress pod reports on the Ingress.
	ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    gm.ingressChanged,
		UpdateFunc: func(_, cur interface{}) { gm.ingressChanged(cur) },
		DeleteFunc: gm.ingressChanged,
	})
}

// ingressChanged - queue the Gateway of a route's Ingress, while one of its
// ports waits for loxilb-ingress.
func (gm *GatewayManager) ingressChanged(obj interface{}) {
	if deleted, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deleted.Obj
	}
	ing, ok := obj.(*netv1.Ingress)
	if !ok {
		return
	}
	namespace, name := ing.Annotations["parent-gateway-namespace"], ing.Annotations["parent-gateway"]
	if namespace == "" || name == "" {
		return
	}
	if key := GenKey(namespace, name); gm.bringingBack(key) {
		gm.queue.Add(key)
	}
}

// IngressServiceSynced - the load-balancer manager has synced a Service: if
// it is the ingress service of a Gateway with a port on its way back, the
// Gateway looks again at whether the port's L4 rule is on the peers. Called
// with that manager's cache locked; it only queues.
func (gm *GatewayManager) IngressServiceSynced(namespace, name string) {
	gateway, ok := strings.CutSuffix(name, "-ingress-service")
	if !ok {
		return
	}
	if key := GenKey(namespace, gateway); gm.bringingBack(key) {
		gm.queue.Add(key)
	}
}

// bringingBack - whether a port of the Gateway under key is on its way back.
func (gm *GatewayManager) bringingBack(key string) bool {
	gm.returningMu.Lock()
	defer gm.returningMu.Unlock()
	return len(gm.returning[key]) > 0
}

// portReturnOf - the return of a port of the Gateway under key, begun now if
// it was not under way.
func (gm *GatewayManager) portReturnOf(key string, port v1.PortNumber) portReturn {
	gm.returningMu.Lock()
	defer gm.returningMu.Unlock()
	if gm.returning == nil {
		gm.returning = map[string]map[v1.PortNumber]*portReturn{}
	}
	if gm.returning[key] == nil {
		gm.returning[key] = map[v1.PortNumber]*portReturn{}
	}
	ret, ok := gm.returning[key][port]
	if !ok {
		ret = &portReturn{since: gm.clock()}
		gm.returning[key][port] = ret
	}
	return *ret
}

// portReturnSent - the port's L4 rule may go to the peers.
func (gm *GatewayManager) portReturnSent(key string, port v1.PortNumber) {
	gm.returningMu.Lock()
	defer gm.returningMu.Unlock()
	if ret := gm.returning[key][port]; ret != nil {
		ret.sent = true
	}
}

// keepReturns - forget the returns of the Gateway's ports that are not on
// their way back any more.
func (gm *GatewayManager) keepReturns(key string, leaving map[v1.PortNumber]bool) {
	gm.returningMu.Lock()
	defer gm.returningMu.Unlock()
	for port := range gm.returning[key] {
		if !leaving[port] {
			delete(gm.returning[key], port)
		}
	}
	if len(gm.returning[key]) == 0 {
		delete(gm.returning, key)
	}
}

// recordGatewayWarning - a warning event on gw. Recorded against a reference:
// the recorder's scheme does not know the Gateway type.
func (gm *GatewayManager) recordGatewayWarning(gw *v1.Gateway, reason, message string) {
	if gm.eventRecorder == nil {
		return
	}
	ref := &corev1.ObjectReference{
		APIVersion:      v1.SchemeGroupVersion.String(),
		Kind:            "Gateway",
		Namespace:       gw.Namespace,
		Name:            gw.Name,
		UID:             gw.UID,
		ResourceVersion: gw.ResourceVersion,
	}
	gm.eventRecorder.Event(ref, corev1.EventTypeWarning, reason, message)
}

func (gm *GatewayManager) clock() time.Time {
	if gm.now != nil {
		return gm.now()
	}
	return time.Now()
}

// leavingPorts - the ports of a Gateway's direct rules its ingress service
// carries, as ingressServicePorts has them: those on their way back. The
// rules of a port that is neither, one no listener has now or one an
// InferencePool has taken, go at once.
func leavingPorts(rules map[string]*corev1.Service, carried []corev1.ServicePort) map[v1.PortNumber]bool {
	ingress := map[v1.PortNumber]bool{}
	for _, p := range carried {
		ingress[v1.PortNumber(p.Port)] = true
	}
	leaving := map[v1.PortNumber]bool{}
	for _, svc := range rules {
		if len(svc.Spec.Ports) == 0 {
			continue
		}
		if port := v1.PortNumber(svc.Spec.Ports[0].Port); ingress[port] {
			leaving[port] = true
		}
	}
	return leaving
}

// bringBack - for each port of gw on its way back to the ingress service,
// whether its L4 rule is held back (hold), and whether its direct rules stay
// (keep). While one of them waits, errHandoverPending brings the Gateway
// back.
func (gm *GatewayManager) bringBack(gw *v1.Gateway, leaving map[v1.PortNumber]bool) (hold, keep map[v1.PortNumber]bool, err error) {
	key := GenKey(gw.Namespace, gw.Name)
	hold, keep = map[v1.PortNumber]bool{}, map[v1.PortNumber]bool{}
	gm.keepReturns(key, leaving)
	if len(leaving) == 0 {
		return hold, keep, nil
	}

	ports := make([]v1.PortNumber, 0, len(leaving))
	for p := range leaving {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })

	var waiting []string
	var sent []int32
	for _, port := range ports {
		ret := gm.portReturnOf(key, port)
		if !ret.sent {
			done, why, err := gm.ingressesProgrammed(gw, port)
			if err != nil {
				why, done = err.Error(), false
			}
			if !done {
				if left := ingressProgrammedTimeout - gm.clock().Sub(ret.since); left > 0 {
					hold[port], keep[port] = true, true
					waiting = append(waiting, fmt.Sprintf("port %d waits for loxilb-ingress: %s", port, why))
					// Nothing else may bring the Gateway back when the
					// wait is over.
					gm.queue.AddAfter(key, left)
					continue
				}
				msg := fmt.Sprintf("port %d: loxilb-ingress did not report the port's Ingresses installed in %v (%s); its L4 rule is sent now",
					port, ingressProgrammedTimeout, why)
				klog.Warningf("gateway %s/%s: %s", gw.Namespace, gw.Name, msg)
				gm.recordGatewayWarning(gw, reasonIngressNotProgrammed, msg)
			} else {
				klog.Infof("gateway %s/%s: the L4 rule of port %d is sent: %s", gw.Namespace, gw.Name, port, why)
			}
			gm.portReturnSent(key, port)
		}
		sent = append(sent, int32(port))
	}

	if len(sent) > 0 && gm.installedPorts != nil {
		installed := gm.installedPorts(gw.Namespace, ingressServiceName(gw), sent)
		for _, p := range sent {
			if !installed[p] {
				keep[v1.PortNumber(p)] = true
				waiting = append(waiting, fmt.Sprintf("port %d waits for its L4 rule on every peer", p))
			}
		}
	}

	if len(waiting) > 0 {
		return hold, keep, fmt.Errorf("gateway %s/%s: %s: %w", gw.Namespace, gw.Name, strings.Join(waiting, "; "), errHandoverPending)
	}
	return hold, keep, nil
}

// ingressesProgrammed - whether every loxilb-ingress pod that reports has
// installed the Ingresses of gw's listeners on port, as the route manager
// writes them; and why, or which have not.
func (gm *GatewayManager) ingressesProgrammed(gw *v1.Gateway, port v1.PortNumber) (bool, string, error) {
	if gm.ingressesOn == nil || gm.ingressLister == nil {
		return true, "loxilb-ingress is not waited for", nil
	}
	pods := gm.reportingPods()
	if len(pods) == 0 {
		return true, "no ready loxilb-ingress pod reports which Ingresses it has installed", nil
	}
	want, err := gm.ingressesOn(gw, port)
	if err != nil {
		return false, "", err
	}
	if len(want) == 0 {
		return true, "no route on the port has an Ingress", nil
	}

	var missing []string
	for _, w := range want {
		have, err := gm.ingressLister.Ingresses(w.Namespace).Get(w.Name)
		if errors.IsNotFound(err) || (err == nil && !equality.Semantic.DeepEqual(have.Spec, w.Spec)) {
			missing = append(missing, fmt.Sprintf("Ingress %s/%s is not written yet", w.Namespace, w.Name))
			continue
		}
		if err != nil {
			return false, "", err
		}
		generation := strconv.FormatInt(have.Generation, 10)
		var notYet []string
		for _, pod := range pods {
			if have.Annotations[programmedAnnotationPrefix+pod.Name] != generation {
				notYet = append(notYet, pod.Namespace+"/"+pod.Name)
			}
		}
		if len(notYet) > 0 {
			missing = append(missing, fmt.Sprintf("Ingress %s/%s (generation %s) is not on %s",
				w.Namespace, w.Name, generation, strings.Join(notYet, ", ")))
		}
	}
	if len(missing) > 0 {
		return false, strings.Join(missing, "; "), nil
	}
	return true, "loxilb-ingress has the port's Ingresses", nil
}

// reportingPods - the ready loxilb-ingress pods that report which Ingresses
// they have installed. A pod whose name does not fit in the annotation key
// cannot, and is not waited for.
func (gm *GatewayManager) reportingPods() []*corev1.Pod {
	var pods []*corev1.Pod
	for _, pod := range gm.loxilbIngressPods() {
		if !podReady(pod) || pod.Labels[programmedReportLabel] != programmedReportV1 {
			continue
		}
		if errs := validation.IsQualifiedName(programmedAnnotationPrefix + pod.Name); len(errs) > 0 {
			klog.V(2).Infof("loxilb-ingress pod %s/%s cannot report on an Ingress: %s", pod.Namespace, pod.Name, strings.Join(errs, "; "))
			continue
		}
		pods = append(pods, pod)
	}
	return pods
}
