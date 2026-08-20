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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	sigsclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	sigsexternalversions "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigsv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	infclientset "sigs.k8s.io/gateway-api-inference-extension/client-go/clientset/versioned"
	infexternalversions "sigs.k8s.io/gateway-api-inference-extension/client-go/informers/externalversions"
	infv1lister "sigs.k8s.io/gateway-api-inference-extension/client-go/listers/api/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

const (
	inferencePoolMgrName = "InferencePoolManager"

	// poolOwnerLabel - set on every Service this manager creates, so the ones
	// belonging to a pool can be listed directly instead of scanning the
	// namespace and filtering on annotations.
	poolOwnerLabel = "loxilb.io/inference-pool"

	// serviceSuffix - appended to the pool name. A pool exposed on more than
	// one listener gets the target appended after this.
	serviceSuffix = "-inference"

	// maxServiceNameLen - RFC 1035 label limit the API server enforces on
	// Service names.
	maxServiceNameLen = 63

	// inferencePoolCRDName - watched only once this exists. The Inference
	// Extension ships its own CRDs, so a cluster can have the Gateway API
	// without them.
	inferencePoolCRDName = "inferencepools.inference.networking.k8s.io"

	loxilbAnnotationPrefix  = "loxilb.io/"
	lbModeAnnotation        = "loxilb.io/lbmode"
	epSelectAnnotation      = "loxilb.io/epselect"
	usePodNetworkAnnotation = "loxilb.io/usepodnetwork"

	lbModeFullProxy = "fullproxy"
)

// gatewayOnlySelectors - endpoint selection algorithms that exist only in
// loxilb-inference-gateway and only work under mode=fullproxy. Outside
// fullproxy the kernel selector matches no case and black-holes the traffic,
// so a rule combining them with another mode has to be refused rather than
// programmed.
var gatewayOnlySelectors = map[string]bool{
	"chwbl":     true,
	"gpuaware":  true,
	"wrr-hash":  true,
	"wrrhash":   true,
}

// InferencePoolManager - translates InferencePool into the LoadBalancer
// Service that the load-balancer manager already knows how to program.
//
// loxilb-inference-gateway picks endpoints itself - prefix-cache aware,
// GPU-aware, KV-exact, prefill/decode - which is the job the Inference
// Extension gives to an Endpoint Picker. So a pool is read here as what it
// describes rather than as a delegation: which pods, on which port. That is
// `selector` and `targetPorts`, and both map onto a Service directly.
type InferencePoolManager struct {
	kubeClient      clientset.Interface
	kubeExtClient   apiextensionclientset.Interface
	sigsClient      sigsclientset.Interface
	infClient       infclientset.Interface
	networkConfig   *config.NetworkConfig
	gatewayProvider string

	poolLister      infv1lister.InferencePoolLister
	poolSynced      cache.InformerSynced
	httpRouteLister sigsv1lister.HTTPRouteLister
	httpRouteSynced cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[InferencePoolQueueEntry]
}

// InferencePoolQueueEntry - a pool to reconcile.
type InferencePoolQueueEntry struct {
	Namespace string
	Name      string
}

// poolTarget - one place a pool is exposed: a Gateway listener, reached
// through an HTTPRoute that lists the pool in its backendRefs.
//
// A pool on its own says nothing about where it is served from; only the route
// that references it names a Gateway. That is why this manager watches routes
// as well as pools.
type poolTarget struct {
	gateway          *v1.Gateway
	listener         *v1.Listener
	routeName        string
	routeAnnotations map[string]string
}

