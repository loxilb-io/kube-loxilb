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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigs_v1_informer "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

const (
	httpRouteMgrName              = "HTTPRouteManager"
	httpRouteRuleServiceCreate    = "create"
	httpRouteRuleServiceDuplicate = "duplicate"
	httpRouteRuleServiceUpdate    = "update"

	httpPort  = 80
	httpsPort = 443
)

type HTTPRouteManager struct {
	kubeClient            clientset.Interface
	sigsClient            sigsclientset.Interface
	networkConfig         *config.NetworkConfig
	gatewayProvider       string
	httpRouteInformer     sigs_v1_informer.HTTPRouteInformer
	httpRouteLister       sigs_v1_lister.HTTPRouteLister
	httpRouteListerSynced cache.InformerSynced
	gatewayLister         sigs_v1_lister.GatewayLister
	gatewayListerSynced   cache.InformerSynced
	// The class says whether a Gateway is this controller's to serve.
	gatewayClassLister       sigs_v1_lister.GatewayClassLister
	gatewayClassListerSynced cache.InformerSynced
	// For a listener that takes routes from the namespaces a selector picks.
	namespaceLister       corev1lister.NamespaceLister
	namespaceListerSynced cache.InformerSynced
	// What a backendRef into another namespace needs.
	referenceGrants *ReferenceGrants
	// Whether a backendRef to a Service resolves.
	serviceLister       corev1lister.ServiceLister
	serviceListerSynced cache.InformerSynced
	// Whether InferencePool backendRefs are served at all.
	servesInferencePools bool
	// directPeers - set when routes may be served directly on a Gateway's
	// listeners (direct.go). A listener the ingress service does not carry
	// then gets no Ingress.
	directPeers DirectPeers

	queue workqueue.RateLimitingInterface
}

type HTTPRouteQueueEntry struct {
	namespace string
	name      string
}

func NewHTTPRouteManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	kubeInformerFactory informers.SharedInformerFactory,
	sigsInformerFactory externalversions.SharedInformerFactory,
	referenceGrants *ReferenceGrants) *HTTPRouteManager {

	httpRouteInformer := sigsInformerFactory.Gateway().V1().HTTPRoutes()
	gatewayInformer := sigsInformerFactory.Gateway().V1().Gateways()
	gatewayClassInformer := sigsInformerFactory.Gateway().V1().GatewayClasses()
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()
	serviceInformer := kubeInformerFactory.Core().V1().Services()
	manager := &HTTPRouteManager{
		kubeClient:            kubeClient,
		sigsClient:            sigsClient,
		networkConfig:         networkConfig,
		gatewayProvider:       networkConfig.LoxilbGatewayClass,
		httpRouteInformer:     httpRouteInformer,
		httpRouteLister:       httpRouteInformer.Lister(),
		httpRouteListerSynced: httpRouteInformer.Informer().HasSynced,
		gatewayLister:         gatewayInformer.Lister(),
		gatewayListerSynced:   gatewayInformer.Informer().HasSynced,

		gatewayClassLister:       gatewayClassInformer.Lister(),
		gatewayClassListerSynced: gatewayClassInformer.Informer().HasSynced,
		namespaceLister:          namespaceInformer.Lister(),
		namespaceListerSynced:    namespaceInformer.Informer().HasSynced,
		referenceGrants:          referenceGrants,
		serviceLister:            serviceInformer.Lister(),
		serviceListerSynced:      serviceInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "httpRoute"),
	}

	manager.httpRouteInformer.Informer().AddEventHandler(
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

	// What a route attaches to is read from its Gateways, so a change to one
	// has to reach the routes that name it.
	gatewayInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) {
				manager.enqueueGatewayRoutes(cur)
			},
			UpdateFunc: manager.gatewayUpdated,
			DeleteFunc: func(old interface{}) {
				manager.enqueueGatewayRoutes(old)
			},
		},
	)

	// A Gateway is served only once its class names this controller, and
	// the class may come after it. Its controllerName cannot change.
	gatewayClassInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) {
				manager.enqueueClassRoutes(cur)
			},
			DeleteFunc: func(old interface{}) {
				manager.enqueueClassRoutes(old)
			},
		},
	)

	// A grant that comes or goes lets in or withdraws the backends its
	// routes have in its namespace.
	referenceGrants.AddHandler(manager.referenceGrantChanged)

	// A backend Service that comes or goes resolves or unresolves the routes
	// that name it. One that changes leaves that as it was.
	serviceInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) {
				manager.enqueueServiceRoutes(cur)
				manager.ingressServiceChanged(cur)
			},
			UpdateFunc: func(old, cur interface{}) {
				manager.ingressServiceChanged(cur)
			},
			DeleteFunc: func(old interface{}) {
				manager.enqueueServiceRoutes(old)
				manager.ingressServiceChanged(old)
			},
		},
	)

	// A listener can pick the namespaces it takes routes from by label.
	namespaceInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: manager.namespaceUpdated,
		},
	)

	return manager
}

