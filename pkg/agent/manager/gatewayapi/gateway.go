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
	"path"
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

	queue workqueue.RateLimitingInterface
}

func NewGatewayManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	externalIPPool *ippool.IPPool,
	sigsInformerFactory externalversions.SharedInformerFactory) *GatewayManager {

	gatewayInformer := sigsInformerFactory.Gateway().V1().Gateways()
	manager := &GatewayManager{
		kubeClient:          kubeClient,
		sigsClient:          sigsClient,
		networkConfig:       networkConfig,
		externalIPPool:      externalIPPool,
		gatewayInformer:     gatewayInformer,
		gatewayLister:       gatewayInformer.Lister(),
		gatewayListerSynced: gatewayInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "gateway"),
	}

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
		gw, ok = obj.(*v1.Gateway)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	gm.queue.Add(gw)
}

func (gm *GatewayManager) Run(stopCh <-chan struct{}) {
	defer gm.queue.ShutDown()

	klog.Infof("Starting %s", gwMgrName)
	defer klog.Infof("Shutting down %s", gwMgrName)

	if !cache.WaitForNamedCacheSync(
		gwMgrName,
		stopCh,
		gm.gatewayListerSynced) {
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
			return gm.deleteGateway(entry.Namespace, entry.Name, entry.Status.Addresses, entry.Annotations["ident-ipam"])
		}
		return err
	}
	return gm.createGateway(gw)
}

func (gm *GatewayManager) deleteGateway(ns, name string, addresses []v1.GatewayStatusAddress, identIPAM string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	ipPool := gm.externalIPPool
	for _, address := range addresses {
		ipPool.ReturnIPAddr(address.Value, identIPAM)
	}

	err := gm.deleteIngressLbService(ctx, ns, name)
	klog.Infof("gateway %s/%s is deleted.", ns, name)

	return err
}

