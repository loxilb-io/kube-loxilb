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
)

const (
	gcMgrName = "GatewayClassManager"
)

type GatewayClassManager struct {
	kubeClient               clientset.Interface
	sigsClient               sigsclientset.Interface
	networkConfig            *config.NetworkConfig
	gatewayClassInformer     sigsinformer.GatewayClassInformer
	gatewayClassLister       sigslister.GatewayClassLister
	gatewayClassListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

func NewGatewayClassManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	sigsInformerFactory externalversions.SharedInformerFactory) *GatewayClassManager {

	gatewayClassInformer := sigsInformerFactory.Gateway().V1().GatewayClasses()
	manager := &GatewayClassManager{
		kubeClient:               kubeClient,
		sigsClient:               sigsClient,
		networkConfig:            networkConfig,
		gatewayClassInformer:     gatewayClassInformer,
		gatewayClassLister:       gatewayClassInformer.Lister(),
		gatewayClassListerSynced: gatewayClassInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "gatewayclass"),
	}

	manager.gatewayClassInformer.Informer().AddEventHandler(
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

func (m *GatewayClassManager) enqueueObject(obj interface{}) {
	gc, ok := obj.(*v1.GatewayClass)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		gc, ok = obj.(*v1.GatewayClass)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	m.queue.Add(gc.Name)
}

func (m *GatewayClassManager) Run(stopCh <-chan struct{}) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", gcMgrName)
	defer klog.Infof("Shutting down %s", gcMgrName)

	if !cache.WaitForNamedCacheSync(
		gcMgrName,
		stopCh,
		m.gatewayClassListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *GatewayClassManager) worker() {
	for m.processNextWorkItem() {
	}
}

func (m *GatewayClassManager) processNextWorkItem() bool {
	obj, quit := m.queue.Get()
	if quit {
		return false
	}
	defer m.queue.Done(obj)

	if key, ok := obj.(string); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.reconcile(key); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing gatewayClass %s, requeuing. Error: %v", key, err)
	}
	return true
}

func (m *GatewayClassManager) reconcile(name string) error {
	gc, err := m.gatewayClassLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return nil
		}
		return err
	}
	return m.updateGatewayClass(gc)
}

func (m *GatewayClassManager) updateGatewayClass(gc *v1.GatewayClass) error {
	// Providers of the Gateway API may need to pass parameters to their controller as part of the class definition.
	// This is done using the GatewayClass.spec.parametersRef field
	if gc.Spec.ParametersRef != nil {
		klog.Infof("gateway class has parametersRef(type: %s): ", gc.Spec.ParametersRef.Kind)
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	// A new GatewayClass will start with the Accepted condition set to False.
	// At this point the controller has not seen the configuration.
	// Once the controller has processed the configuration, the condition will be set to True
	if gc.Spec.ControllerName == v1.GatewayController(m.networkConfig.LoxilbGatewayClass) {
		for i := range gc.Status.Conditions {
			if gc.Status.Conditions[i].Type == "Accepted" {
				gc.Status.Conditions[i].Status = metav1.ConditionTrue
				gc.Status.Conditions[i].LastTransitionTime = metav1.Now()
				klog.V(4).Infof("gatewayClass %s is created and accepted.", gc.Name)
			}
		}
		_, err := m.sigsClient.GatewayV1().GatewayClasses().UpdateStatus(ctx, gc, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("unable to update gateway class. err: %v", err)
		}
	}

	return nil
}
