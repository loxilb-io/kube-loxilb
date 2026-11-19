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
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

// L4 routes: TCPRoute and UDPRoute, which one manager serves alike.
//
// Such a route is served by a LoadBalancer Service on its Gateway's address
// and its listeners' ports. Which Service, the route's serviceBehaviour label
// says:
//
//   - create, the default: one named for the backendRef, in its namespace,
//     that this controller makes; the pods it selects are the ones the
//     route's selectorkey and selectorvalue labels name.
//   - duplicate: a copy of the backendRef's Service, named <name>-gw-api.
//   - update: the backendRef's Service itself, made into one.
const (
	l4BehaviourLabel     = "serviceBehaviour"
	l4BehaviourCreate    = "create"
	l4BehaviourDuplicate = "duplicate"
	l4BehaviourUpdate    = "update"

	// l4BehaviourAnnotation - on a Service made or taken for a route, which
	// of the three it was. A Service from an earlier build has none.
	l4BehaviourAnnotation = "route-service-behaviour"
	// l4OriginalAnnotation - on a Service an update took, what the update
	// changed, so that it can be put back when the route lets it go.
	l4OriginalAnnotation = "route-service-original"
)

// l4RouteKey - a route in the work queue.
type l4RouteKey struct {
	namespace string
	name      string
}

// l4Route - what the manager reads of a TCPRoute or UDPRoute.
type l4Route struct {
	Namespace   string
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	ParentRefs  []v1.ParentReference
	// BackendRefs - those of every rule.
	BackendRefs []v1.BackendRef
}

// l4RouteKind - what sets TCPRoutes and UDPRoutes apart.
type l4RouteKind struct {
	kind     v1.Kind
	protocol corev1.Protocol
	// parentAnnotation - the annotation that names the route on its Service;
	// with "-namespace" after it, the one that names its namespace.
	parentAnnotation string

	get  func(namespace, name string) (*l4Route, error)
	list func(namespace string) ([]*l4Route, error)
	// isRoute - whether an object from the informer is a route of the kind.
	isRoute func(obj interface{}) bool
}

type l4RouteManager struct {
	l4RouteKind
	name string

	kubeClient      clientset.Interface
	sigsClient      sigsclientset.Interface
	gatewayProvider string

	gatewayLister      sigs_v1_lister.GatewayLister
	gatewayClassLister sigs_v1_lister.GatewayClassLister
	namespaceLister    corev1lister.NamespaceLister
	serviceLister      corev1lister.ServiceLister
	referenceGrants    *ReferenceGrants
	synced             []cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

func newL4RouteManager(
	name, queueName string,
	kind l4RouteKind,
	routeInformer cache.SharedIndexInformer,
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	kubeInformerFactory informers.SharedInformerFactory,
	sigsInformerFactory externalversions.SharedInformerFactory,
	referenceGrants *ReferenceGrants) *l4RouteManager {

	gatewayInformer := sigsInformerFactory.Gateway().V1().Gateways()
	gatewayClassInformer := sigsInformerFactory.Gateway().V1().GatewayClasses()
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()
	serviceInformer := kubeInformerFactory.Core().V1().Services()

	m := &l4RouteManager{
		l4RouteKind:        kind,
		name:               name,
		kubeClient:         kubeClient,
		sigsClient:         sigsClient,
		gatewayProvider:    networkConfig.LoxilbGatewayClass,
		gatewayLister:      gatewayInformer.Lister(),
		gatewayClassLister: gatewayClassInformer.Lister(),
		namespaceLister:    namespaceInformer.Lister(),
		serviceLister:      serviceInformer.Lister(),
		referenceGrants:    referenceGrants,
		synced: []cache.InformerSynced{
			routeInformer.HasSynced,
			gatewayInformer.Informer().HasSynced,
			gatewayClassInformer.Informer().HasSynced,
			namespaceInformer.Informer().HasSynced,
			serviceInformer.Informer().HasSynced,
		},
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), queueName),
	}

	routeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.enqueueObject,
		UpdateFunc: func(old, cur interface{}) { m.enqueueObject(cur) },
		DeleteFunc: m.enqueueObject,
	})

	// What a route attaches to, and the address it is served on, are read
	// from its Gateways. A status write changes neither.
	gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: m.enqueueGatewayRoutes,
		UpdateFunc: func(old, cur interface{}) {
			oldGw, okOld := old.(*v1.Gateway)
			curGw, okCur := cur.(*v1.Gateway)
			if okOld && okCur && oldGw.Generation == curGw.Generation {
				return
			}
			m.enqueueGatewayRoutes(cur)
		},
		DeleteFunc: m.enqueueGatewayRoutes,
	})

	// A Gateway is served once its class names this controller.
	gatewayClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.enqueueClassRoutes,
		DeleteFunc: m.enqueueClassRoutes,
	})

	// A listener can pick the namespaces it takes routes from by label.
	namespaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			oldNs, okOld := old.(*corev1.Namespace)
			curNs, okCur := cur.(*corev1.Namespace)
			if okOld && okCur && !maps.Equal(oldNs.Labels, curNs.Labels) {
				m.enqueueNamespaceRoutes(curNs.Name)
			}
		},
	})

	// A grant lets in or withdraws the backends its routes have in its
	// namespace.
	referenceGrants.AddHandler(func(spec v1.ReferenceGrantSpec) {
		for _, from := range spec.From {
			if from.Group == v1.GroupName && from.Kind == m.kind {
				m.enqueueNamespaceRoutes(string(from.Namespace))
			}
		}
	})

	return m
}