func NewInferencePoolManager(
	kubeClient clientset.Interface,
	kubeExtClient apiextensionclientset.Interface,
	sigsClient sigsclientset.Interface,
	infClient infclientset.Interface,
	networkConfig *config.NetworkConfig,
	sigsInformerFactory sigsexternalversions.SharedInformerFactory,
	infInformerFactory infexternalversions.SharedInformerFactory) *InferencePoolManager {

	poolInformer := infInformerFactory.Inference().V1().InferencePools()
	httpRouteInformer := sigsInformerFactory.Gateway().V1().HTTPRoutes()

	manager := &InferencePoolManager{
		kubeClient:      kubeClient,
		kubeExtClient:   kubeExtClient,
		sigsClient:      sigsClient,
		infClient:       infClient,
		networkConfig:   networkConfig,
		gatewayProvider: networkConfig.LoxilbGatewayClass,
		poolLister:      poolInformer.Lister(),
		poolSynced:      poolInformer.Informer().HasSynced,
		httpRouteLister: httpRouteInformer.Lister(),
		httpRouteSynced: httpRouteInformer.Informer().HasSynced,

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[InferencePoolQueueEntry](minRetryDelay, maxRetryDelay),
			workqueue.TypedRateLimitingQueueConfig[InferencePoolQueueEntry]{Name: "inferencePool"}),
	}

	poolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(cur interface{}) { manager.enqueuePool(cur) },
		UpdateFunc: func(old, cur interface{}) { manager.enqueuePool(cur) },
		DeleteFunc: func(old interface{}) { manager.enqueuePool(old) },
	})

	// A route change can attach a pool to a Gateway, move it to another
	// listener, or detach it, none of which touches the pool object. Without
	// this the pool would keep whatever Service it had at the moment it was
	// last written.
	httpRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(cur interface{}) { manager.enqueueRoutePools(cur) },
		UpdateFunc: func(old, cur interface{}) { manager.enqueueRoutePools(old); manager.enqueueRoutePools(cur) },
		DeleteFunc: func(old interface{}) { manager.enqueueRoutePools(old) },
	})

	return manager
}

func (m *InferencePoolManager) enqueuePool(obj interface{}) {
	pool, ok := obj.(*infv1.InferencePool)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("%s: received unexpected object: %v", inferencePoolMgrName, obj)
			return
		}
		pool, ok = deletedState.Obj.(*infv1.InferencePool)
		if !ok {
			klog.Errorf("%s: DeletedFinalStateUnknown contains non-InferencePool object: %v", inferencePoolMgrName, deletedState.Obj)
			return
		}
	}

	m.queue.Add(InferencePoolQueueEntry{Namespace: pool.Namespace, Name: pool.Name})
}

func (m *InferencePoolManager) enqueueRoutePools(obj interface{}) {
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

	for _, entry := range poolsReferencedBy(route) {
		m.queue.Add(entry)
	}
}