// ServeRoutesDirectly - with the Gateway manager serving routes directly
// where peers allows it, leave the listeners its ingress service does not
// carry without an Ingress, and report what a direct rule cannot hold.
func (h *HTTPRouteManager) ServeRoutesDirectly(peers DirectPeers) {
	h.directPeers = peers
}

// EnqueueAll - queue every HTTPRoute; for a change that may move any of
// them, such as a peer reporting what it is.
func (h *HTTPRouteManager) EnqueueAll() {
	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("unable to list HTTPRoutes: %v", err)
		return
	}
	for _, route := range routes {
		h.queue.Add(HTTPRouteQueueEntry{route.Namespace, route.Name})
	}
}

// ingressServiceChanged - queue the routes of the Gateway whose ingress
// service it is. Which ports it carries decides which listeners need an
// Ingress.
func (h *HTTPRouteManager) ingressServiceChanged(obj interface{}) {
	if h.directPeers == nil {
		return
	}
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	svc, ok := obj.(*corev1.Service)
	if !ok || svc.Annotations["gateway-api-controller"] != h.gatewayProvider {
		return
	}
	gateway := svc.Annotations["parent-gateway"]
	if gateway == "" || svc.Name != gateway+"-ingress-service" {
		return
	}
	if gw, err := h.gatewayLister.Gateways(svc.Namespace).Get(gateway); err == nil {
		h.enqueueGatewayRoutes(gw)
	}
}

// ingressPortsOf - the ports gw's ingress service carries; nil when routes
// are not served directly, and every listener goes through it.
func (h *HTTPRouteManager) ingressPortsOf(gw *v1.Gateway) map[v1.PortNumber]bool {
	if h.directPeers == nil {
		return nil
	}
	ports := map[v1.PortNumber]bool{}
	if h.serviceLister == nil {
		return ports
	}
	svc, err := h.serviceLister.Services(gw.Namespace).Get(gw.Name + "-ingress-service")
	if err != nil {
		return ports
	}
	for _, p := range svc.Spec.Ports {
		ports[v1.PortNumber(p.Port)] = true
	}
	return ports
}

// directOutcome - what the direct rules of a port came to for one route:
// whether any rule of it is served, and why parts of it are not.
func (h *HTTPRouteManager) directOutcome(gw *v1.Gateway, port v1.PortNumber, route types.NamespacedName) (bool, []string) {
	direct, _, held := directPortsOf(gw, h.claimedListenersOf(gw), h.directPeers)
	// A held port stays with the direct routes if they serve it already
	// (direct_handover.go): the ingress service does not carry it.
	if !direct[port] && !(held[port] && !h.ingressPortsOf(gw)[port]) {
		return false, nil
	}
	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		return false, nil
	}
	rules, refusals := directRules(gw, port, directInputs{
		routes: routes, nsLabels: h.namespaceLabels(), grants: h.referenceGrants, services: h.serviceLister,
	})
	served := false
	for _, r := range rules {
		if r.route == route {
			served = true
			break
		}
	}
	return served, refusals[route]
}

func (h *HTTPRouteManager) claimedListenersOf(gw *v1.Gateway) map[v1.SectionName]bool {
	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		return map[v1.SectionName]bool{}
	}
	return claimedListeners(gw, routes)
}

// SetServesInferencePools - say whether the InferencePool backendRefs of a
// route are served, by the inference extension's own manager. A route that
// names one when they are not does not resolve its references.
func (h *HTTPRouteManager) SetServesInferencePools(served bool) {
	h.servesInferencePools = served
}