func (m *l4RouteManager) enqueueObject(obj interface{}) {
	// A delete the watch missed arrives as a tombstone; the route is the
	// object it wraps.
	route := obj
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		route = deletedState.Obj
	}
	if m.isRoute == nil || !m.isRoute(route) {
		klog.Errorf("%s: received unexpected object: %v", m.name, obj)
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(route)
	if err != nil {
		klog.Errorf("%s: received unexpected object %v: %v", m.name, obj, err)
		return
	}
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: unexpected key %s: %v", m.name, key, err)
		return
	}
	m.queue.Add(l4RouteKey{namespace, name})
}

func (m *l4RouteManager) enqueueNamespaceRoutes(namespace string) {
	routes, err := m.list(namespace)
	if err != nil {
		klog.Errorf("%s: unable to list routes of namespace %s: %v", m.name, namespace, err)
		return
	}
	for _, route := range routes {
		m.queue.Add(l4RouteKey{route.Namespace, route.Name})
	}
}

// enqueueGatewayRoutes - queue the routes that name a Gateway as a parent.
func (m *l4RouteManager) enqueueGatewayRoutes(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	gw, ok := obj.(*v1.Gateway)
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
		for _, ref := range route.ParentRefs {
			namespace, name, ok := gatewayOf(route.Namespace, ref)
			if ok && namespace == gw.Namespace && name == gw.Name {
				m.queue.Add(l4RouteKey{route.Namespace, route.Name})
				break
			}
		}
	}
}

// enqueueClassRoutes - queue the routes of every Gateway of a GatewayClass.
func (m *l4RouteManager) enqueueClassRoutes(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	gc, ok := obj.(*v1.GatewayClass)
	if !ok {
		klog.Errorf("%s: received unexpected object: %v", m.name, obj)
		return
	}

	gateways, err := m.gatewayLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("%s: unable to list gateways: %v", m.name, err)
		return
	}
	for _, gw := range gateways {
		if string(gw.Spec.GatewayClassName) == gc.Name {
			m.enqueueGatewayRoutes(gw)
		}
	}
}

func (m *l4RouteManager) Run(stopCh <-chan struct{}) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", m.name)
	defer klog.Infof("Shutting down %s", m.name)

	if !cache.WaitForNamedCacheSync(m.name, stopCh, m.synced...) {
		return
	}
	if !m.referenceGrants.WaitForSync(stopCh) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *l4RouteManager) worker() {
	for m.processNextWorkItem() {
	}
}

func (m *l4RouteManager) processNextWorkItem() bool {
	obj, quit := m.queue.Get()
	if quit {
		return false
	}
	defer m.queue.Done(obj)

	key, ok := obj.(l4RouteKey)
	if !ok {
		m.queue.Forget(obj)
		klog.Errorf("%s: expected a route in the work queue but got %#v", m.name, obj)
		return true
	}
	if err := m.reconcile(key); err != nil {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing %s %s/%s, requeuing. Error: %v", m.kind, key.namespace, key.name, err)
		return true
	}
	m.queue.Forget(obj)
	return true
}

