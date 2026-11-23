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
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
		// A delete the watch missed arrives as a tombstone. The GatewayClass
		// is the object it wraps, not the tombstone itself.
		gc, ok = deletedState.Obj.(*v1.GatewayClass)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-GatewayClass object: %v", deletedState.Obj)
			return
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

// updateGatewayClass - accept a GatewayClass of this controller.
//
// A GatewayClass starts with Accepted Unknown (Pending, "Waiting for
// controller"), as its CRD defaults it. Accepting it sets the whole condition:
// its reason and message too, which were left Pending while the status said
// True, and the generation it was accepted at. Its transition time moves only
// when its status does, and an accepted GatewayClass is not written again.
func (m *GatewayClassManager) updateGatewayClass(gc *v1.GatewayClass) error {
	if gc.Spec.ControllerName != v1.GatewayController(m.networkConfig.LoxilbGatewayClass) {
		return nil
	}
	// Providers of the Gateway API may need to pass parameters to their controller as part of the class definition.
	// This is done using the GatewayClass.spec.parametersRef field
	if gc.Spec.ParametersRef != nil {
		klog.V(4).Infof("gatewayClass %s has a parametersRef (kind %s), which is not read", gc.Name, gc.Spec.ParametersRef.Kind)
	}

	// A condition from a later generation than this copy's is left as it is,
	// as Gateway API asks: the copy is stale, and a later event brings the
	// newer one.
	if c := meta.FindStatusCondition(gc.Status.Conditions, string(v1.GatewayClassConditionStatusAccepted)); c != nil && c.ObservedGeneration > gc.Generation {
		return nil
	}

	// The lister's object is the informer's cache; the status is worked out
	// on a copy.
	updated := gc.DeepCopy()
	meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type:               string(v1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(v1.GatewayClassReasonAccepted),
		Message:            "accepted by kube-loxilb",
		ObservedGeneration: gc.Generation,
	})
	if reflect.DeepEqual(updated.Status, gc.Status) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()
	if _, err := m.sigsClient.GatewayV1().GatewayClasses().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("gatewayClass %s: unable to update status: %w", gc.Name, err)
	}
	klog.V(4).Infof("gatewayClass %s is accepted (generation %d)", gc.Name, gc.Generation)
	return nil
}
