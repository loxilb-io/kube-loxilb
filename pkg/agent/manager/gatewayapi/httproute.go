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
	"time"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
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
	sigsInformerFactory externalversions.SharedInformerFactory) *HTTPRouteManager {

	httpRouteInformer := sigsInformerFactory.Gateway().V1().HTTPRoutes()
	manager := &HTTPRouteManager{
		kubeClient:            kubeClient,
		sigsClient:            sigsClient,
		networkConfig:         networkConfig,
		gatewayProvider:       networkConfig.LoxilbGatewayClass,
		httpRouteInformer:     httpRouteInformer,
		httpRouteLister:       httpRouteInformer.Lister(),
		httpRouteListerSynced: httpRouteInformer.Informer().HasSynced,

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

	return manager
}

func (h *HTTPRouteManager) enqueueObject(obj interface{}) {
	httpRoute, ok := obj.(*v1.HTTPRoute)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		httpRoute, ok = obj.(*v1.HTTPRoute)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	h.queue.Add(HTTPRouteQueueEntry{httpRoute.Namespace, httpRoute.Name})
}

func (h *HTTPRouteManager) Run(stopCh <-chan struct{}) {
	defer h.queue.ShutDown()

	klog.Infof("Starting %s", httpRouteMgrName)
	defer klog.Infof("Shutting down %s", httpRouteMgrName)

	if !cache.WaitForNamedCacheSync(
		httpRouteMgrName,
		stopCh,
		h.httpRouteListerSynced) {
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

	return h.createHTTPRoute(httpRoute)
}

func (h *HTTPRouteManager) createHTTPRoute(httpRoute *v1.HTTPRoute) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// find all parent resources
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		// If no namespace is specified, the parent resource is searched for in the same namespace as HTTPRoute.
		parentNs := "default"
		if parentRef.Namespace != nil {
			parentNs = string(*parentRef.Namespace)
		}
		gateway, err := h.findGateway(ctx, httpRoute, parentRef)
		if err != nil {
			klog.Errorf("not found gateway %s/%s", parentNs, string(parentRef.Name))
			return err
		}

		if len(gateway.Status.Addresses) == 0 {
			return fmt.Errorf("gateway %s/%s has no addresses assigned", gateway.Namespace, gateway.Name)
		}

		listener := h.findListener(gateway, parentRef)
		if listener != nil {
			err := h.reconcileIngress(ctx, httpRoute, gateway, listener)
			if err != nil {
				klog.Errorf("failure reconcileIngress. err: %v", err)
				return err
			}
		} else {
			klog.Infof("Unknown Listener on gateway %s/%s", gateway.Namespace, gateway.Name)
		}
	}

	klog.Infof("HTTPRoute %s is reconciled.", httpRoute.Name)
	return nil
}

func (h *HTTPRouteManager) deleteHTTPRoute(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	ingList, err := h.kubeClient.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("HTTPRoute: get ingresses list (ns: %s) failure. err: %v", namespace, err)
		return err
	}

	for _, ingress := range ingList.Items {
		if ingress.Annotations["gateway-api-controller"] == h.gatewayProvider &&
			ingress.Annotations["parent-http-route"] == name {

			err = h.kubeClient.NetworkingV1().Ingresses(namespace).Delete(ctx, ingress.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("HTTPRoute: delete ingress %s/%s failure. err: %v", namespace, ingress.Name, err)
				return err
			}
			klog.Infof("ingress %s/%s is deleted by HTTPRoute %s", namespace, ingress.Name, name)

		}
	}

	klog.Infof("HTTPRoute %s is deleted.", name)
	return nil
}