func (h *HTTPRouteManager) enqueueObject(obj interface{}) {
	httpRoute, ok := obj.(*v1.HTTPRoute)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		// A delete the watch missed arrives as a tombstone. The HTTPRoute
		// is the object it wraps, not the tombstone itself.
		httpRoute, ok = deletedState.Obj.(*v1.HTTPRoute)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-HTTPRoute object: %v", deletedState.Obj)
			return
		}
	}

	h.queue.Add(HTTPRouteQueueEntry{httpRoute.Namespace, httpRoute.Name})
}

// gatewayUpdated - queue the routes of a Gateway whose spec changed. A status
// write leaves the generation alone and changes nothing a route reads; the
// Gateway manager writes one each time it handles a route's Gateway.
func (h *HTTPRouteManager) gatewayUpdated(old, cur interface{}) {
	oldGw, okOld := old.(*v1.Gateway)
	curGw, okCur := cur.(*v1.Gateway)
	if okOld && okCur && oldGw.Generation == curGw.Generation {
		return
	}
	h.enqueueGatewayRoutes(cur)
}

// enqueueGatewayRoutes - queue the HTTPRoutes that name a Gateway as a parent.
func (h *HTTPRouteManager) enqueueGatewayRoutes(obj interface{}) {
	gw, ok := obj.(*v1.Gateway)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		gw, ok = deletedState.Obj.(*v1.Gateway)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Gateway object: %v", deletedState.Obj)
			return
		}
	}

	routes, err := h.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("gateway %s/%s: unable to list HTTPRoutes: %v", gw.Namespace, gw.Name, err)
		return
	}
	for _, route := range routes {
		for _, ref := range route.Spec.ParentRefs {
			namespace, name, ok := gatewayOf(route.Namespace, ref)
			if ok && namespace == gw.Namespace && name == gw.Name {
				h.queue.Add(HTTPRouteQueueEntry{route.Namespace, route.Name})
				break
			}
		}
	}
}

// enqueueClassRoutes - queue the routes of every Gateway of a GatewayClass.
func (h *HTTPRouteManager) enqueueClassRoutes(obj interface{}) {
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

	gateways, err := h.gatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("gatewayClass %s: unable to list gateways: %v", gc.Name, err)
		return
	}
	for _, gw := range gateways {
		if string(gw.Spec.GatewayClassName) == gc.Name {
			h.enqueueGatewayRoutes(gw)
		}
	}
}

// referenceGrantChanged - queue the HTTPRoutes of the namespaces a grant lets
// refer into its own.
func (h *HTTPRouteManager) referenceGrantChanged(spec v1.ReferenceGrantSpec) {
	for _, from := range spec.From {
		if from.Group != v1.GroupName || from.Kind != kindHTTPRoute {
			continue
		}
		routes, err := h.httpRouteLister.HTTPRoutes(string(from.Namespace)).List(labels.Everything())
		if err != nil {
			klog.Errorf("namespace %s: unable to list HTTPRoutes: %v", from.Namespace, err)
			continue
		}
		for _, route := range routes {
			h.queue.Add(HTTPRouteQueueEntry{route.Namespace, route.Name})
		}
	}
}

// namespaceUpdated - queue the routes of a namespace whose labels changed,
// which a listener's namespace selector reads.
func (h *HTTPRouteManager) namespaceUpdated(old, cur interface{}) {
	oldNs, okOld := old.(*corev1.Namespace)
	curNs, okCur := cur.(*corev1.Namespace)
	if !okOld || !okCur || maps.Equal(oldNs.Labels, curNs.Labels) {
		return
	}

	routes, err := h.httpRouteLister.HTTPRoutes(curNs.Name).List(labels.Everything())
	if err != nil {
		klog.Errorf("namespace %s: unable to list HTTPRoutes: %v", curNs.Name, err)
		return
	}
	for _, route := range routes {
		h.queue.Add(HTTPRouteQueueEntry{route.Namespace, route.Name})
	}
}

