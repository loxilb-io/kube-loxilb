/*
* Copyright (c) 2024 NetLOX Inc
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
	"maps"
	"path"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigsinformer "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1"
	sigslister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
)

const (
	gwMgrName = "GatewayManager"
)

type GatewayManager struct {
	kubeClient          clientset.Interface
	sigsClient          sigsclientset.Interface
	networkConfig       *config.NetworkConfig
	externalIPPool      *ippool.IPPool
	gatewayInformer     sigsinformer.GatewayInformer
	gatewayLister       sigslister.GatewayLister
	gatewayListerSynced cache.InformerSynced
	// httpRouteLister - to see which listeners an InferencePool has claimed.
	httpRouteLister       sigslister.HTTPRouteLister
	httpRouteListerSynced cache.InformerSynced
	// gatewayClassLister - to tell this controller's Gateways from others'.
	gatewayClassLister       sigslister.GatewayClassLister
	gatewayClassListerSynced cache.InformerSynced
	// namespaceLister - for a listener that takes routes from the namespaces
	// a selector picks, when its attached routes are counted.
	namespaceLister       corelister.NamespaceLister
	namespaceListerSynced cache.InformerSynced
	// routeSources - route managers that count their routes on listeners.
	routeSources   []RouteSource
	routeSourcesMu sync.Mutex
	// serviceLister - the generated Services of direct routes, and the
	// backends they name.
	serviceLister       corelister.ServiceLister
	serviceListerSynced cache.InformerSynced

	// directPeers - set when HTTPRoutes may be served directly on a
	// Gateway's listeners (direct.go); nil serves them through the ingress
	// service only.
	directPeers     DirectPeers
	referenceGrants *ReferenceGrants

	queue workqueue.RateLimitingInterface
}

func NewGatewayManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	externalIPPool *ippool.IPPool,
	kubeInformerFactory informers.SharedInformerFactory,
	sigsInformerFactory externalversions.SharedInformerFactory) *GatewayManager {

	gatewayInformer := sigsInformerFactory.Gateway().V1().Gateways()
	httpRouteInformer := sigsInformerFactory.Gateway().V1().HTTPRoutes()
	gatewayClassInformer := sigsInformerFactory.Gateway().V1().GatewayClasses()
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()
	serviceInformer := kubeInformerFactory.Core().V1().Services()
	manager := &GatewayManager{
		kubeClient:          kubeClient,
		sigsClient:          sigsClient,
		networkConfig:       networkConfig,
		externalIPPool:      externalIPPool,
		gatewayInformer:     gatewayInformer,
		gatewayLister:       gatewayInformer.Lister(),
		gatewayListerSynced: gatewayInformer.Informer().HasSynced,
		httpRouteLister:     httpRouteInformer.Lister(),
		// Waited for in Run: a listener reads as unclaimed until the routes
		// are in the cache, and an ingress service would take its port.
		httpRouteListerSynced: httpRouteInformer.Informer().HasSynced,

		gatewayClassLister:       gatewayClassInformer.Lister(),
		gatewayClassListerSynced: gatewayClassInformer.Informer().HasSynced,
		namespaceLister:          namespaceInformer.Lister(),
		namespaceListerSynced:    namespaceInformer.Informer().HasSynced,
		serviceLister:            serviceInformer.Lister(),
		serviceListerSynced:      serviceInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "gateway"),
	}

	// A direct route's Service reports when its rule is on the peers, which
	// lets the ingress service leave the port.
	serviceInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(cur interface{}) { manager.enqueueServiceGateway(cur) },
			UpdateFunc: func(old, cur interface{}) { manager.enqueueServiceGateway(cur) },
			DeleteFunc: func(old interface{}) { manager.enqueueServiceGateway(old) },
		},
	)

	// A Gateway is this controller's once its class names it, and the class
	// may come after the Gateway. A class's controllerName cannot change.
	gatewayClassInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(cur interface{}) { manager.enqueueClassGateways(cur) },
			DeleteFunc: func(old interface{}) { manager.enqueueClassGateways(old) },
		},
	)

	// A route attaching an InferencePool to a listener changes what this
	// gateway should expose, and touches no Gateway object.
	// It also changes how many routes attach to a listener, the Gateways a
	// route leaves included.
	httpRouteInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) { manager.enqueueRouteGateways(cur) },
			UpdateFunc: func(old, cur interface{}) {
				manager.enqueueRouteGateways(old)
				manager.enqueueRouteGateways(cur)
			},
			DeleteFunc: func(old interface{}) { manager.enqueueRouteGateways(old) },
		},
	)

	// A namespace's labels decide which routes a listener with a namespace
	// selector takes.
	namespaceInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: manager.namespaceUpdated,
		},
	)

	manager.gatewayInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) {
				manager.enqueueObject(cur)
			},
			UpdateFunc: func(old, cur interface{}) {
				manager.enqueueObject(cur)
			},
			DeleteFunc: func(old interface{}) {
				manager.enqueueObject(old)
			},
		},
	)

	return manager
}

// GenKey generate key for queue
func GenKey(ns, name string) string {
	return path.Join(ns, name)
}

func (gm *GatewayManager) enqueueObject(obj interface{}) {
	gw, ok := obj.(*v1.Gateway)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		// A delete the watch missed arrives as a tombstone. The Gateway is
		// the object it wraps, and its last known addresses are what the
		// delete hands back to the address pool.
		gw, ok = deletedState.Obj.(*v1.Gateway)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Gateway object: %v", deletedState.Obj)
			return
		}
	}

	gm.queue.Add(gw)
}

// enqueueClassGateways - queue the gateways of a GatewayClass.
func (gm *GatewayManager) enqueueClassGateways(obj interface{}) {
	gc, ok := obj.(*v1.GatewayClass)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		gc, ok = deletedState.Obj.(*v1.GatewayClass)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-GatewayClass object: %v", deletedState.Obj)
			return
		}
	}

	gateways, err := gm.gatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("gatewayClass %s: unable to list gateways: %v", gc.Name, err)
		return
	}
	for _, gw := range gateways {
		if string(gw.Spec.GatewayClassName) == gc.Name {
			gm.queue.Add(gw)
		}
	}
}

// ServeRoutesDirectly - serve HTTPRoutes on the listeners where peers allows
// it as direct routes (direct.go), with grants for the backends in other
// namespaces.
func (gm *GatewayManager) ServeRoutesDirectly(peers DirectPeers, grants *ReferenceGrants) {
	gm.directPeers = peers
	gm.referenceGrants = grants
	grants.AddHandler(func(v1.ReferenceGrantSpec) { gm.EnqueueAll() })
}

// EnqueueAll - queue every Gateway; for a change that may move any of them,
// such as a peer reporting what it is.
func (gm *GatewayManager) EnqueueAll() {
	gateways, err := gm.gatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("unable to list gateways: %v", err)
		return
	}
	for _, gw := range gateways {
		gm.queue.Add(gw)
	}
}

// enqueueServiceGateway - queue the Gateway that controls a generated
// Service.
func (gm *GatewayManager) enqueueServiceGateway(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	svc, ok := obj.(*corev1.Service)
	if !ok || svc.Labels[gatewayRouteLabel] == "" {
		return
	}
	owner := metav1.GetControllerOf(svc)
	if owner == nil || owner.Kind != "Gateway" {
		return
	}
	if gw, err := gm.gatewayLister.Gateways(svc.Namespace).Get(owner.Name); err == nil {
		gm.queue.Add(gw)
	}
}

// namespaceUpdated - queue the gateways with a listener that selects
// namespaces by label, when a namespace's labels change.
func (gm *GatewayManager) namespaceUpdated(old, cur interface{}) {
	oldNs, okOld := old.(*corev1.Namespace)
	curNs, okCur := cur.(*corev1.Namespace)
	if !okOld || !okCur || maps.Equal(oldNs.Labels, curNs.Labels) {
		return
	}

	gateways, err := gm.gatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("namespace %s: unable to list gateways: %v", curNs.Name, err)
		return
	}
	for _, gw := range gateways {
		for _, l := range gw.Spec.Listeners {
			if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil &&
				l.AllowedRoutes.Namespaces.From != nil && *l.AllowedRoutes.Namespaces.From == v1.NamespacesFromSelector {
				gm.queue.Add(gw)
				break
			}
		}
	}
}

// servesGateway - whether the Gateway's class names this controller.
func (gm *GatewayManager) servesGateway(gw *v1.Gateway) bool {
	gc, err := gm.gatewayClassLister.Get(string(gw.Spec.GatewayClassName))
	if err != nil {
		return false
	}
	return gc.Spec.ControllerName == v1.GatewayController(gm.networkConfig.LoxilbGatewayClass)
}

// enqueueRouteGateways - queue the gateways a route names as parents.
func (gm *GatewayManager) enqueueRouteGateways(obj interface{}) {
	route, ok := obj.(*v1.HTTPRoute)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		route, ok = deletedState.Obj.(*v1.HTTPRoute)
		if !ok {
			return
		}
	}

	for _, parentRef := range route.Spec.ParentRefs {
		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		gateway, err := gm.gatewayLister.Gateways(namespace).Get(string(parentRef.Name))
		if err != nil {
			continue
		}
		gm.queue.Add(gateway)
	}
}

func (gm *GatewayManager) Run(stopCh <-chan struct{}) {
	defer gm.queue.ShutDown()

	klog.Infof("Starting %s", gwMgrName)
	defer klog.Infof("Shutting down %s", gwMgrName)

	// The routes have to be in the cache before any Gateway is handled. An
	// empty route cache makes every listener look unclaimed, and the ingress
	// service then takes the port of a listener an InferencePool is about to
	// use. A Gateway applied before its routes still has that moment; the
	// load-balancer manager retries the pool's Service until the port is
	// released instead of failing on it.
	if !cache.WaitForNamedCacheSync(
		gwMgrName,
		stopCh,
		gm.gatewayListerSynced,
		gm.httpRouteListerSynced,
		gm.gatewayClassListerSynced,
		gm.namespaceListerSynced,
		gm.serviceListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(gm.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (gm *GatewayManager) worker() {
	for gm.processNextWorkItem() {
	}
}

func (gm *GatewayManager) processNextWorkItem() bool {
	obj, quit := gm.queue.Get()
	if quit {
		return false
	}
	defer gm.queue.Done(obj)

	if key, ok := obj.(*v1.Gateway); !ok {
		gm.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := gm.reconcile(key); err == nil {
		gm.queue.Forget(obj)
	} else {
		gm.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing gateway %s/%s, requeuing. Error: %v", key.Namespace, key.Name, err)
	}
	return true
}

func (gm *GatewayManager) reconcile(entry *v1.Gateway) error {
	gw, err := gm.gatewayLister.Gateways(entry.Namespace).Get(entry.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return gm.deleteGateway(entry.Namespace, entry.Name, allocatedAddress(entry), entry.Annotations["ident-ipam"])
		}
		return err
	}
	return gm.createGateway(gw)
}

// allocatedAddress - the address this controller allocated for the Gateway,
// or "" when it allocated none. Earlier builds noted it as a label.
func allocatedAddress(gw *v1.Gateway) string {
	if addr := gw.Annotations["ipam-address"]; addr != "" {
		return addr
	}
	return gw.Labels["ipam-address"]
}

// deleteGateway - give back what this controller took for a Gateway: the
// address it allocated, which it noted as ipam-address, and the ingress
// service. An address the Gateway came with, or one of another controller's
// Gateway, was never this controller's to return.
func (gm *GatewayManager) deleteGateway(ns, name, ipamAddress, identIPAM string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	if ipamAddress != "" {
		gm.externalIPPool.ReturnIPAddr(ipamAddress, identIPAM)
	}

	err := gm.deleteIngressLbService(ctx, ns, name)
	klog.Infof("gateway %s/%s is deleted.", ns, name)

	return err
}

func (gm *GatewayManager) createGateway(gw *v1.Gateway) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	if !gm.servesGateway(gw) {
		// Another controller's, or one whose class is not there yet - its
		// coming queues the Gateway. Neither gets an address, a status or an
		// ingress service from this one; an ingress service made while the
		// Gateway was of this controller's class goes.
		klog.V(2).Infof("gateway %s/%s: gatewayClass %s is not this controller's", gw.Namespace, gw.Name, gw.Spec.GatewayClassName)
		_, err := gm.kubeClient.CoreV1().Services(gw.Namespace).Get(ctx, fmt.Sprintf("%s-ingress-service", gw.Name), metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		return gm.deleteIngressLbService(ctx, gw.Namespace, gw.Name)
	}

	// The Gateway is the informer's; what follows writes into it.
	gw = gw.DeepCopy()
	updatedGw := gw
	var err error

	// If gateway has no address, get new IP in ipam
	if len(gw.Spec.Addresses) == 0 {
		ipPool := gm.externalIPPool
		newIP, identIPAM := ipPool.GetNewIPAddr(GenKey(gw.Namespace, gw.Name), 0, "")
		if newIP == nil {
			klog.Error("gateway ip pool failure")
			klog.Exit("kube-loxilb cant run optimally anymore")
			return fmt.Errorf("failed to generate gateway address")
		}

		// update Gateway spec. If fail, return error
		gw.Spec.Addresses = append(gw.Spec.Addresses, v1.GatewaySpecAddress{Value: newIP.String()})
		if gw.Labels == nil {
			gw.Labels = make(map[string]string)
		}
		gw.Labels["implementation"] = implementation
		if gw.Annotations == nil {
			gw.Annotations = make(map[string]string)
		}
		gw.Annotations["ipam-address"] = newIP.String()
		gw.Annotations["ident-ipam"] = identIPAM

		updatedGw, err = gm.sigsClient.GatewayV1().Gateways(gw.Namespace).Update(ctx, gw, metav1.UpdateOptions{})
		if err != nil {
			ipPool.ReturnIPAddr(newIP.String(), identIPAM)
			klog.Errorf("gateway %s/%s unable to update configuration. err: %v", gw.Namespace, gw.Name, err)
			return fmt.Errorf("unable to update gateway configuration")
		}

		klog.Infof("gateway %s/%s is assigned IP %s", gw.Namespace, gw.Name, newIP.String())
	}

	// Direct routes first: a port leaves the ingress service only once its
	// rules are on the peers. A port that goes back to the ingress service
	// loses its direct routes after the ingress service has it again.
	desiredDirect, readyDirect := map[string]bool{}, map[v1.PortNumber]bool{}
	var directErr error
	if gm.directPeers != nil {
		direct, why := directPortsOf(updatedGw, gm.listenersClaimedByInferencePools(updatedGw), gm.directPeers)
		for port, reason := range why {
			klog.V(2).Infof("gateway %s/%s: port %d is served through the ingress service: %s", updatedGw.Namespace, updatedGw.Name, port, reason)
		}
		desiredDirect, readyDirect, directErr = gm.applyDirectRoutes(ctx, updatedGw, direct)
		if directErr != nil {
			klog.Errorf("gateway %s/%s: direct routes: %v", updatedGw.Namespace, updatedGw.Name, directErr)
		}
	}

	// The ingress service next: whether it could be made is part of the
	// status.
	svc, serviceErr := gm.createIngressLbService(ctx, updatedGw, readyDirect)
	if serviceErr != nil {
		klog.Errorf("gateway %s/%s failed to create service. err: %v", updatedGw.Namespace, updatedGw.Name, serviceErr)
	}
	if serviceErr == nil && directErr == nil {
		if err := gm.pruneDirectRoutes(ctx, updatedGw, desiredDirect); err != nil {
			directErr = err
		}
	}

	if err := gm.updateGatewayStatus(ctx, updatedGw, serviceErr); err != nil {
		klog.Errorf("gateway %s/%s unable to update gateway status. err: %v", updatedGw.Namespace, updatedGw.Name, err)
		if serviceErr == nil {
			return err
		}
	}
	if serviceErr != nil {
		return serviceErr
	}
	if directErr != nil {
		return directErr
	}

	if svc != nil {
		klog.Infof("gateway %s/%s is assigned an IP %v and created a service %s/%s.", updatedGw.Namespace, updatedGw.Name, updatedGw.Spec.Addresses, svc.Namespace, svc.Name)
	} else {
		klog.Infof("gateway %s/%s is assigned an IP %v (no service created - no HTTP/HTTPS listeners).", updatedGw.Namespace, updatedGw.Name, updatedGw.Spec.Addresses)
	}
	klog.Infof("gateway %s/%s is created.", updatedGw.Namespace, updatedGw.Name)
	return nil
}

// servicePortsEqual - compare on what the gateway decides, ignoring the node
// port the API server allocated.
func servicePortsEqual(existing, desired []corev1.ServicePort) bool {
	if len(existing) != len(desired) {
		return false
	}

	for i := range desired {
		if existing[i].Name != desired[i].Name ||
			existing[i].Port != desired[i].Port ||
			existing[i].TargetPort != desired[i].TargetPort ||
			existing[i].Protocol != desired[i].Protocol {
			return false
		}
	}

	return true
}

// listenersClaimedByInferencePools - listeners that an InferencePool-backed
// HTTPRoute attaches to.
//
// The ingress service exists to hand a listener's traffic to loxilb-ingress
// for path routing. An InferencePool wants that same listener pointed at model
// server pods instead. Both cannot own one address and port, so the pool wins
// and the ingress service leaves the listener alone.
func (gm *GatewayManager) listenersClaimedByInferencePools(gateway *v1.Gateway) map[v1.SectionName]bool {
	claimed := map[v1.SectionName]bool{}
	if gm.httpRouteLister == nil {
		return claimed
	}

	routes, err := gm.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("gateway %s/%s: unable to list HTTPRoutes: %v", gateway.Namespace, gateway.Name, err)
		return claimed
	}
	return claimedListeners(gateway, routes)
}

// claimedListeners - the listeners of gateway an InferencePool-backed route
// among routes attaches to.
func claimedListeners(gateway *v1.Gateway, routes []*v1.HTTPRoute) map[v1.SectionName]bool {
	claimed := map[v1.SectionName]bool{}
	for _, route := range routes {
		if len(poolsReferencedBy(route)) == 0 {
			continue
		}

		for _, parentRef := range route.Spec.ParentRefs {
			namespace := route.Namespace
			if parentRef.Namespace != nil {
				namespace = string(*parentRef.Namespace)
			}
			if namespace != gateway.Namespace || string(parentRef.Name) != gateway.Name {
				continue
			}

			if listener := findListenerForRoute(gateway, parentRef); listener != nil {
				claimed[listener.Name] = true
			}
		}
	}

	return claimed
}

func (gm *GatewayManager) deleteIngressLbService(ctx context.Context, gwNs, gwName string) error {
	svcList, err := gm.kubeClient.CoreV1().Services(gwNs).List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("Gateway: There is no service to delete.")
			return nil
		}
		klog.Errorf("Gateway: get services list (ns: %s) failure. err: %v", gwNs, err)
		return err
	}

	for _, svc := range svcList.Items {
		if svc.Annotations["gateway-api-controller"] == gm.networkConfig.LoxilbGatewayClass &&
			svc.Annotations["parent-gateway"] == gwName {

			err = gm.kubeClient.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Gateway: delete loadbalancer service %s/%s failure. err: %v", svc.Namespace, svc.Name, err)
				return err
			}
			klog.Infof("service %s/%s is deleted by gateway %s", svc.Namespace, svc.Name, gwName)
		}
	}

	return nil
}

// createIngressLbService creates a LoadBalancer Service for the Gateway.
// Its ports are those of the Gateway's HTTP and HTTPS listeners, as
// ingressServicePorts gives them. Service is only created if there are
// HTTP or HTTPS protocol listeners.
//
// Returns:
//   - *corev1.Service: The created or existing Service, or nil if no HTTP/HTTPS listeners exist
//   - error: Any error encountered during service creation
func (gm *GatewayManager) createIngressLbService(ctx context.Context, gateway *v1.Gateway, direct map[v1.PortNumber]bool) (*corev1.Service, error) {
	newService := corev1.Service{}
	newService.Name = fmt.Sprintf("%s-ingress-service", gateway.Name)
	newService.Namespace = gateway.Namespace

	existing, getErr := gm.kubeClient.CoreV1().Services(newService.Namespace).Get(ctx, newService.Name, metav1.GetOptions{})

	if len(gateway.Spec.Addresses) == 0 {
		return nil, fmt.Errorf("gateway has no external IP address")
	}

	servicePorts := ingressServicePorts(gateway, gm.listenersClaimedByInferencePools(gateway), direct)

	// If no HTTP/HTTPS listeners are left to serve, there is no service to
	// keep - which is also how an ingress service created before a pool
	// claimed its listener gets cleaned up.
	if len(servicePorts) == 0 {
		if getErr == nil {
			if err := gm.kubeClient.CoreV1().Services(existing.Namespace).Delete(ctx, existing.Name, metav1.DeleteOptions{}); err != nil {
				return nil, err
			}
			klog.Infof("service %s/%s is deleted - every listener is served elsewhere", existing.Namespace, existing.Name)
		} else {
			klog.Infof("gateway %s/%s has no HTTP/HTTPS listeners to serve, skipping service creation", gateway.Namespace, gateway.Name)
		}
		return nil, nil
	}

	if getErr == nil {
		if servicePortsEqual(existing.Spec.Ports, servicePorts) && controlledBy(existing, gateway) {
			return existing, nil
		}

		// Controlled by the Gateway, so the load-balancer manager lets it
		// share the Gateway's address and ports with the direct routes while
		// a port moves between them. The load-balancer manager writes the
		// same Service's status, so a stale copy is read again rather than
		// failing the Gateway on it.
		var svc *corev1.Service
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			updated := existing.DeepCopy()
			updated.Spec.Ports = servicePorts
			updated.OwnerReferences = gatewayControllerRef(gateway)
			var err error
			svc, err = gm.kubeClient.CoreV1().Services(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
			if errors.IsConflict(err) {
				if fresh, getErr := gm.kubeClient.CoreV1().Services(existing.Namespace).Get(ctx, existing.Name, metav1.GetOptions{}); getErr == nil {
					existing = fresh
				}
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		klog.Infof("service %s/%s ports updated to match the gateway's listeners", svc.Namespace, svc.Name)
		return svc, nil
	}

	newService.SetAnnotations(gateway.Annotations)
	if newService.Annotations == nil {
		newService.Annotations = map[string]string{}
	}

	newService.Annotations["gateway-api-controller"] = gm.networkConfig.LoxilbGatewayClass
	newService.Annotations["parent-gateway"] = gateway.Name
	newService.Annotations["ipam-address"] = gateway.Spec.Addresses[0].Value

	newService.SetLabels(map[string]string{
		"implementation": implementation,
	})
	newService.OwnerReferences = gatewayControllerRef(gateway)

	loadBalancerClass := gm.networkConfig.LoxilbLoadBalancerClass
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Spec.Addresses[0].Value
	newService.Spec.Ports = servicePorts

	if newService.Spec.Selector == nil {
		newService.Spec.Selector = map[string]string{}
	}
	newService.Spec.Selector["app"] = "loxilb-ingress"

	svc, err := gm.kubeClient.CoreV1().Services(newService.Namespace).Create(ctx, &newService, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return svc, nil
}

// ingressServicePorts - the ports of a Gateway's ingress service: one for each
// port its HTTP and HTTPS listeners use, however many of them share it.
//
// Port mapping:
//   - Service Port: the listener's port (80 for HTTP, 443 for HTTPS when unset)
//   - TargetPort: always the standard one (80 for HTTP, 443 for HTTPS), where
//     loxilb-ingress listens
//
// Listeners on one port with different hostnames are how a Gateway serves
// several hosts there, and loxilb-ingress tells them apart by host. A Service
// takes each port once, so a port for each listener made the API server
// refuse the whole Service. A port is named for its protocol and number: a
// listener name may be longer than a port name can be, and may hold dots.
//
// The first listener on a port decides its protocol; one of the other
// protocol there is left out, as the two cannot share it. A port that an
// InferencePool serves for any of its listeners is left to the pool: its
// Service carries the gateway address and that port already, and a second
// rule on them would make which one the data plane binds a race.
func ingressServicePorts(gateway *v1.Gateway, claimed map[v1.SectionName]bool, direct map[v1.PortNumber]bool) []corev1.ServicePort {
	type port struct {
		protocol  v1.ProtocolType
		listeners []v1.SectionName
		claimedBy v1.SectionName
	}
	ports := map[int32]*port{}
	var order []int32

	for _, listener := range gateway.Spec.Listeners {
		var number int32
		switch listener.Protocol {
		case v1.HTTPProtocolType:
			number = httpPort
		case v1.HTTPSProtocolType:
			number = httpsPort
		default:
			continue
		}
		if listener.Port != 0 {
			number = int32(listener.Port)
		}
		// Served by direct routes, whose rules are on the peers already.
		if direct[v1.PortNumber(number)] {
			continue
		}

		p, seen := ports[number]
		if !seen {
			p = &port{protocol: listener.Protocol}
			ports[number] = p
			order = append(order, number)
		}
		if p.protocol != listener.Protocol {
			klog.Warningf("gateway %s/%s: listener %s is %s on port %d, which listener %s has as %s - no ingress service port for it",
				gateway.Namespace, gateway.Name, listener.Name, listener.Protocol, number, p.listeners[0], p.protocol)
			continue
		}
		p.listeners = append(p.listeners, listener.Name)
		if claimed[listener.Name] && p.claimedBy == "" {
			p.claimedBy = listener.Name
		}
	}

	var servicePorts []corev1.ServicePort
	for _, number := range order {
		p := ports[number]
		if p.claimedBy != "" {
			klog.Infof("gateway %s/%s: listener %s is served by an InferencePool - no ingress service port %d for listeners %v",
				gateway.Namespace, gateway.Name, p.claimedBy, number, p.listeners)
			continue
		}

		target := httpPort
		if p.protocol == v1.HTTPSProtocolType {
			target = httpsPort
		}
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       fmt.Sprintf("%s-%d", strings.ToLower(string(p.protocol)), number),
			Port:       number,
			TargetPort: intstr.FromInt(target),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return servicePorts
}