// poolsReferencedBy - every InferencePool a route sends traffic to.
func poolsReferencedBy(route *v1.HTTPRoute) []InferencePoolQueueEntry {
	seen := map[InferencePoolQueueEntry]bool{}
	var entries []InferencePoolQueueEntry

	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if classifyBackendRef(backendRef.BackendObjectReference) != backendInferencePool {
				continue
			}

			namespace := route.Namespace
			if backendRef.Namespace != nil {
				namespace = string(*backendRef.Namespace)
			}

			entry := InferencePoolQueueEntry{Namespace: namespace, Name: string(backendRef.Name)}
			if !seen[entry] {
				seen[entry] = true
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// WaitForInferencePoolCRD - block until the InferencePool CRD exists.
//
// The extension is optional and its CRDs are installed separately, so a
// cluster without them is a normal configuration, not an error: wait quietly
// instead of failing the agent or filling the log with watch errors.
func (m *InferencePoolManager) WaitForInferencePoolCRD(stopCh <-chan struct{}) bool {
	logged := false

	err := wait.PollUntilContextCancel(wait.ContextForChannel(stopCh), time.Second*5, true,
		func(ctx context.Context) (bool, error) {
			_, err := m.kubeExtClient.ApiextensionsV1().CustomResourceDefinitions().
				Get(ctx, inferencePoolCRDName, metav1.GetOptions{})
			if err != nil {
				if !logged {
					klog.Infof("%s: waiting for the %s CRD - install the Gateway API Inference Extension to use it",
						inferencePoolMgrName, inferencePoolCRDName)
					logged = true
				}
				return false, nil
			}

			klog.Infof("%s: %s CRD found", inferencePoolMgrName, inferencePoolCRDName)
			return true, nil
		})

	return err == nil
}

func (m *InferencePoolManager) Run(stopCh <-chan struct{}) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", inferencePoolMgrName)
	defer klog.Infof("Shutting down %s", inferencePoolMgrName)

	if !cache.WaitForNamedCacheSync(inferencePoolMgrName, stopCh, m.poolSynced, m.httpRouteSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *InferencePoolManager) worker() {
	for m.processNextWorkItem() {
	}
}

func (m *InferencePoolManager) processNextWorkItem() bool {
	entry, quit := m.queue.Get()
	if quit {
		return false
	}
	defer m.queue.Done(entry)

	if err := m.reconcile(entry.Namespace, entry.Name); err == nil {
		m.queue.Forget(entry)
	} else {
		m.queue.AddRateLimited(entry)
		klog.Errorf("Error syncing InferencePool %s/%s, requeuing. Error: %v", entry.Namespace, entry.Name, err)
	}

	return true
}

func (m *InferencePoolManager) reconcile(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	pool, err := m.poolLister.InferencePools(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return m.deleteOwnedServices(ctx, namespace, name, nil)
		}
		return err
	}

	targets, err := m.findTargets(ctx, pool)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		// Nothing references this pool, so there is no Gateway to serve it
		// from and no parent to report a condition under. Drop whatever we
		// had; the route coming back re-queues the pool.
		klog.V(2).Infof("InferencePool %s/%s is not referenced by any HTTPRoute", namespace, name)
		if err := m.deleteOwnedServices(ctx, namespace, name, nil); err != nil {
			return err
		}
		return m.clearStatus(ctx, pool)
	}

	// The Inference Extension hands endpoint selection to an Endpoint Picker;
	// loxilb does that selection itself. Ignoring a picker the user asked for
	// would serve a different routing policy than the one they declared, so
	// only FailOpen - "carry on if the picker is unavailable" - is accepted.
	if reason, message, ok := endpointPickerDecision(pool); !ok {
		klog.Warningf("InferencePool %s/%s: %s", namespace, name, message)
		if err := m.deleteOwnedServices(ctx, namespace, name, nil); err != nil {
			return err
		}
		return m.updateStatus(ctx, pool, targets, statusReport{
			accepted: false, acceptedReason: reason, acceptedMessage: message,
			resolvedRefs: true, resolvedRefsReason: string(infv1.InferencePoolReasonResolvedRefs),
		})
	} else if message != "" {
		klog.Infof("InferencePool %s/%s: %s", namespace, name, message)
	}

	desired, err := m.buildServices(pool, targets)
	if err != nil {
		klog.Errorf("InferencePool %s/%s: %v", namespace, name, err)
		if delErr := m.deleteOwnedServices(ctx, namespace, name, nil); delErr != nil {
			return delErr
		}
		return m.updateStatus(ctx, pool, targets, statusReport{
			accepted: false, acceptedReason: string(infv1.InferencePoolReasonNotSupportedByParent),
			acceptedMessage: err.Error(),
			resolvedRefs:    true, resolvedRefsReason: string(infv1.InferencePoolReasonResolvedRefs),
		})
	}

	keep := make(map[string]bool, len(desired))
	for _, svc := range desired {
		keep[svc.Name] = true
		if err := m.applyService(ctx, pool, svc); err != nil {
			return err
		}
	}

	if err := m.deleteOwnedServices(ctx, namespace, name, keep); err != nil {
		return err
	}

	klog.Infof("InferencePool %s/%s is reconciled into %d service(s)", namespace, name, len(desired))

	return m.updateStatus(ctx, pool, targets, statusReport{
		accepted: true, acceptedReason: string(infv1.InferencePoolReasonAccepted),
		acceptedMessage: "managed by kube-loxilb",
		resolvedRefs:    true, resolvedRefsReason: string(infv1.InferencePoolReasonResolvedRefs),
	})
}

// endpointPickerDecision - whether a pool's endpointPickerRef lets us serve it.
//
// Returns the reason and message to report, and whether the pool is accepted.
// A pool with no picker is the plain case and reports nothing.
//
// Absence is read as an empty name rather than a nil reference. The field is
// required in the v1.5.0 CRD and optional from v1.6.0, and the Go type changed
// from a value to a pointer with it; testing the name works either way, and
// works when a v1.6.0 cluster serves a pool that omits the field entirely.
func endpointPickerDecision(pool *infv1.InferencePool) (reason, message string, ok bool) {
	ref := pool.Spec.EndpointPickerRef
	if ref.Name == "" {
		return "", "", true
	}

	if ref.FailureMode == infv1.EndpointPickerFailOpen {
		return "", fmt.Sprintf("endpointPickerRef %q is ignored - loxilb selects endpoints itself; "+
			"accepted because failureMode is FailOpen", string(ref.Name)), true
	}

	return string(infv1.InferencePoolReasonNotSupportedByParent),
		fmt.Sprintf("endpointPickerRef %q requires an Endpoint Picker, which kube-loxilb does not delegate to "+
			"(loxilb selects endpoints itself). Remove endpointPickerRef, or set failureMode: FailOpen to accept "+
			"loxilb's own endpoint selection", string(ref.Name)),
		false
}

