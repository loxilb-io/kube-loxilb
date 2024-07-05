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

type GatewayQueueEntry struct {
	namespace string
	name      string
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

	gm.queue.Add(GatewayQueueEntry{gw.Namespace, gw.Name})
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

	if key, ok := obj.(GatewayQueueEntry); !ok {
		gm.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := gm.reconcile(key.namespace, key.name); err == nil {
		gm.queue.Forget(obj)
	} else {
		gm.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing gateway %s/%s, requeuing. Error: %v", key.namespace, key.name, err)
	}
	return true
}

func (gm *GatewayManager) reconcile(namespace, name string) error {
	gw, err := gm.gatewayLister.Gateways(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return nil
		}
		return err
	}
	return gm.createGateway(gw)
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
		gw.Labels["ipam-address"] = newIP.String()
		gw.Labels["implementation"] = implementation
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
	_, err = gm.sigsClient.GatewayV1().Gateways(updatedGw.Namespace).UpdateStatus(ctx, updatedGw, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("gateway %s/%s unable to update gateway status. err: %v", updatedGw.Namespace, updatedGw.Name, err)
	}

	return nil
}