func (m *l4RouteManager) reconcile(key l4RouteKey) error {
	route, err := m.get(key.namespace, key.name)
	if errors.IsNotFound(err) {
		route = nil
	} else if err != nil {
		return err
	}
	return m.sync(key, route)
}

// sync - make the Services of a route what it calls for, or, for a route
// that has gone (nil), let go of them all.
func (m *l4RouteManager) sync(key l4RouteKey, route *l4Route) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	var desired []*corev1.Service
	if route != nil {
		var ready bool
		var err error
		desired, ready, err = m.desiredServices(route)
		if err != nil {
			return err
		}
		if !ready {
			// Its Gateway has no address yet. The address comes in a spec
			// update, which queues the route; until then what it has stays.
			return nil
		}
	}

	owned, err := m.ownedServices(ctx, key)
	if err != nil {
		return err
	}

	var errs []error
	keep := map[string]bool{}
	for _, svc := range desired {
		keep[svc.Namespace+"/"+svc.Name] = true
		if err := m.applyService(ctx, key, svc); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		// What the route has stays until the rest are in place.
		return utilerrors.NewAggregate(errs)
	}

	for id, svc := range owned {
		if keep[id] {
			continue
		}
		if err := m.releaseService(ctx, key, svc); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

// servesGateway - whether the Gateway's class names this controller.
func (m *l4RouteManager) servesGateway(gw *v1.Gateway) bool {
	gc, err := m.gatewayClassLister.Get(string(gw.Spec.GatewayClassName))
	if err != nil {
		return false
	}
	return gc.Spec.ControllerName == v1.GatewayController(m.gatewayProvider)
}

func (m *l4RouteManager) namespaceLabels() namespaceLabels {
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

// servingGateway - the Gateway of this controller's the route is served on,
// and the listeners there that take it.
//
// A Service has one address, so the route is served on the first Gateway
// its parentRefs attach it to; one it also attaches to is said in the log.
func (m *l4RouteManager) servingGateway(route *l4Route) (*v1.Gateway, []*v1.Listener) {
	attaching := attachingRoute{Kind: m.kind, Namespace: route.Namespace}
	nsLabels := m.namespaceLabels()

	var gateway *v1.Gateway
	var listeners []*v1.Listener
	for _, ref := range route.ParentRefs {
		namespace, name, ok := gatewayOf(route.Namespace, ref)
		if !ok {
			continue
		}
		gw, err := m.gatewayLister.Gateways(namespace).Get(name)
		if err != nil || !m.servesGateway(gw) {
			continue
		}

		attachment := attachRoute(gw, ref, attaching, nsLabels)
		if !attachment.Accepted() {
			klog.Infof("%s %s/%s does not attach to gateway %s/%s (%s): %s", m.kind, route.Namespace, route.Name,
				gw.Namespace, gw.Name, attachment.Reason, attachment.Message)
			continue
		}
		if gateway == nil {
			gateway = gw
		}
		if gw.Namespace != gateway.Namespace || gw.Name != gateway.Name {
			klog.Warningf("%s %s/%s attaches to gateway %s/%s too, but is served on %s/%s only: its Service has one address",
				m.kind, route.Namespace, route.Name, gw.Namespace, gw.Name, gateway.Namespace, gateway.Name)
			continue
		}
		for _, la := range attachment.Listeners {
			listeners = append(listeners, la.Listener)
		}
	}
	return gateway, listeners
}

// servicePorts - one port for each listener port, to the backend's port. A
// single port keeps the empty name it always had; more than one need names.
func (m *l4RouteManager) servicePorts(listeners []*v1.Listener, backendPort int32) []corev1.ServicePort {
	var ports []corev1.ServicePort
	seen := map[int32]bool{}
	for _, l := range listeners {
		if seen[int32(l.Port)] {
			continue
		}
		seen[int32(l.Port)] = true
		ports = append(ports, corev1.ServicePort{
			Port:       int32(l.Port),
			TargetPort: intstr.FromInt(int(backendPort)),
			Protocol:   m.protocol,
		})
	}
	if len(ports) > 1 {
		for i := range ports {
			ports[i].Name = fmt.Sprintf("%s-%d", strings.ToLower(string(m.protocol)), ports[i].Port)
		}
	}
	return ports
}

// desiredServices - the Services the route should have. ready is false when
// the Gateway it is served on has no address yet.
func (m *l4RouteManager) desiredServices(route *l4Route) (desired []*corev1.Service, ready bool, err error) {
	gateway, listeners := m.servingGateway(route)
	if gateway == nil {
		return nil, true, nil
	}
	if len(gateway.Spec.Addresses) == 0 {
		return nil, false, nil
	}
	address := gateway.Spec.Addresses[0].Value

	behaviour := route.Labels[l4BehaviourLabel]
	if behaviour == "" {
		behaviour = l4BehaviourCreate
	}
	if behaviour != l4BehaviourCreate && behaviour != l4BehaviourDuplicate && behaviour != l4BehaviourUpdate {
		return nil, true, fmt.Errorf("%s %s/%s: unknown %s %q", m.kind, route.Namespace, route.Name, l4BehaviourLabel, behaviour)
	}

	seen := map[string]bool{}
	for _, ref := range route.BackendRefs {
		obj := ref.BackendObjectReference
		// The backend of an L4 route is a Service. Anything else - an
		// InferencePool included, since inference routing is HTTP - would be
		// turned into a Service reference that names an object of a
		// different kind, so refuse instead of guessing.
		if classifyBackendRef(obj) != backendService {
			klog.Warningf("%s %s/%s: backendRef %s is not a Service - skipping", m.kind, route.Namespace, route.Name, backendRefDescription(obj))
			continue
		}
		if obj.Port == nil {
			klog.Warningf("%s %s/%s: backendRef %s has no port - skipping", m.kind, route.Namespace, route.Name, obj.Name)
			continue
		}
		namespace := route.Namespace
		if obj.Namespace != nil {
			namespace = string(*obj.Namespace)
		}
		// A Service in another namespace is written to as well as read, in
		// every behaviour, so the route needs a grant there.
		if !m.referenceGrants.Permits(
			grantReference{Group: v1.GroupName, Kind: m.kind, Namespace: route.Namespace},
			grantReference{Group: "", Kind: "Service", Namespace: namespace, Name: string(obj.Name)},
		) {
			klog.Warningf("%s %s/%s: no ReferenceGrant in namespace %s permits backendRef %s - skipping",
				m.kind, route.Namespace, route.Name, namespace, obj.Name)
			continue
		}

		svc, err := m.desiredService(route, behaviour, namespace, string(obj.Name), address, m.servicePorts(listeners, int32(*obj.Port)))
		if err != nil {
			return nil, true, err
		}
		if id := svc.Namespace + "/" + svc.Name; !seen[id] {
			seen[id] = true
			desired = append(desired, svc)
		}
	}
	return desired, true, nil
}

// desiredService - the Service one backendRef calls for, as this controller
// would write it.
func (m *l4RouteManager) desiredService(route *l4Route, behaviour, namespace, name, address string, ports []corev1.ServicePort) (*corev1.Service, error) {
	var svc *corev1.Service
	switch behaviour {
	case l4BehaviourCreate:
		svc = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		if key, value := route.Labels["selectorkey"], route.Labels["selectorvalue"]; key != "" && value != "" {
			svc.Spec.Selector = map[string]string{key: value}
		}
	case l4BehaviourDuplicate, l4BehaviourUpdate:
		source, err := m.serviceLister.Services(namespace).Get(name)
		if err != nil {
			return nil, fmt.Errorf("%s %s/%s: service %s/%s to %s: %w", m.kind, route.Namespace, route.Name, namespace, name, behaviour, err)
		}
		if behaviour == l4BehaviourUpdate {
			svc = source.DeepCopy()
			// What the route's annotations do not keep of the loxilb.io/
			// ones goes, as it always has.
			for k := range svc.Annotations {
				if strings.Contains(k, "loxilb.io") {
					if _, kept := route.Annotations[k]; !kept {
						delete(svc.Annotations, k)
					}
				}
			}
			break
		}
		svc = &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name + "-gw-api", Labels: maps.Clone(source.Labels),
		}}
		svc.Spec = *source.Spec.DeepCopy()
		svc.Spec.ClusterIP = ""
		svc.Spec.ClusterIPs = nil
	}

	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	maps.Copy(svc.Annotations, route.Annotations)
	svc.Annotations["gateway-api-controller"] = m.gatewayProvider
	svc.Annotations[m.parentAnnotation] = route.Name
	svc.Annotations[m.parentAnnotation+"-namespace"] = route.Namespace
	svc.Annotations[l4BehaviourAnnotation] = behaviour

	if svc.Labels == nil {
		svc.Labels = map[string]string{}
	}
	svc.Labels["ipam-address"] = address
	svc.Labels["implementation"] = implementation

	loadBalancerClass := m.gatewayProvider
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.LoadBalancerClass = &loadBalancerClass
	svc.Spec.LoadBalancerIP = address
	svc.Spec.Ports = ports
	return svc, nil
}