func (h *HTTPRouteManager) findGateway(ctx context.Context, httpRoute *v1.HTTPRoute, parentRef v1.ParentReference) (*v1.Gateway, error) {
	var gatewayNamespace string
	if parentRef.Namespace != nil {
		gatewayNamespace = string(*parentRef.Namespace)
	} else {
		gatewayNamespace = httpRoute.Namespace
	}

	gatewayName := string(parentRef.Name)
	gateway, err := h.sigsClient.GatewayV1().Gateways(gatewayNamespace).Get(ctx, gatewayName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("HTTPRoute %s/%s parentRefs gateway %s/%s found failure. err: %v", httpRoute.Namespace, httpRoute.Name, gatewayNamespace, gatewayName, err)
		return nil, err
	}

	return gateway, nil
}

func (h *HTTPRouteManager) findListener(gateway *v1.Gateway, parentRef v1.ParentReference) *v1.Listener {
	if parentRef.SectionName != nil {
		for i := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[i].Name == *parentRef.SectionName {
				return &gateway.Spec.Listeners[i]
			}
		}
	}

	return nil
}

func (h *HTTPRouteManager) reconcileIngress(ctx context.Context, httpRoute *v1.HTTPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	// create ingress
	_, err := h.createIngress(ctx, httpRoute, gateway, listener)
	if err != nil {
		klog.Errorf("HTTPManager: failed to create new ingress %s/%s. err: %v", httpRoute.Namespace, httpRoute.Name, err)
	}
	return nil
}

func (h *HTTPRouteManager) createIngress(ctx context.Context, httpRoute *v1.HTTPRoute, gateway *v1.Gateway, listener *v1.Listener) (*netv1.Ingress, error) {
	newIngress := netv1.Ingress{}
	newIngress.Name = httpRoute.Name
	newIngress.Namespace = httpRoute.Namespace

	newIngress.SetAnnotations(httpRoute.Annotations)
	if newIngress.Annotations == nil {
		newIngress.Annotations = map[string]string{}
	}
	newIngress.Annotations["gateway-api-controller"] = h.gatewayProvider
	newIngress.Annotations["parent-gateway"] = gateway.Name
	newIngress.Annotations["parent-gateway-namespace"] = gateway.Namespace
	newIngress.Annotations["parent-http-route"] = httpRoute.Name

	newIngress.SetLabels(map[string]string{
		"implementation": implementation,
	})
	ingressClass := "loxilb"
	newIngress.Spec.IngressClassName = &ingressClass

	// If the listener has a hostname, it takes precedence over httpRoute.
	var hostnames []string
	if listener.Hostname != nil {
		hostnames = append(hostnames, string(*listener.Hostname))
	} else {
		for _, hostname := range httpRoute.Spec.Hostnames {
			hostnames = append(hostnames, string(hostname))
		}
	}

	// TODO: Currently, only TLS Kind is supported.
	if listener.TLS != nil {
		for _, tlsCertRef := range listener.TLS.CertificateRefs {
			if tlsCertRef.Kind != nil && *tlsCertRef.Kind == v1.Kind("Secret") {
				newIngressTLS := netv1.IngressTLS{
					SecretName: string(tlsCertRef.Name),
					Hosts:      hostnames,
				}
				newIngress.Spec.TLS = append(newIngress.Spec.TLS, newIngressTLS)
			}
		}
	}

	for _, rule := range httpRoute.Spec.Rules {
		var paths []netv1.HTTPIngressPath
		for _, match := range rule.Matches {
			for _, backref := range rule.BackendRefs {

				if backref.Namespace != nil {
					if newIngress.Namespace != string(*backref.Namespace) {
						newIngress.Annotations["external-backend-service"] = "true"
						newIngress.Annotations["service-"+string(backref.Name)+"-namespace"] = string(*backref.Namespace)
					}
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

		for _, hostname := range hostnames {
			newRule := netv1.IngressRule{
				Host: string(hostname),
				IngressRuleValue: netv1.IngressRuleValue{
					HTTP: &netv1.HTTPIngressRuleValue{
						Paths: paths,
					},
				},
			}
			newIngress.Spec.Rules = append(newIngress.Spec.Rules, newRule)
		}
	}

	ing, err := h.kubeClient.NetworkingV1().Ingresses(newIngress.Namespace).Create(ctx, &newIngress, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return ing, nil
}