func (h *HTTPRouteManager) Run(stopCh <-chan struct{}) {
	defer h.queue.ShutDown()

	klog.Infof("Starting %s", httpRouteMgrName)
	defer klog.Infof("Shutting down %s", httpRouteMgrName)

	if !cache.WaitForNamedCacheSync(
		httpRouteMgrName,
		stopCh,
		h.httpRouteListerSynced,
		h.gatewayListerSynced,
		h.gatewayClassListerSynced,
		h.namespaceListerSynced,
		h.serviceListerSynced) {
		return
	}
	if !h.referenceGrants.WaitForSync(stopCh) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(h.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (h *HTTPRouteManager) worker() {
	for h.processNextWorkItem() {
	}
}

func (h *HTTPRouteManager) processNextWorkItem() bool {
	obj, quit := h.queue.Get()
	if quit {
		return false
	}
	defer h.queue.Done(obj)

	if key, ok := obj.(HTTPRouteQueueEntry); !ok {
		h.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := h.reconcile(key.namespace, key.name); err == nil {
		h.queue.Forget(obj)
	} else {
		h.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing httpRoute %s/%s, requeuing. Error: %v", key.namespace, key.name, err)
	}
	return true
}

func (h *HTTPRouteManager) reconcile(namespace, name string) error {
	httpRoute, err := h.httpRouteLister.HTTPRoutes(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return h.deleteHTTPRoute(namespace, name)
		}
		return err
	}

	return h.syncHTTPRoute(httpRoute)
}

// syncHTTPRoute - make the route's Ingresses what its attachments call for:
// one for each listener it attaches to, and none for a listener it left.
func (h *HTTPRouteManager) syncHTTPRoute(httpRoute *v1.HTTPRoute) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// Said here, once per route, rather than where the references are
	// resolved: that runs from event handlers, once per pool and listener.
	if refs := crossNamespacePoolRefs(httpRoute); len(refs) > 0 {
		klog.Warningf("HTTPRoute %s/%s: InferencePool backendRefs into another namespace are not served (%s); "+
			"the route and the pool have to be in the same namespace",
			httpRoute.Namespace, httpRoute.Name, strings.Join(refs, ", "))
	}

	desired, parents, err := h.routeIngresses(httpRoute)
	if err != nil {
		return err
	}

	existing, err := h.ownedIngresses(ctx, httpRoute.Namespace, httpRoute.Name)
	if err != nil {
		return err
	}

	errs := h.applyIngresses(ctx, httpRoute, desired, existing)

	// The status says what the route attaches to, which does not wait for
	// its Ingresses; a failure to write them is the route's to retry.
	if err := h.updateRouteStatus(ctx, httpRoute, h.routeParentStatuses(httpRoute, parents)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	klog.Infof("HTTPRoute %s/%s is reconciled with %d ingresses.", httpRoute.Namespace, httpRoute.Name, len(desired))
	return nil
}

// applyIngresses - create and update the desired Ingresses, then delete the
// route's others.
func (h *HTTPRouteManager) applyIngresses(ctx context.Context, httpRoute *v1.HTTPRoute, desired []*netv1.Ingress, existing map[string]*netv1.Ingress) []error {
	var errs []error
	keep := map[string]bool{}
	for _, ing := range desired {
		keep[ing.Name] = true
		if err := h.applyIngress(ctx, ing, existing[ing.Name]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		// The Ingresses left over stay until the rest are in place. One of
		// them may still be serving a listener whose new Ingress failed.
		return errs
	}

	for name := range existing {
		if keep[name] {
			continue
		}
		if err := h.kubeClient.NetworkingV1().Ingresses(httpRoute.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("HTTPRoute %s/%s: delete ingress %s: %w", httpRoute.Namespace, httpRoute.Name, name, err))
			continue
		}
		klog.Infof("ingress %s/%s is deleted by HTTPRoute %s - no listener it attaches to needs it", httpRoute.Namespace, name, httpRoute.Name)
	}
	return errs
}

func (h *HTTPRouteManager) deleteHTTPRoute(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	if err := h.deleteRouteIngresses(ctx, namespace, name); err != nil {
		return err
	}

	klog.Infof("HTTPRoute %s is deleted.", name)
	return nil
}

// deleteRouteIngresses - remove the Ingresses this manager made for the route.
func (h *HTTPRouteManager) deleteRouteIngresses(ctx context.Context, namespace, name string) error {
	owned, err := h.ownedIngresses(ctx, namespace, name)
	if err != nil {
		return err
	}

	for ingName := range owned {
		err = h.kubeClient.NetworkingV1().Ingresses(namespace).Delete(ctx, ingName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.Errorf("HTTPRoute: delete ingress %s/%s failure. err: %v", namespace, ingName, err)
			return err
		}
		klog.Infof("ingress %s/%s is deleted by HTTPRoute %s", namespace, ingName, name)
	}

	return nil
}

// ownedIngresses - the Ingresses this manager made for the route, by name.
func (h *HTTPRouteManager) ownedIngresses(ctx context.Context, namespace, name string) (map[string]*netv1.Ingress, error) {
	ingList, err := h.kubeClient.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("HTTPRoute: get ingresses list (ns: %s) failure. err: %v", namespace, err)
		return nil, err
	}

	owned := map[string]*netv1.Ingress{}
	for i := range ingList.Items {
		ingress := &ingList.Items[i]
		if ingress.Annotations["gateway-api-controller"] == h.gatewayProvider &&
			ingress.Annotations["parent-http-route"] == name {
			owned[ingress.Name] = ingress
		}
	}
	return owned, nil
}

// findGateway - the Gateway a parentRef names, or nil when it names none that
// exists yet. A Gateway applied after its routes queues them when it comes,
// so the route has nothing to retry until then.
func (h *HTTPRouteManager) findGateway(httpRoute *v1.HTTPRoute, parentRef v1.ParentReference) (*v1.Gateway, error) {
	gatewayNamespace, gatewayName, ok := gatewayOf(httpRoute.Namespace, parentRef)
	if !ok {
		klog.V(4).Infof("HTTPRoute %s/%s: parentRef %s is not a Gateway", httpRoute.Namespace, httpRoute.Name, parentRef.Name)
		return nil, nil
	}

	gateway, err := h.gatewayLister.Gateways(gatewayNamespace).Get(gatewayName)
	if errors.IsNotFound(err) {
		klog.Infof("HTTPRoute %s/%s: gateway %s/%s does not exist yet", httpRoute.Namespace, httpRoute.Name, gatewayNamespace, gatewayName)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("HTTPRoute %s/%s: get gateway %s/%s: %w", httpRoute.Namespace, httpRoute.Name, gatewayNamespace, gatewayName, err)
	}

	return gateway, nil
}

// servesGateway - whether the Gateway's class names this controller. A route
// on another controller's Gateway is that controller's to serve.
func (h *HTTPRouteManager) servesGateway(gw *v1.Gateway) bool {
	gc, err := h.gatewayClassLister.Get(string(gw.Spec.GatewayClassName))
	if err != nil {
		// Its class may not exist yet; it queues the routes when it comes.
		klog.V(4).Infof("gateway %s/%s: gatewayClass %s: %v", gw.Namespace, gw.Name, gw.Spec.GatewayClassName, err)
		return false
	}
	return gc.Spec.ControllerName == v1.GatewayController(h.gatewayProvider)
}

// namespaceLabels - the labels of a namespace, read from the cache.
func (h *HTTPRouteManager) namespaceLabels() namespaceLabels {
	if h.namespaceLister == nil {
		return nil
	}
	return func(name string) (labels.Set, error) {
		ns, err := h.namespaceLister.Get(name)
		if err != nil {
			return nil, err
		}
		return labels.Set(ns.Labels), nil
	}
}

// parentAttachment - what the route came to through one parentRef that names
// a Gateway of this controller's.
type parentAttachment struct {
	Ref        v1.ParentReference
	Attachment routeAttachment
	// Unserved - why a listener the route attaches to cannot serve it, one
	// for each such listener.
	Unserved []string
	// Notes - what listeners that serve the route leave out of it.
	Notes []string
}

// routeIngresses - the Ingresses the route should have, one for each listener
// it attaches to that an Ingress can serve it on, and what each of its
// parentRefs to a Gateway of this controller's came to.
func (h *HTTPRouteManager) routeIngresses(httpRoute *v1.HTTPRoute) ([]*netv1.Ingress, []parentAttachment, error) {
	route := attachingRoute{Kind: kindHTTPRoute, Namespace: httpRoute.Namespace, Hostnames: httpRoute.Spec.Hostnames}
	nsLabels := h.namespaceLabels()

	var ingresses []*netv1.Ingress
	var parents []parentAttachment
	seen := map[string]bool{}
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		gateway, err := h.findGateway(httpRoute, parentRef)
		if err != nil {
			return nil, nil, err
		}
		if gateway == nil || !h.servesGateway(gateway) {
			continue
		}

		parent := parentAttachment{Ref: parentRef, Attachment: attachRoute(gateway, parentRef, route, nsLabels)}
		if !parent.Attachment.Accepted() {
			klog.Infof("HTTPRoute %s/%s does not attach to gateway %s/%s (%s): %s",
				httpRoute.Namespace, httpRoute.Name, gateway.Namespace, gateway.Name,
				parent.Attachment.Reason, parent.Attachment.Message)
		}

		ingressPorts := h.ingressPortsOf(gateway)
		for _, la := range parent.Attachment.Listeners {
			if ingressPorts != nil && !ingressPorts[la.Listener.Port] {
				// Not on the ingress service: served by direct rules, or
				// not at all. What the direct rules leave out of the route
				// is said in its status.
				served, why := h.directOutcome(gateway, la.Listener.Port, types.NamespacedName{Namespace: httpRoute.Namespace, Name: httpRoute.Name})
				switch {
				case len(why) == 0:
				case served:
					parent.Notes = append(parent.Notes, fmt.Sprintf("listener %s: %s", la.Listener.Name, strings.Join(why, "; ")))
				default:
					parent.Unserved = append(parent.Unserved, fmt.Sprintf("listener %s does not serve the route: %s", la.Listener.Name, strings.Join(why, "; ")))
				}
				continue
			}
			ing, unserved := h.listenerIngress(httpRoute, gateway, la)
			if unserved != "" {
				klog.Warningf("HTTPRoute %s/%s: gateway %s/%s %s",
					httpRoute.Namespace, httpRoute.Name, gateway.Namespace, gateway.Name, unserved)
				parent.Unserved = append(parent.Unserved, unserved)
			}
			// Two parentRefs can select the same listener.
			if ing == nil || seen[ing.Name] {
				continue
			}
			seen[ing.Name] = true
			ingresses = append(ingresses, ing)
		}
		parents = append(parents, parent)
	}
	return ingresses, parents, nil
}

