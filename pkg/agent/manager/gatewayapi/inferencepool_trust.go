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
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A pool's Service is served on its Gateway listener's address and port,
// beside the other pools there, which the load-balancer manager lets it share
// (listenerShare). That is a Gateway's listener given to whatever the Service
// names, so the load-balancer manager asks this manager whether it made the
// Service, rather than reading the label any Service can carry. This manager
// in turn makes a Service only for a listener the pool's route attaches to,
// as Gateway API has a route attach: a class this controller serves, and a
// listener that takes the route's kind, namespace and hostnames.

// generatedService - what a Service generated for a pool is made with: the
// Gateway listener's address and port.
type generatedService struct {
	address string
	port    int32
}

// generatedServices - the Services this manager made, by pool and then name.
// A pool is in it once a reconcile decided its Services, which may be none.
// While a reconcile applies them, a name can stand for the Service as it was
// and as it is about to be.
type generatedServices struct {
	mu     sync.RWMutex
	byPool map[types.NamespacedName]map[string][]generatedService
}

// allow - add the Services a reconcile is about to apply to those of the
// pool, which still holds them as they were and the ones about to be deleted.
func (g *generatedServices) allow(pool types.NamespacedName, services []*corev1.Service) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byPool == nil {
		g.byPool = map[types.NamespacedName]map[string][]generatedService{}
	}
	if g.byPool[pool] == nil {
		g.byPool[pool] = map[string][]generatedService{}
	}
	for _, svc := range services {
		made := generatedServiceOf(svc)
		if !slices.Contains(g.byPool[pool][svc.Name], made) {
			g.byPool[pool][svc.Name] = append(g.byPool[pool][svc.Name], made)
		}
	}
}

// settle - the pool's Services are exactly these now.
func (g *generatedServices) settle(pool types.NamespacedName, services []*corev1.Service) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byPool == nil {
		g.byPool = map[types.NamespacedName]map[string][]generatedService{}
	}
	set := make(map[string][]generatedService, len(services))
	for _, svc := range services {
		set[svc.Name] = []generatedService{generatedServiceOf(svc)}
	}
	g.byPool[pool] = set
}

// forget - the pool is gone.
func (g *generatedServices) forget(pool types.NamespacedName) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byPool, pool)
}

// lookup - whether the Service named name of the pool was made as svc is;
// known is false while no reconcile has decided on the pool.
func (g *generatedServices) lookup(pool types.NamespacedName, name string, svc generatedService) (made, known bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	set, known := g.byPool[pool]
	if !known {
		return false, false
	}
	return slices.Contains(set[name], svc), true
}

func generatedServiceOf(svc *corev1.Service) generatedService {
	var port int32
	if len(svc.Spec.Ports) > 0 {
		port = svc.Spec.Ports[0].Port
	}
	return generatedService{address: svc.Spec.LoadBalancerIP, port: port}
}

// Generated - whether svc is a Service this manager made for a pool, as it
// made it: the pool's, on the listener address and port it was given, and
// no other port. decided is false while this manager cannot tell yet: it has
// not reconciled the pool since it started.
func (m *InferencePoolManager) Generated(svc *corev1.Service) (generated, decided bool) {
	name := svc.Labels[poolOwnerLabel]
	if name == "" {
		return false, true
	}
	pool := types.NamespacedName{Namespace: svc.Namespace, Name: name}
	made, known := m.generated.lookup(pool, svc.Name, generatedServiceOf(svc))
	if !known {
		if m.poolSynced != nil && !m.poolSynced() {
			return false, false
		}
		if m.poolLister != nil {
			if _, err := m.poolLister.InferencePools(pool.Namespace).Get(pool.Name); errors.IsNotFound(err) {
				// No such pool: nothing will make the Service.
				return false, true
			}
		}
		return false, false
	}
	if !made || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		return false, true
	}
	return true, true
}

// servesGateway - whether the Gateway's class names this controller. A pool
// on another controller's Gateway is that controller's to serve.
func (m *InferencePoolManager) servesGateway(gw *v1.Gateway) bool {
	if m.gatewayClassLister == nil {
		return false
	}
	gc, err := m.gatewayClassLister.Get(string(gw.Spec.GatewayClassName))
	if err != nil {
		return false
	}
	return gc.Spec.ControllerName == v1.GatewayController(m.networkConfig.LoxilbGatewayClass)
}

// namespaceLabels - the labels of a namespace, read from the cache.
func (m *InferencePoolManager) namespaceLabels() namespaceLabels {
	if m.namespaceLister == nil {
		return nil
	}
	return func(name string) (labels.Set, error) {
		ns, err := m.namespaceLister.Get(name)
		if err != nil {
			return nil, err
		}
		return labels.Set(ns.Labels), nil
	}
}

// poolListener - the listener a pool is served on through a parentRef of its
// route: the first the route attaches to that is HTTP or HTTPS, in the
// Gateway's order; nil when there is none, with why.
//
// It is the listener findListenerForRoute picks whenever the route attaches
// to that one: the named one, the first on the port, or the first HTTP or
// HTTPS one when the parentRef names neither.
func poolListener(gw *v1.Gateway, route *v1.HTTPRoute, ref v1.ParentReference, nsLabels namespaceLabels) (*v1.Listener, string) {
	attachment := attachRoute(gw, ref, attachingRoute{
		Kind:      "HTTPRoute",
		Namespace: route.Namespace,
		Hostnames: route.Spec.Hostnames,
	}, nsLabels)
	if !attachment.Accepted() {
		return nil, attachment.Message
	}
	for _, a := range attachment.Listeners {
		if a.Listener.Protocol == v1.HTTPProtocolType || a.Listener.Protocol == v1.HTTPSProtocolType {
			return a.Listener, ""
		}
	}
	return nil, fmt.Sprintf("HTTPRoute %s/%s attaches to no HTTP or HTTPS listener of Gateway %s/%s",
		route.Namespace, route.Name, gw.Namespace, gw.Name)
}