// ownsService - whether this controller made or took svc for the route.
// One from an earlier build names the route but not its namespace, and is
// taken to be the route's in the route's own namespace only.
func (m *l4RouteManager) ownsService(key l4RouteKey, svc *corev1.Service) bool {
	if svc.Annotations["gateway-api-controller"] != m.gatewayProvider || svc.Annotations[m.parentAnnotation] != key.name {
		return false
	}
	if ns, ok := svc.Annotations[m.parentAnnotation+"-namespace"]; ok {
		return ns == key.namespace
	}
	return svc.Namespace == key.namespace
}

// ownedServices - the Services this controller made or took for the route,
// by namespace/name.
func (m *l4RouteManager) ownedServices(ctx context.Context, key l4RouteKey) (map[string]*corev1.Service, error) {
	list, err := m.kubeClient.CoreV1().Services(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set{"implementation": implementation}).String()})
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s: list services: %w", m.kind, key.namespace, key.name, err)
	}
	owned := map[string]*corev1.Service{}
	for i := range list.Items {
		svc := &list.Items[i]
		if m.ownsService(key, svc) {
			owned[svc.Namespace+"/"+svc.Name] = svc
		}
	}
	return owned, nil
}

// l4Original - what an update changes of a Service, as it was before.
type l4Original struct {
	Type              corev1.ServiceType   `json:"type,omitempty"`
	Ports             []corev1.ServicePort `json:"ports,omitempty"`
	LoadBalancerClass *string              `json:"loadBalancerClass,omitempty"`
	LoadBalancerIP    string               `json:"loadBalancerIP,omitempty"`
}