// listenerIngressName - the name of the Ingress a route has for one listener.
// The Gateway and listener go in as a hash, which keeps the name inside the
// API's limit and apart from those of the route's other listeners.
func listenerIngressName(route, gwNamespace, gwName string, listener v1.SectionName) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{route, gwNamespace, gwName, string(listener)}, "/")))
	suffix := hex.EncodeToString(sum[:4])

	base := route
	if max := validation.DNS1123SubdomainMaxLength - len(suffix) - 1; len(base) > max {
		base = strings.TrimRight(base[:max], "-.")
	}
	return base + "-" + suffix
}

// listenerCertificateSecrets - the names of the Secrets a listener terminates
// TLS with. A certificateRef of another kind is skipped.
func listenerCertificateSecrets(l *v1.Listener) []string {
	if l.TLS == nil {
		return nil
	}

	var names []string
	for _, ref := range l.TLS.CertificateRefs {
		if ref.Group != nil && *ref.Group != "" {
			continue
		}
		if ref.Kind != nil && *ref.Kind != "Secret" {
			continue
		}
		names = append(names, string(ref.Name))
	}
	return names
}

// listenerIngress - the Ingress that serves a route on one listener it
// attaches to, or nil when there is none to make. unserved says why, when
// the listener cannot serve the route at all.
//
// loxilb-ingress serves every loxilb Ingress on its HTTP port, or on its HTTPS
// port for a host the Ingress has TLS for, whichever listener it came from.
// So an HTTPS listener is served only for hosts with a certificate: one that
// has no hostname, or no Secret, gets no Ingress rather than one that would
// answer in plain HTTP.
func (h *HTTPRouteManager) listenerIngress(httpRoute *v1.HTTPRoute, gateway *v1.Gateway, la listenerAttachment) (ing *netv1.Ingress, unserved string) {
	listener := la.Listener
	hosts, ok := servedHostnames(gateway, listener, la.Hostnames)
	if !ok {
		klog.V(2).Infof("HTTPRoute %s/%s: every hostname it has on listener %s of gateway %s/%s goes to a more specific listener",
			httpRoute.Namespace, httpRoute.Name, listener.Name, gateway.Namespace, gateway.Name)
		return nil, ""
	}

	// An IngressRule without a host serves any.
	hostnames := []string{""}
	if hosts != nil {
		hostnames = hostnames[:0]
		for _, host := range hosts {
			hostnames = append(hostnames, string(host))
		}
	}

	var tls []netv1.IngressTLS
	if listener.Protocol == v1.HTTPSProtocolType {
		if hosts == nil {
			return nil, fmt.Sprintf("listener %s does not serve the route: neither it nor the route names a hostname, "+
				"and loxilb-ingress terminates TLS only for a named host", listener.Name)
		}
		secrets := listenerCertificateSecrets(listener)
		if len(secrets) == 0 {
			return nil, fmt.Sprintf("listener %s does not serve the route: it has no Secret to terminate TLS with", listener.Name)
		}
		for _, secret := range secrets {
			tls = append(tls, netv1.IngressTLS{SecretName: secret, Hosts: hostnames})
		}
	}

	newIngress := &netv1.Ingress{}
	newIngress.Name = listenerIngressName(httpRoute.Name, gateway.Namespace, gateway.Name, listener.Name)
	newIngress.Namespace = httpRoute.Namespace

	// A copy: the route is the informer's, and the annotations below are not
	// its to carry.
	newIngress.Annotations = maps.Clone(httpRoute.Annotations)
	if newIngress.Annotations == nil {
		newIngress.Annotations = map[string]string{}
	}
	newIngress.Annotations["gateway-api-controller"] = h.gatewayProvider
	newIngress.Annotations["parent-gateway"] = gateway.Name
	newIngress.Annotations["parent-gateway-namespace"] = gateway.Namespace
	newIngress.Annotations["parent-listener"] = string(listener.Name)
	newIngress.Annotations["parent-http-route"] = httpRoute.Name

	newIngress.SetLabels(map[string]string{
		"implementation": implementation,
	})
	ingressClass := "loxilb"
	newIngress.Spec.IngressClassName = &ingressClass
	newIngress.Spec.TLS = tls

	anyPath := false
	for _, rule := range httpRoute.Spec.Rules {
		paths := h.rulePaths(httpRoute, rule, newIngress)

		// A rule whose backends are all served elsewhere adds nothing; an
		// IngressRule without paths would make the whole Ingress invalid.
		if len(paths) == 0 {
			continue
		}
		anyPath = true

		for _, hostname := range hostnames {
			newRule := netv1.IngressRule{
				Host: hostname,
				IngressRuleValue: netv1.IngressRuleValue{
					HTTP: &netv1.HTTPIngressRuleValue{
						Paths: paths,
					},
				},
			}
			newIngress.Spec.Rules = append(newIngress.Spec.Rules, newRule)
		}
	}

	if !anyPath {
		// Every backend is served some other way - an InferencePool becomes a
		// Service of its own manager's - and an Ingress with no rules is
		// refused by the API server anyway.
		klog.V(4).Infof("HTTPRoute %s/%s: no backendRef needs an Ingress", httpRoute.Namespace, httpRoute.Name)
		return nil, ""
	}

	return newIngress, ""
}