// findTargets - resolve every Gateway listener this pool is served on.
func (m *InferencePoolManager) findTargets(ctx context.Context, pool *infv1.InferencePool) ([]poolTarget, error) {
	routes, err := m.httpRouteLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	// Sorted so the generated Service names and the status parents do not
	// depend on lister ordering.
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Namespace != routes[j].Namespace {
			return routes[i].Namespace < routes[j].Namespace
		}
		return routes[i].Name < routes[j].Name
	})

	var targets []poolTarget
	for _, route := range routes {
		if !routeReferencesPool(route, pool) {
			continue
		}

		for _, parentRef := range route.Spec.ParentRefs {
			gateway, err := m.findGatewayForRoute(ctx, route, parentRef)
			if err != nil {
				klog.V(4).Infof("InferencePool %s/%s: parent gateway of HTTPRoute %s/%s unresolved: %v",
					pool.Namespace, pool.Name, route.Namespace, route.Name, err)
				continue
			}
			if len(gateway.Status.Addresses) == 0 {
				// The Gateway manager assigns the address; until it has, there
				// is nothing to put in the Service.
				return nil, fmt.Errorf("gateway %s/%s has no address assigned yet", gateway.Namespace, gateway.Name)
			}

			listener := findListenerForRoute(gateway, parentRef)
			if listener == nil {
				klog.Warningf("InferencePool %s/%s: gateway %s/%s has no listener matching the parentRef of HTTPRoute %s/%s",
					pool.Namespace, pool.Name, gateway.Namespace, gateway.Name, route.Namespace, route.Name)
				continue
			}

			targets = append(targets, poolTarget{
				gateway:          gateway,
				listener:         listener,
				routeName:        route.Name,
				routeAnnotations: route.Annotations,
			})
		}
	}

	return targets, nil
}

func routeReferencesPool(route *v1.HTTPRoute, pool *infv1.InferencePool) bool {
	for _, entry := range poolsReferencedBy(route) {
		if entry.Namespace == pool.Namespace && entry.Name == pool.Name {
			return true
		}
	}

	return false
}

func (m *InferencePoolManager) findGatewayForRoute(ctx context.Context, route *v1.HTTPRoute, parentRef v1.ParentReference) (*v1.Gateway, error) {
	namespace := route.Namespace
	if parentRef.Namespace != nil {
		namespace = string(*parentRef.Namespace)
	}

	return m.sigsClient.GatewayV1().Gateways(namespace).Get(ctx, string(parentRef.Name), metav1.GetOptions{})
}

// findListenerForRoute - the listener a parentRef attaches to.
//
// sectionName is the explicit form, but it is optional and most Inference
// Extension examples leave it out, so port and then protocol are used as
// fallbacks. Returning nil for an unset sectionName - which is what the older
// route managers do - would make those examples silently produce nothing.
func findListenerForRoute(gateway *v1.Gateway, parentRef v1.ParentReference) *v1.Listener {
	if parentRef.SectionName != nil {
		for i := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[i].Name == *parentRef.SectionName {
				return &gateway.Spec.Listeners[i]
			}
		}
		return nil
	}

	if parentRef.Port != nil {
		for i := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[i].Port == *parentRef.Port {
				return &gateway.Spec.Listeners[i]
			}
		}
		return nil
	}

	for i := range gateway.Spec.Listeners {
		protocol := gateway.Spec.Listeners[i].Protocol
		if protocol == v1.HTTPProtocolType || protocol == v1.HTTPSProtocolType {
			return &gateway.Spec.Listeners[i]
		}
	}

	return nil
}