// applyService - create the desired Service, or bring the existing one to it.
//
// A Service of the desired name that is not the route's is left alone, but
// one an earlier build made for the route in another namespace - which did
// not note the route's namespace - is taken as its own. The node ports the
// API server gave the existing Service stay.
func (m *l4RouteManager) applyService(ctx context.Context, key l4RouteKey, desired *corev1.Service) error {
	services := m.kubeClient.CoreV1().Services(desired.Namespace)
	behaviour := desired.Annotations[l4BehaviourAnnotation]

	existing, err := services.Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		if behaviour == l4BehaviourUpdate {
			return fmt.Errorf("%s %s/%s: service %s/%s to update does not exist", m.kind, key.namespace, key.name, desired.Namespace, desired.Name)
		}
		if _, err := services.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("%s %s/%s: create service %s/%s: %w", m.kind, key.namespace, key.name, desired.Namespace, desired.Name, err)
		}
		klog.Infof("service %s/%s is created by %s %s/%s", desired.Namespace, desired.Name, m.kind, key.namespace, key.name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s %s/%s: get service %s/%s: %w", m.kind, key.namespace, key.name, desired.Namespace, desired.Name, err)
	}

	ours := existing.Annotations["gateway-api-controller"] == m.gatewayProvider && existing.Annotations[m.parentAnnotation] == key.name
	if ns, noted := existing.Annotations[m.parentAnnotation+"-namespace"]; noted && ns != key.namespace {
		ours = false
	}
	if !ours && behaviour != l4BehaviourUpdate {
		return fmt.Errorf("%s %s/%s: service %s/%s exists and was not made for the route", m.kind, key.namespace, key.name, desired.Namespace, desired.Name)
	}

	updated := existing.DeepCopy()
	if behaviour == l4BehaviourUpdate {
		if !ours {
			// Taken now: note what it was, to put it back when let go.
			original, err := json.Marshal(l4Original{
				Type: existing.Spec.Type, Ports: existing.Spec.Ports,
				LoadBalancerClass: existing.Spec.LoadBalancerClass, LoadBalancerIP: existing.Spec.LoadBalancerIP,
			})
			if err != nil {
				return err
			}
			desired.Annotations[l4OriginalAnnotation] = string(original)
		} else if original, ok := existing.Annotations[l4OriginalAnnotation]; ok {
			desired.Annotations[l4OriginalAnnotation] = original
		}
		updated.Annotations = desired.Annotations
		updated.Labels = desired.Labels
	} else {
		// A made one selects what the route's labels, or the Service it
		// copies, select now.
		updated.Annotations = desired.Annotations
		updated.Labels = desired.Labels
		updated.Spec.Selector = desired.Spec.Selector
	}
	updated.Spec.Type = desired.Spec.Type
	if updated.Spec.LoadBalancerClass == nil || *updated.Spec.LoadBalancerClass != *desired.Spec.LoadBalancerClass {
		updated.Spec.LoadBalancerClass = desired.Spec.LoadBalancerClass
	}
	updated.Spec.LoadBalancerIP = desired.Spec.LoadBalancerIP
	updated.Spec.Ports = keepNodePorts(existing.Spec.Ports, desired.Spec.Ports)

	if equality.Semantic.DeepEqual(existing, updated) {
		return nil
	}
	if _, err := services.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("%s %s/%s: update service %s/%s: %w", m.kind, key.namespace, key.name, desired.Namespace, desired.Name, err)
	}
	klog.Infof("service %s/%s is reconciled by %s %s/%s", desired.Namespace, desired.Name, m.kind, key.namespace, key.name)
	return nil
}