// rulePaths - the Ingress paths of one route rule, one for each match and
// backend an Ingress can carry. A backend in another namespace is left out
// unless a ReferenceGrant there permits it, and noted on ing, where
// loxilb-ingress looks for it, when one does.
func (h *HTTPRouteManager) rulePaths(httpRoute *v1.HTTPRoute, rule v1.HTTPRouteRule, ing *netv1.Ingress) []netv1.HTTPIngressPath {
	var paths []netv1.HTTPIngressPath
	for _, match := range rule.Matches {
		for _, backref := range rule.BackendRefs {
			// An Ingress backend is always a Service. A backendRef that
			// names something else has to be skipped rather than read as
			// one, or the Ingress ends up pointing at a Service that was
			// never created and the route silently forwards nowhere.
			switch classifyBackendRef(backref.BackendObjectReference) {
			case backendService:
			case backendInferencePool:
				// Handled by the InferencePool controller, which programs
				// the loxilb rule directly instead of going through an
				// Ingress.
				klog.V(4).Infof("HTTPRoute %s/%s: backendRef %s is an InferencePool, not an Ingress backend",
					httpRoute.Namespace, httpRoute.Name, backendRefDescription(backref.BackendObjectReference))
				continue
			default:
				klog.Warningf("HTTPRoute %s/%s: backendRef %s is not supported - skipping",
					httpRoute.Namespace, httpRoute.Name, backendRefDescription(backref.BackendObjectReference))
				continue
			}

			if backref.Namespace != nil && string(*backref.Namespace) != httpRoute.Namespace {
				if !h.referenceGrants.Permits(
					grantReference{Group: v1.GroupName, Kind: kindHTTPRoute, Namespace: httpRoute.Namespace},
					grantReference{Group: "", Kind: "Service", Namespace: string(*backref.Namespace), Name: string(backref.Name)},
				) {
					klog.Warningf("HTTPRoute %s/%s: no ReferenceGrant in namespace %s permits backendRef %s - skipping",
						httpRoute.Namespace, httpRoute.Name, *backref.Namespace, backendRefDescription(backref.BackendObjectReference))
					continue
				}
				ing.Annotations["external-backend-service"] = "true"
				ing.Annotations["service-"+string(backref.Name)+"-namespace"] = string(*backref.Namespace)
			}

			newIngressPath := netv1.HTTPIngressPath{
				Backend: netv1.IngressBackend{
					Service: &netv1.IngressServiceBackend{
						Name: string(backref.Name),
						Port: netv1.ServiceBackendPort{},
					},
				},
			}

			if match.Path != nil {
				if match.Path.Value != nil {
					newIngressPath.Path = *match.Path.Value
				}
				if match.Path.Type != nil {
					switch *match.Path.Type {
					case v1.PathMatchExact:
						exactType := netv1.PathTypeExact
						newIngressPath.PathType = &exactType
					case v1.PathMatchPathPrefix:
						fallthrough
					default:
						prefixType := netv1.PathTypePrefix
						newIngressPath.PathType = &prefixType
					}
				}
			}
			if backref.Port != nil {
				newIngressPath.Backend.Service.Port.Number = int32(*backref.Port)
			}
			// TODO: now httpRoute don't support Path Type RegularExpression

			paths = append(paths, newIngressPath)
		}
	}
	return paths
}

