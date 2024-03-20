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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	sigsclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigs_v1alpha2_informer "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1alpha2"
	sigs_v1alpha2_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1alpha2"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

const (
	udpRouteMgrName              = "UDPRouteManager"
	udpRouteRuleServiceCreate    = "create"
	udpRouteRuleServiceDuplicate = "duplicate"
	udpRouteRuleServiceUpdate    = "update"
)

type UDPRouteManager struct {
	kubeClient           clientset.Interface
	sigsClient           sigsclientset.Interface
	networkConfig        *config.NetworkConfig
	gatewayProvider      string
	udpRouteInformer     sigs_v1alpha2_informer.UDPRouteInformer
	udpRouteLister       sigs_v1alpha2_lister.UDPRouteLister
	udpRouteListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

type UDPRouteQueueEntry struct {
	namespace string
	name      string
}

func NewUDPRouteManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	sigsInformerFactory externalversions.SharedInformerFactory) *UDPRouteManager {

	udpRouteInformer := sigsInformerFactory.Gateway().V1alpha2().UDPRoutes()
	manager := &UDPRouteManager{
		kubeClient:           kubeClient,
		sigsClient:           sigsClient,
		networkConfig:        networkConfig,
		gatewayProvider:      networkConfig.LoxilbGatewayClass,
		udpRouteInformer:     udpRouteInformer,
		udpRouteLister:       udpRouteInformer.Lister(),
		udpRouteListerSynced: udpRouteInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "gatewayClass"),
	}

	manager.udpRouteInformer.Informer().AddEventHandler(
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

func (u *UDPRouteManager) enqueueObject(obj interface{}) {
	udpRoute, ok := obj.(*v1alpha2.UDPRoute)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		udpRoute, ok = obj.(*v1alpha2.UDPRoute)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	u.queue.Add(UDPRouteQueueEntry{udpRoute.Namespace, udpRoute.Name})
}

func (u *UDPRouteManager) Run(stopCh <-chan struct{}) {
	defer u.queue.ShutDown()

	klog.Infof("Starting %s", udpRouteMgrName)
	defer klog.Infof("Shutting down %s", udpRouteMgrName)

	if !cache.WaitForNamedCacheSync(
		udpRouteMgrName,
		stopCh,
		u.udpRouteListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(u.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (u *UDPRouteManager) worker() {
	for u.processNextWorkItem() {
	}
}

func (u *UDPRouteManager) processNextWorkItem() bool {
	obj, quit := u.queue.Get()
	if quit {
		return false
	}
	defer u.queue.Done(obj)

	if key, ok := obj.(UDPRouteQueueEntry); !ok {
		u.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := u.reconcile(key.namespace, key.name); err == nil {
		u.queue.Forget(obj)
	} else {
		u.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing udpRoute %s/%s, requeuing. Error: %v", key.namespace, key.name, err)
	}
	return true
}

func (u *UDPRouteManager) reconcile(namespace, name string) error {
	udpRoute, err := u.udpRouteLister.UDPRoutes(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return u.deleteUDPRoute(namespace, name)
		}
		return err
	}

	return u.createUDPRoute(udpRoute)
}

func (u *UDPRouteManager) createUDPRoute(udpRoute *v1alpha2.UDPRoute) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// find all parent resources
	for _, parentRef := range udpRoute.Spec.ParentRefs {
		// If no namespace is specified, the parent resource is searched for in the same namespace as UDPRoute.
		gateway, err := u.findGateway(ctx, udpRoute, parentRef)
		if err != nil {
			klog.Errorf("not found gateway %s/%s", string(*parentRef.Namespace), string(parentRef.Name))
			return err
		}

		if len(gateway.Status.Addresses) == 0 {
			return fmt.Errorf("gateway %s/%s has no addresses assigned", gateway.Namespace, gateway.Name)
		}

		listener := u.findListener(gateway, parentRef)
		if listener != nil {
			for _, rule := range udpRoute.Spec.Rules {
				for _, backendRef := range rule.BackendRefs {
					err := u.reconcileService(ctx, backendRef, udpRoute, gateway, listener)
					if err != nil {
						klog.Errorf("failure reconcileService. err: %v", err)
						return err
					}
				}
			}
		} else {
			klog.Infof("Unknown Listener on gateway %s/%s", gateway.Namespace, gateway.Name)
		}
	}

	klog.Infof("UDPRoute %s is reconciled.", udpRoute.Name)
	return nil
}

func (u *UDPRouteManager) deleteUDPRoute(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	svcList, err := u.kubeClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("get services list (ns: %s) failure. err: %v", namespace, err)
		return err
	}

	for _, svc := range svcList.Items {
		if svc.Annotations["gateway-api-controller"] == u.gatewayProvider &&
			svc.Annotations["parent-udp-route"] == name {

			err = u.kubeClient.CoreV1().Services(namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("delete service %s/%s failure. err: %v", namespace, svc.Name, err)
				return err
			}
			klog.Infof("service %s/%s is deleted by UDPRoute %s", namespace, svc.Name, name)
		}
	}
	klog.Infof("UDPRoute %s is deleted.", name)
	return nil
}

func (u *UDPRouteManager) findGateway(ctx context.Context, udpRoute *v1alpha2.UDPRoute, parentRef v1.ParentReference) (*v1.Gateway, error) {
	var gatewayNamespace string
	if parentRef.Namespace != nil {
		gatewayNamespace = string(*parentRef.Namespace)
	} else {
		gatewayNamespace = udpRoute.Namespace
	}

	gatewayName := string(parentRef.Name)
	gateway, err := u.sigsClient.GatewayV1().Gateways(gatewayNamespace).Get(ctx, gatewayName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("UDPRoute %s/%s parentRefs gateway %s/%s found failure. err: %v", udpRoute.Namespace, udpRoute.Name, gatewayNamespace, gatewayName, err)
		return nil, err
	}

	return gateway, nil
}

func (u *UDPRouteManager) findListener(gateway *v1.Gateway, parentRef v1.ParentReference) *v1.Listener {
	if parentRef.SectionName != nil {
		for i := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[i].Name == *parentRef.SectionName {
				return &gateway.Spec.Listeners[i]
			}
		}
	}

	return nil
}

func (u *UDPRouteManager) reconcileService(ctx context.Context, backendRef v1.BackendRef, udpRoute *v1alpha2.UDPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	serviceBehaviour := udpRoute.Labels["serviceBehaviour"]
	if serviceBehaviour == "" {
		serviceBehaviour = udpRouteRuleServiceCreate
	}

	var serviceNamespace string
	if backendRef.Namespace != nil {
		serviceNamespace = string(*backendRef.Namespace)
	} else {
		serviceNamespace = udpRoute.Namespace
	}

	service, err := u.kubeClient.CoreV1().Services(serviceNamespace).Get(ctx, string(backendRef.Name), metav1.GetOptions{})
	switch serviceBehaviour {
	case udpRouteRuleServiceCreate:
		isUpdated := false
		if err == nil {
			gatewayController, isGC := service.Annotations["gateway-api-controller"]
			parentUDPRouteName, isParent := service.Annotations["parent-udp-route"]
			if isGC && isParent && gatewayController == u.gatewayProvider && parentUDPRouteName == udpRoute.Name {
				klog.Infof("service %s/%s is already created by UDPRoute %s", service.Namespace, service.Name, udpRoute.Name)
				isUpdated = true
			} else {
				return fmt.Errorf("unable to create service %s/%s. it already exists", serviceNamespace, string(backendRef.Name))
			}
		} else if !errors.IsNotFound(err) {
			klog.Errorf("failed to get Service %s/%s. err: %v", serviceNamespace, string(backendRef.Name), err)
			return err
		}

		if isUpdated {
			// If Service is updated
			err = u.updateService(ctx, *service, backendRef, udpRoute, gateway, listener)
		} else {
			// If Service doesn't exist, create
			_, err = u.createService(ctx, serviceNamespace, backendRef, udpRoute, gateway, listener)
		}
		if err != nil {
			klog.Errorf("failed to create new Service %s/%s. err: %v", serviceNamespace, string(backendRef.Name), err)
			return err
		}

	case udpRouteRuleServiceDuplicate:
		if err != nil {
			klog.Errorf("not found service %s/%s so UDPRoute %s can't duplicate service", serviceNamespace, string(backendRef.Name), udpRoute.Name)
			return err
		}
		// No error means that the service exists.. lets copy it and create our own
		if err := u.duplicateService(ctx, service, backendRef, udpRoute, gateway, listener); err != nil {
			klog.Errorf("failed to create duplicated service %s/%s by UDPRoute %s", service.Namespace, service.Name, udpRoute.Name)
			return err
		}

	case udpRouteRuleServiceUpdate:
		if err != nil {
			klog.Errorf("not found service %s/%s so UDPRoute %s can't update service", serviceNamespace, string(backendRef.Name), udpRoute.Name)
			return err
		}

		if err := u.updateService(ctx, *service, backendRef, udpRoute, gateway, listener); err != nil {
			klog.Errorf("failed to update service %s/%s by UDPRoute %s", service.Namespace, service.Name, udpRoute.Name)
			return err
		}
	default:
		return fmt.Errorf("unknown service action %s", serviceBehaviour)
	}

	klog.Infof("service %s/%s is reconciled by UDPRoute %s", service.Namespace, service.Name, udpRoute.Name)

	return nil
}

func (u *UDPRouteManager) createService(ctx context.Context, serviceNamespace string, backendRef v1.BackendRef, udpRoute *v1alpha2.UDPRoute, gateway *v1.Gateway, listener *v1.Listener) (*corev1.Service, error) {
	newService := corev1.Service{}
	newService.Name = string(backendRef.Name)
	newService.Namespace = serviceNamespace

	// Initialise the Annotations
	newService.SetAnnotations(udpRoute.Annotations)
	if newService.Annotations == nil {
		newService.Annotations = map[string]string{}
	}
	newService.Annotations["gateway-api-controller"] = u.gatewayProvider
	newService.Annotations["parent-udp-route"] = udpRoute.Name

	// Initialise the Labels
	newService.SetLabels(map[string]string{
		"ipam-address":   gateway.Status.Addresses[0].Value,
		"implementation": implementation,
	})
	loadBalancerClass := u.gatewayProvider
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	newService.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			Protocol:   corev1.ProtocolUDP,
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}
	if udpRoute.Labels != nil {
		if newService.Spec.Selector == nil {
			newService.Spec.Selector = map[string]string{}
		}
		key, isKey := udpRoute.Labels["selectorkey"]
		value, isValue := udpRoute.Labels["selectorvalue"]
		if isKey && isValue {
			newService.Spec.Selector[key] = value
		}
	}
	svc, err := u.kubeClient.CoreV1().Services(serviceNamespace).Create(ctx, &newService, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return svc, nil
}

func (u *UDPRouteManager) duplicateService(ctx context.Context, service *corev1.Service, backendRef v1.BackendRef, udpRoute *v1alpha2.UDPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	newService := service.DeepCopy()
	newService.Name = string(backendRef.Name) + "-gw-api"
	_, err := u.kubeClient.CoreV1().Services(service.Namespace).Get(ctx, newService.Name, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("unable to duplicate service %s/%s. it already exists", service.Namespace, newService.Name)
	}

	if !errors.IsNotFound(err) {
		return err
	}

	newService.Namespace = service.Namespace
	newService.ResourceVersion = ""
	newService.Spec.ClusterIP = ""
	newService.Spec.ClusterIPs = []string{}
	// Initialise the Annotations
	newService.SetAnnotations(udpRoute.Annotations)
	if newService.Annotations == nil {
		newService.Annotations = map[string]string{}
	}
	newService.Annotations["gateway-api-controller"] = u.gatewayProvider
	newService.Annotations["parent-udp-route"] = udpRoute.Name
	// Initialise the Labels
	if newService.Labels == nil {
		newService.Labels = map[string]string{}
	}
	newService.Labels["ipam-address"] = gateway.Status.Addresses[0].Value
	newService.Labels["implementation"] = implementation
	// Set service configuration
	loadBalancerClass := u.gatewayProvider
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	newService.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			Protocol:   corev1.ProtocolUDP,
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}

	if _, err := u.kubeClient.CoreV1().Services(newService.Namespace).Create(ctx, newService, metav1.CreateOptions{}); err != nil {
		return err
	}
	return nil
}