func (gm *GatewayManager) createGateway(gw *v1.Gateway) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	_, err := gm.sigsClient.GatewayV1().GatewayClasses().Get(ctx, string(gw.Spec.GatewayClassName), metav1.GetOptions{})
	if err != nil {
		klog.Errorf("gateway %s/%s has wrong gatewayClass %s. err: %v", gw.Namespace, gw.Name, string(gw.Spec.GatewayClassName), err)
		return nil
	}

	updatedGw := gw

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
		gw.Spec.Addresses = append(gw.Spec.Addresses, v1.GatewayAddress{Value: newIP.String()})
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

	// update Gateway status. If fail, no return error
	for _, specAddress := range updatedGw.Spec.Addresses {
		found := false
		for _, statusAddress := range updatedGw.Status.Addresses {
			if statusAddress.Value == specAddress.Value {
				found = true
				break
			}
		}
		if !found {
			updatedGw.Status.Addresses = append(updatedGw.Status.Addresses, v1.GatewayStatusAddress(specAddress))
		}
	}

	for i := range updatedGw.Status.Conditions {
		if updatedGw.Status.Conditions[i].Type == string(v1.GatewayConditionAccepted) {
			updatedGw.Status.Conditions[i].Status = metav1.ConditionTrue
			updatedGw.Status.Conditions[i].Message = "managed by kube-loxilb"
			updatedGw.Status.Conditions[i].Reason = string(v1.GatewayReasonAccepted)
			updatedGw.Status.Conditions[i].LastTransitionTime = metav1.Now()
			klog.V(4).Infof("gateway %s is accepted.", updatedGw.Name)

		} else if updatedGw.Status.Conditions[i].Type == string(v1.GatewayConditionProgrammed) {
			updatedGw.Status.Conditions[i].Status = metav1.ConditionTrue
			updatedGw.Status.Conditions[i].Message = "managed by kube-loxilb"
			updatedGw.Status.Conditions[i].Reason = string(v1.GatewayReasonProgrammed)
			updatedGw.Status.Conditions[i].LastTransitionTime = metav1.Now()
			klog.V(4).Infof("gateway %s is programmed.", updatedGw.Name)
		}
	}
	newGw, err := gm.sigsClient.GatewayV1().Gateways(updatedGw.Namespace).UpdateStatus(ctx, updatedGw, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("gateway %s/%s unable to update gateway status. err: %v", updatedGw.Namespace, updatedGw.Name, err)
	}

	svc, err := gm.createIngressLbService(ctx, newGw)
	if err != nil {
		klog.Errorf("gateway %s/%s failed to create service. err: %v", updatedGw.Namespace, updatedGw.Name, err)
		return err
	}

	if svc != nil {
		klog.Infof("gateway %s/%s is assigned an IP %v and created a service %s/%s.", updatedGw.Namespace, updatedGw.Name, updatedGw.Status.Addresses, svc.Namespace, svc.Name)
	} else {
		klog.Infof("gateway %s/%s is assigned an IP %v (no service created - no HTTP/HTTPS listeners).", updatedGw.Namespace, updatedGw.Name, updatedGw.Status.Addresses)
	}
	klog.Infof("gateway %s/%s is created.", updatedGw.Namespace, updatedGw.Name)
	return nil
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
// It extracts HTTP/HTTPS listeners from the Gateway spec and creates corresponding service ports.
// Service is only created if there are HTTP or HTTPS protocol listeners.
//
// Port mapping:
//   - Service Port: Uses the port specified in the listener (defaults to 80 for HTTP, 443 for HTTPS)
//   - TargetPort: Always uses standard ports (80 for HTTP, 443 for HTTPS)
//
// Returns:
//   - *corev1.Service: The created or existing Service, or nil if no HTTP/HTTPS listeners exist
//   - error: Any error encountered during service creation
func (gm *GatewayManager) createIngressLbService(ctx context.Context, gateway *v1.Gateway) (*corev1.Service, error) {
	newService := corev1.Service{}
	newService.Name = fmt.Sprintf("%s-ingress-service", gateway.Name)
	newService.Namespace = gateway.Namespace

	svc, err := gm.kubeClient.CoreV1().Services(newService.Namespace).Get(ctx, newService.Name, metav1.GetOptions{})
	if err == nil {
		return svc, nil
	}

	if len(gateway.Spec.Addresses) == 0 {
		return nil, fmt.Errorf("gateway has no external IP address")
	}

	// Extract HTTP/HTTPS listeners and their ports
	var servicePorts []corev1.ServicePort
	for _, listener := range gateway.Spec.Listeners {
		if listener.Protocol == v1.HTTPProtocolType {
			port := int32(httpPort) // default 80
			if listener.Port != 0 {
				port = int32(listener.Port)
			}
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name:       string(listener.Name),
				Port:       port,
				TargetPort: intstr.FromInt(httpPort), // Always use default HTTP port 80
				Protocol:   corev1.ProtocolTCP,
			})
		} else if listener.Protocol == v1.HTTPSProtocolType {
			port := int32(httpsPort) // default 443
			if listener.Port != 0 {
				port = int32(listener.Port)
			}
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name:       string(listener.Name),
				Port:       port,
				TargetPort: intstr.FromInt(httpsPort), // Always use default HTTPS port 443
				Protocol:   corev1.ProtocolTCP,
			})
		}
	}

	// If no HTTP/HTTPS listeners found, don't create service
	if len(servicePorts) == 0 {
		klog.Infof("gateway %s/%s has no HTTP/HTTPS listeners, skipping service creation", gateway.Namespace, gateway.Name)
		return nil, nil
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

	loadBalancerClass := gm.networkConfig.LoxilbLoadBalancerClass
	newService.Spec.Type = corev1.ServiceTypeLoadBalancer
	newService.Spec.LoadBalancerClass = &loadBalancerClass
	newService.Spec.LoadBalancerIP = gateway.Status.Addresses[0].Value
	newService.Spec.Ports = servicePorts

	if newService.Spec.Selector == nil {
		newService.Spec.Selector = map[string]string{}
	}
	newService.Spec.Selector["app"] = "loxilb-ingress"

	svc, err = gm.kubeClient.CoreV1().Services(newService.Namespace).Create(ctx, &newService, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return svc, nil
}
