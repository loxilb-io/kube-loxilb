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
	tcpRouteMgrName              = "TCPRouteManager"
	tcpRouteRuleServiceCreate    = "create"
	tcpRouteRuleServiceDuplicate = "duplicate"
	tcpRouteRuleServiceUpdate    = "update"
)

type TCPRouteManager struct {
	kubeClient           clientset.Interface
	sigsClient           sigsclientset.Interface
	networkConfig        *config.NetworkConfig
	gatewayProvider      string
	tcpRouteInformer     sigs_v1alpha2_informer.TCPRouteInformer
	tcpRouteLister       sigs_v1alpha2_lister.TCPRouteLister
	tcpRouteListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

type TCPRouteQueueEntry struct {
	namespace string
	name      string
}

func NewTCPRouteManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	sigsInformerFactory externalversions.SharedInformerFactory) *TCPRouteManager {

	tcpRouteInformer := sigsInformerFactory.Gateway().V1alpha2().TCPRoutes()
	manager := &TCPRouteManager{
		kubeClient:           kubeClient,
		sigsClient:           sigsClient,
		networkConfig:        networkConfig,
		gatewayProvider:      networkConfig.LoxilbGatewayClass,
		tcpRouteInformer:     tcpRouteInformer,
		tcpRouteLister:       tcpRouteInformer.Lister(),
		tcpRouteListerSynced: tcpRouteInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "gatewayClass"),
	}

	manager.tcpRouteInformer.Informer().AddEventHandler(
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

func (tr *TCPRouteManager) enqueueObject(obj interface{}) {
	tcpRoute, ok := obj.(*v1alpha2.TCPRoute)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		tcpRoute, ok = obj.(*v1alpha2.TCPRoute)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	tr.queue.Add(TCPRouteQueueEntry{tcpRoute.Namespace, tcpRoute.Name})
}

func (tr *TCPRouteManager) Run(stopCh <-chan struct{}) {
	defer tr.queue.ShutDown()

	klog.Infof("Starting %s", tcpRouteMgrName)
	defer klog.Infof("Shutting down %s", tcpRouteMgrName)

	if !cache.WaitForNamedCacheSync(
		tcpRouteMgrName,
		stopCh,
		tr.tcpRouteListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(tr.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (tr *TCPRouteManager) worker() {
	for tr.processNextWorkItem() {
	}
}

func (tr *TCPRouteManager) processNextWorkItem() bool {
	obj, quit := tr.queue.Get()
	if quit {
		return false
	}
	defer tr.queue.Done(obj)

	if key, ok := obj.(TCPRouteQueueEntry); !ok {
		tr.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := tr.reconcile(key.namespace, key.name); err == nil {
		tr.queue.Forget(obj)
	} else {
		tr.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing tcpRoute %s/%s, requeuing. Error: %v", key.namespace, key.name, err)
	}
	return true
}

func (tr *TCPRouteManager) reconcile(namespace, name string) error {
	tcpRoute, err := tr.tcpRouteLister.TCPRoutes(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return tr.deleteTCPRoute(namespace, name)
		}
		return err
	}

	return tr.createTCPRoute(tcpRoute)
}

func (tr *TCPRouteManager) createTCPRoute(tcpRoute *v1alpha2.TCPRoute) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// find all parent resources
	for _, parentRef := range tcpRoute.Spec.ParentRefs {
		// If no namespace is specified, the parent resource is searched for in the same namespace as TCPRoute.
		gateway, err := tr.findGateway(ctx, tcpRoute, parentRef)
		if err != nil {
			klog.Errorf("not found gateway %s/%s", string(*parentRef.Namespace), string(parentRef.Name))
			return err
		}

		if len(gateway.Status.Addresses) == 0 {
			return fmt.Errorf("gateway %s/%s has no addresses assigned", gateway.Namespace, gateway.Name)
		}

		listener := tr.findListener(gateway, parentRef)
		if listener != nil {
			for _, rule := range tcpRoute.Spec.Rules {
				for _, backendRef := range rule.BackendRefs {
					err := tr.reconcileService(ctx, backendRef, tcpRoute, gateway, listener)
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

	klog.Infof("TCPRoute %s is reconciled.", tcpRoute.Name)
	return nil
}

func (tr *TCPRouteManager) deleteTCPRoute(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	svcList, err := tr.kubeClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("get services list (ns: %s) failure. err: %v", namespace, err)
		return err
	}

	for _, svc := range svcList.Items {
		if svc.Annotations["gateway-api-controller"] == tr.gatewayProvider &&
			svc.Annotations["parent-tcp-route"] == name {

			err = tr.kubeClient.CoreV1().Services(namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("delete service %s/%s failure. err: %v", namespace, svc.Name, err)
				return err
			}
			klog.Infof("service %s/%s is deleted by TCPRoute %s", namespace, svc.Name, name)
		}
	}
	klog.Infof("TCPRoute %s is deleted.", name)
	return nil
}

func (tr *TCPRouteManager) findGateway(ctx context.Context, tcpRoute *v1alpha2.TCPRoute, parentRef v1.ParentReference) (*v1.Gateway, error) {
	var gatewayNamespace string
	if parentRef.Namespace != nil {
		gatewayNamespace = string(*parentRef.Namespace)
	} else {
		gatewayNamespace = tcpRoute.Namespace
	}

	gatewayName := string(parentRef.Name)
	gateway, err := tr.sigsClient.GatewayV1().Gateways(gatewayNamespace).Get(ctx, gatewayName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("TCPRoute %s/%s parentRefs gateway %s/%s found failure. err: %v", tcpRoute.Namespace, tcpRoute.Name, gatewayNamespace, gatewayName, err)
		return nil, err
	}

	return gateway, nil
}

func (tr *TCPRouteManager) findListener(gateway *v1.Gateway, parentRef v1.ParentReference) *v1.Listener {
	if parentRef.SectionName != nil {
		for i := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[i].Name == *parentRef.SectionName {
				return &gateway.Spec.Listeners[i]
			}
		}
	}

	return nil
}

func (tr *TCPRouteManager) reconcileService(ctx context.Context, backendRef v1.BackendRef, tcpRoute *v1alpha2.TCPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	serviceBehaviour := tcpRoute.Labels["serviceBehaviour"]
	if serviceBehaviour == "" {
		serviceBehaviour = tcpRouteRuleServiceCreate
	}

	var serviceNamespace string
	if backendRef.Namespace != nil {
		serviceNamespace = string(*backendRef.Namespace)
	} else {
		serviceNamespace = tcpRoute.Namespace
	}

	service, err := tr.kubeClient.CoreV1().Services(serviceNamespace).Get(ctx, string(backendRef.Name), metav1.GetOptions{})
	switch serviceBehaviour {
	case tcpRouteRuleServiceCreate:
		isUpdated := false
		if err == nil {
			gatewayController, isGC := service.Annotations["gateway-api-controller"]
			parentTcpRouteName, isParent := service.Annotations["parent-tcp-route"]
			if isGC && isParent && gatewayController == tr.gatewayProvider && parentTcpRouteName == tcpRoute.Name {
				klog.Infof("service %s/%s is already created by TCPRoute %s", service.Namespace, service.Name, tcpRoute.Name)
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
			err = tr.updateService(ctx, *service, backendRef, tcpRoute, gateway, listener)
		} else {
			// If Service doesn't exist, create
			_, err = tr.createService(ctx, serviceNamespace, backendRef, tcpRoute, gateway, listener)
		}
		if err != nil {
			klog.Errorf("failed to create new Service %s/%s. err: %v", serviceNamespace, string(backendRef.Name), err)
			return err
		}

	case tcpRouteRuleServiceDuplicate:
		if err != nil {
			klog.Errorf("not found service %s/%s so TCPRoute %s can't duplicate service", serviceNamespace, string(backendRef.Name), tcpRoute.Name)
			return err
		}
		// No error means that the service exists.. lets copy it and create our own
		if err := tr.duplicateService(ctx, service, backendRef, tcpRoute, gateway, listener); err != nil {
			klog.Errorf("failed to create duplicated service %s/%s by TCPRoute %s", service.Namespace, service.Name, tcpRoute.Name)
			return err
		}

	case tcpRouteRuleServiceUpdate:
		if err != nil {
			klog.Errorf("not found service %s/%s so TCPRoute %s can't update service", serviceNamespace, string(backendRef.Name), tcpRoute.Name)
			return err
		}

		if err := tr.updateService(ctx, *service, backendRef, tcpRoute, gateway, listener); err != nil {
			klog.Errorf("failed to update service %s/%s by TCPRoute %s", service.Namespace, service.Name, tcpRoute.Name)
			return err
		}
	default:
		return fmt.Errorf("unknown service action %s", serviceBehaviour)
	}

	klog.Infof("service %s/%s is reconciled by TCPRoute %s", service.Namespace, service.Name, tcpRoute.Name)

	return nil
}

func (tr *TCPRouteManager) createService(ctx context.Context, serviceNamespace string, backendRef v1.BackendRef, tcpRoute *v1alpha2.TCPRoute, gateway *v1.Gateway, listener *v1.Listener) (*corev1.Service, error) {
	newService := corev1.Service{}
	newService.Name = string(backendRef.Name)
	newService.Namespace = serviceNamespace

	// Initialise the Annotations
	newService.SetAnnotations(tcpRoute.Annotations)
	if newService.Annotations == nil {
		newService.Annotations = map[string]string{}
	}
	newService.Annotations["gateway-api-controller"] = tr.gatewayProvider
	newService.Annotations["parent-tcp-route"] = tcpRoute.Name

	// Initialise the Labels
	newService.SetLabels(map[string]string{
		"ipam-address":   gateway.Status.Addresses[0].Value,
		"implementation": implementation,
	})
	loadBalancerClass := tr.gatewayProvider
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	newService.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}
	if tcpRoute.Labels != nil {
		if newService.Spec.Selector == nil {
			newService.Spec.Selector = map[string]string{}
		}
		key, isKey := tcpRoute.Labels["selectorkey"]
		value, isValue := tcpRoute.Labels["selectorvalue"]
		if isKey && isValue {
			newService.Spec.Selector[key] = value
		}
	}
	svc, err := tr.kubeClient.CoreV1().Services(serviceNamespace).Create(ctx, &newService, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return svc, nil
}

func (tr *TCPRouteManager) duplicateService(ctx context.Context, service *corev1.Service, backendRef v1.BackendRef, tcpRoute *v1alpha2.TCPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	newService := service.DeepCopy()
	newService.Name = string(backendRef.Name) + "-gw-api"
	_, err := tr.kubeClient.CoreV1().Services(service.Namespace).Get(ctx, newService.Name, metav1.GetOptions{})
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
	newService.SetAnnotations(tcpRoute.Annotations)
	if newService.Annotations == nil {
		newService.Annotations = map[string]string{}
	}
	newService.Annotations["gateway-api-controller"] = tr.gatewayProvider
	newService.Annotations["parent-tcp-route"] = tcpRoute.Name
	// Initialise the Labels
	if newService.Labels == nil {
		newService.Labels = map[string]string{}
	}
	newService.Labels["ipam-address"] = gateway.Status.Addresses[0].Value
	newService.Labels["implementation"] = implementation
	// Set service configuration
	loadBalancerClass := tr.gatewayProvider
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	newService.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}

	if _, err := tr.kubeClient.CoreV1().Services(newService.Namespace).Create(ctx, newService, metav1.CreateOptions{}); err != nil {
		return err
	}
	return nil
}

func (tr *TCPRouteManager) updateService(ctx context.Context, service corev1.Service, backendRef v1.BackendRef, tcpRoute *v1alpha2.TCPRoute, gateway *v1.Gateway, listener *v1.Listener) error {
	// update service annotations
	for key := range service.Annotations {
		if strings.Contains(key, "loxilb.io") {
			if _, isOk := tcpRoute.Annotations[key]; !isOk {
				delete(service.Annotations, key)
			}
		}
	}
	for key, value := range tcpRoute.Annotations {
		service.Annotations[key] = value
	}
	service.Annotations["gateway-api-controller"] = tr.gatewayProvider
	service.Annotations["parent-tcp-route"] = tcpRoute.Name

	// Set service configuration
	if *service.Spec.LoadBalancerClass != tr.gatewayProvider {
		loadBalancerClass := tr.gatewayProvider
		service.Spec.LoadBalancerClass = &loadBalancerClass
	}
	service.Spec.Type = corev1.ServiceTypeLoadBalancer
	service.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	service.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(listener.Port),
			TargetPort: intstr.FromInt(int(*backendRef.Port)),
		},
	}
	if service.Labels == nil {
		service.Labels = map[string]string{}
	}
	service.Labels["ipam-address"] = gateway.Status.Addresses[0].Value
	service.Labels["implementation"] = implementation

	if _, err := tr.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, &service, metav1.UpdateOptions{}); err != nil {
		return err
	}

	return nil
}