func (u *UDPRouteManager) updateService(ctx context.Context, service corev1.Service, backendRef v1.BackendRef, udpRoute *v1alpha2.UDPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	// update service annotations
	for key := range service.Annotations {
		if strings.Contains(key, "loxilb.io") {
			if _, isOk := udpRoute.Annotations[key]; !isOk {
				delete(service.Annotations, key)
			}
		}
	}
	for key, value := range udpRoute.Annotations {
		service.Annotations[key] = value
	}
	service.Annotations["gateway-api-controller"] = u.gatewayProvider
	service.Annotations["parent-udp-route"] = udpRoute.Name

	// Set service configuration
	if *service.Spec.LoadBalancerClass != u.gatewayProvider {
		loadBalancerClass := u.gatewayProvider
		service.Spec.LoadBalancerClass = &loadBalancerClass
	}
	service.Spec.Type = corev1.ServiceTypeLoadBalancer
	service.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	service.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			Protocol:   corev1.ProtocolUDP,
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}
	if service.Labels == nil {
		service.Labels = map[string]string{}
	}
	service.Labels["ipam-address"] = gateway.Status.Addresses[0].Value
	service.Labels["implementation"] = implementation

	if _, err := u.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, &service, metav1.UpdateOptions{}); err != nil {
		return err
	}

	return nil
}