// buildServices - the Services that express this pool on each of its targets.
func (m *InferencePoolManager) buildServices(pool *infv1.InferencePool, targets []poolTarget) ([]*corev1.Service, error) {
	if len(pool.Spec.TargetPorts) == 0 {
		return nil, fmt.Errorf("targetPorts is empty")
	}
	if len(pool.Spec.Selector.MatchLabels) == 0 {
		return nil, fmt.Errorf("selector.matchLabels is empty")
	}
	if len(pool.Spec.TargetPorts) > 1 {
		// Every port would need its own rule; until that is implemented,
		// say which one is actually being served rather than appearing to
		// serve all of them.
		klog.Warningf("InferencePool %s/%s declares %d targetPorts; only the first (%d) is served",
			pool.Namespace, pool.Name, len(pool.Spec.TargetPorts), pool.Spec.TargetPorts[0].Number)
	}

	multi := len(targets) > 1
	services := make([]*corev1.Service, 0, len(targets))
	for _, target := range targets {
		svc, err := m.buildService(pool, target, multi)
		if err != nil {
			return nil, err
		}
		services = append(services, svc)
	}

	return services, nil
}

func (m *InferencePoolManager) buildService(pool *infv1.InferencePool, target poolTarget, multi bool) (*corev1.Service, error) {
	annotations, err := m.buildAnnotations(pool, target)
	if err != nil {
		return nil, err
	}

	selector := make(map[string]string, len(pool.Spec.Selector.MatchLabels))
	for key, value := range pool.Spec.Selector.MatchLabels {
		selector[string(key)] = string(value)
	}

	loadBalancerClass := m.networkConfig.LoxilbLoadBalancerClass

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceNameFor(pool.Name, target, multi),
			Namespace: pool.Namespace,
			Labels: map[string]string{
				"implementation": implementation,
				poolOwnerLabel:   pool.Name,
			},
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &loadBalancerClass,
			LoadBalancerIP:    target.gateway.Status.Addresses[0].Value,
			Selector:          selector,
			Ports: []corev1.ServicePort{{
				Name:       "inference",
				Port:       int32(target.listener.Port),
				TargetPort: intstr.FromInt32(int32(pool.Spec.TargetPorts[0].Number)),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}, nil
}

// buildAnnotations - the loxilb settings the generated Service carries.
//
// Precedence is pool, then route, then the two values this translation has to
// impose. Route last because the route is the more specific statement: the
// same pool served through two routes can be tuned per route.
func (m *InferencePoolManager) buildAnnotations(pool *infv1.InferencePool, target poolTarget) (map[string]string, error) {
	annotations := map[string]string{}
	copyLoxilbAnnotations(pool.Annotations, annotations)
	copyLoxilbAnnotations(target.routeAnnotations, annotations)

	// Endpoints have to be pod addresses. Selecting on cache locality or GPU
	// state means choosing between model servers, and a node address with a
	// NodePort behind it hands that choice to kube-proxy instead.
	if value, ok := annotations[usePodNetworkAnnotation]; ok && value != "yes" {
		return nil, fmt.Errorf("%s must be \"yes\" for an InferencePool: inference routing selects between "+
			"individual model server pods, which node-address endpoints cannot express", usePodNetworkAnnotation)
	}
	annotations[usePodNetworkAnnotation] = "yes"

	selector := strings.ToLower(annotations[epSelectAnnotation])
	if mode, ok := annotations[lbModeAnnotation]; ok {
		if gatewayOnlySelectors[selector] && mode != lbModeFullProxy {
			return nil, fmt.Errorf("%s=%q requires %s=%q, got %q",
				epSelectAnnotation, selector, lbModeAnnotation, lbModeFullProxy, mode)
		}
	} else {
		annotations[lbModeAnnotation] = lbModeFullProxy
	}

	annotations["gateway-api-controller"] = m.gatewayProvider
	annotations["parent-inference-pool"] = pool.Name
	annotations["parent-gateway"] = target.gateway.Name
	annotations["parent-gateway-namespace"] = target.gateway.Namespace
	annotations["parent-http-route"] = target.routeName

	return annotations, nil
}

// copyLoxilbAnnotations - copy only the loxilb settings.
//
// Copying every annotation, as the older route managers do, also copies
// kubectl's last-applied-configuration and any other bookkeeping the source
// object happens to carry.
func copyLoxilbAnnotations(src, dst map[string]string) {
	for key, value := range src {
		if strings.HasPrefix(key, loxilbAnnotationPrefix) {
			dst[key] = value
		}
	}
}

// serviceNameFor - "<pool>-inference", with the target appended when the pool
// is served on more than one listener.
func serviceNameFor(poolName string, target poolTarget, multi bool) string {
	name := poolName + serviceSuffix
	if multi {
		name = fmt.Sprintf("%s-%s-%s", name, target.gateway.Name, target.listener.Name)
	}

	return truncateName(name)
}

// truncateName - keep a name inside the API server's limit without letting two
// different long names collapse onto one.
func truncateName(name string) string {
	if len(name) <= maxServiceNameLen {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]

	return strings.Trim(name[:maxServiceNameLen-len(suffix)], "-") + suffix
}

// applyService - create the Service, or update it when it already exists and
// belongs to this pool.
func (m *InferencePoolManager) applyService(ctx context.Context, pool *infv1.InferencePool, desired *corev1.Service) error {
	existing, err := m.kubeClient.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		if _, err := m.kubeClient.CoreV1().Services(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return err
		}
		klog.Infof("service %s/%s is created by InferencePool %s", desired.Namespace, desired.Name, pool.Name)

		return nil
	}

	if owner := existing.Labels[poolOwnerLabel]; owner != pool.Name {
		return fmt.Errorf("service %s/%s already exists and is not owned by InferencePool %s",
			desired.Namespace, desired.Name, pool.Name)
	}

	if serviceUpToDate(existing, desired) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.Labels = desired.Labels
	updated.Annotations = desired.Annotations
	updated.Spec.Type = desired.Spec.Type
	updated.Spec.LoadBalancerClass = desired.Spec.LoadBalancerClass
	updated.Spec.LoadBalancerIP = desired.Spec.LoadBalancerIP
	updated.Spec.Selector = desired.Spec.Selector
	updated.Spec.Ports = mergeServicePorts(existing.Spec.Ports, desired.Spec.Ports)

	if _, err := m.kubeClient.CoreV1().Services(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return err
	}
	klog.Infof("service %s/%s is updated by InferencePool %s", updated.Namespace, updated.Name, pool.Name)

	return nil
}