// keepNodePorts - desired, with the node port existing has for each.
func keepNodePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	out := make([]corev1.ServicePort, len(desired))
	copy(out, desired)
	for i := range out {
		for _, e := range existing {
			if e.Port == out[i].Port && e.Protocol == out[i].Protocol {
				out[i].NodePort = e.NodePort
				break
			}
		}
	}
	return out
}

// releaseService - let go of a Service the route no longer calls for. One the
// controller made is deleted. One an update took is the owner's, and is put
// back as it was before - as near as an earlier build, which did not note
// that, allows: a ClusterIP Service with the ports it has now.
func (m *l4RouteManager) releaseService(ctx context.Context, key l4RouteKey, svc *corev1.Service) error {
	services := m.kubeClient.CoreV1().Services(svc.Namespace)
	if svc.Annotations[l4BehaviourAnnotation] != l4BehaviourUpdate {
		if err := services.Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("%s %s/%s: delete service %s/%s: %w", m.kind, key.namespace, key.name, svc.Namespace, svc.Name, err)
		}
		klog.Infof("service %s/%s is deleted by %s %s/%s", svc.Namespace, svc.Name, m.kind, key.namespace, key.name)
		return nil
	}

	restored := svc.DeepCopy()
	original := l4Original{Type: corev1.ServiceTypeClusterIP, Ports: svc.Spec.Ports}
	if noted, ok := svc.Annotations[l4OriginalAnnotation]; ok {
		if err := json.Unmarshal([]byte(noted), &original); err != nil {
			klog.Errorf("%s %s/%s: service %s/%s: unreadable %s: %v", m.kind, key.namespace, key.name, svc.Namespace, svc.Name, l4OriginalAnnotation, err)
			original = l4Original{Type: corev1.ServiceTypeClusterIP, Ports: svc.Spec.Ports}
		}
	}
	restored.Spec.Type = original.Type
	restored.Spec.Ports = original.Ports
	restored.Spec.LoadBalancerClass = original.LoadBalancerClass
	restored.Spec.LoadBalancerIP = original.LoadBalancerIP
	for _, k := range []string{"gateway-api-controller", m.parentAnnotation, m.parentAnnotation + "-namespace",
		l4BehaviourAnnotation, l4OriginalAnnotation} {
		delete(restored.Annotations, k)
	}
	delete(restored.Labels, "ipam-address")
	delete(restored.Labels, "implementation")

	if _, err := services.Update(ctx, restored, metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s: put back service %s/%s: %w", m.kind, key.namespace, key.name, svc.Namespace, svc.Name, err)
	}
	klog.Infof("service %s/%s is let go by %s %s/%s and put back as it was", svc.Namespace, svc.Name, m.kind, key.namespace, key.name)
	return nil
}