// applyIngress - create the Ingress, or bring the route's existing one of that
// name to it.
func (h *HTTPRouteManager) applyIngress(ctx context.Context, desired, existing *netv1.Ingress) error {
	ingresses := h.kubeClient.NetworkingV1().Ingresses(desired.Namespace)
	route := desired.Annotations["parent-http-route"]

	if existing == nil {
		_, err := ingresses.Create(ctx, desired, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			// Not in the route's own, so made by someone else; it is not
			// this manager's to overwrite.
			return fmt.Errorf("HTTPRoute %s/%s: ingress %s exists and was not made for the route: %w",
				desired.Namespace, route, desired.Name, err)
		}
		if err != nil {
			return fmt.Errorf("HTTPRoute %s/%s: create ingress %s: %w", desired.Namespace, route, desired.Name, err)
		}
		klog.Infof("ingress %s/%s is created for listener %s of gateway %s/%s", desired.Namespace, desired.Name,
			desired.Annotations["parent-listener"], desired.Annotations["parent-gateway-namespace"], desired.Annotations["parent-gateway"])
		return nil
	}

	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		maps.Equal(existing.Labels, desired.Labels) &&
		maps.Equal(existing.Annotations, desired.Annotations) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.Spec = desired.Spec
	updated.Labels = desired.Labels
	updated.Annotations = desired.Annotations
	if _, err := ingresses.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("HTTPRoute %s/%s: update ingress %s: %w", desired.Namespace, route, desired.Name, err)
	}
	klog.Infof("ingress %s/%s is updated", desired.Namespace, desired.Name)
	return nil
}