// mergeServicePorts - keep the node port the API server already allocated.
//
// Dropping it would make the apiserver allocate a new one on every update, and
// each reallocation reprograms the loxilb rule for no reason.
func mergeServicePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	merged := make([]corev1.ServicePort, len(desired))
	copy(merged, desired)

	for i := range merged {
		for _, old := range existing {
			if old.Port == merged[i].Port && old.Protocol == merged[i].Protocol {
				merged[i].NodePort = old.NodePort
				break
			}
		}
	}

	return merged
}

func serviceUpToDate(existing, desired *corev1.Service) bool {
	if !reflect.DeepEqual(existing.Annotations, desired.Annotations) ||
		!reflect.DeepEqual(existing.Labels, desired.Labels) ||
		!reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) ||
		existing.Spec.LoadBalancerIP != desired.Spec.LoadBalancerIP ||
		existing.Spec.Type != desired.Spec.Type {
		return false
	}

	if len(existing.Spec.Ports) != len(desired.Spec.Ports) {
		return false
	}
	for i := range desired.Spec.Ports {
		if existing.Spec.Ports[i].Port != desired.Spec.Ports[i].Port ||
			existing.Spec.Ports[i].TargetPort != desired.Spec.Ports[i].TargetPort ||
			existing.Spec.Ports[i].Protocol != desired.Spec.Ports[i].Protocol {
			return false
		}
	}

	return true
}

// deleteOwnedServices - remove the Services of this pool that are no longer
// wanted. keep is nil when the pool itself is gone.
func (m *InferencePoolManager) deleteOwnedServices(ctx context.Context, namespace, poolName string, keep map[string]bool) error {
	list, err := m.kubeClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: poolOwnerLabel + "=" + poolName,
	})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for i := range list.Items {
		svc := &list.Items[i]
		if keep[svc.Name] {
			continue
		}

		if err := m.kubeClient.CoreV1().Services(namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		klog.Infof("service %s/%s is deleted by InferencePool %s", namespace, svc.Name, poolName)
	}

	return nil
}
