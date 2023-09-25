/*
 * Copyright (c) 2023 NetLOX Inc
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

package multicluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/client/clientset/versioned"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/client/informers/externalversions/multiclusterlbservice/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/client/listers/multiclusterlbservice/v1"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/multiclusterlbservice/v1"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "MulticlusterLBServiceManager"
	defaultWorkers = 4
	resyncPeriod   = 60 * time.Second
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	crdClient                  versioned.Interface
	loxiClients                []*api.LoxiClient
	multiclusterLBInformer     crdInformer.MultiClusterLBServiceInformer
	multiclusterLBLister       crdLister.MultiClusterLBServiceLister
	multiclusterLBListerSynced cache.InformerSynced
	queue                      workqueue.RateLimitingInterface
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewMulticlusterLBManager(
	crdClient versioned.Interface,
	loxiClients []*api.LoxiClient,
	networkConfig *config.NetworkConfig,
	multiclusterLBInformer crdInformer.MultiClusterLBServiceInformer) *Manager {

	manager := &Manager{
		crdClient:                  crdClient,
		loxiClients:                loxiClients,
		multiclusterLBInformer:     multiclusterLBInformer,
		multiclusterLBLister:       multiclusterLBInformer.Lister(),
		multiclusterLBListerSynced: multiclusterLBInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
	}

	multiclusterLBInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(cur interface{}) {
				manager.enqueueService(cur)
			},
			UpdateFunc: func(old, cur interface{}) {
				manager.enqueueService(cur)
			},
			DeleteFunc: func(old interface{}) {
				manager.enqueueService(old)
			},
		},
		resyncPeriod,
	)

	return manager
}

func (m *Manager) enqueueService(obj interface{}) {
	lb, ok := obj.(*crdv1.MultiClusterLBService)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.MultiClusterLBService)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-MultiClusterLBService object: %v", deletedState.Obj)
		}
	}

	m.queue.Add(lb)
}

func (m *Manager) Run(stopCh <-chan struct{}, loxiLBLiveCh chan *api.LoxiClient, loxiLBPurgeCh chan *api.LoxiClient, masterEventCh <-chan bool) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(
		mgrName,
		stopCh,
		m.multiclusterLBListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *Manager) worker() {
	for m.processNextWorkItem() {
	}
}

func (m *Manager) processNextWorkItem() bool {
	obj, quit := m.queue.Get()
	if quit {
		return false
	}

	defer m.queue.Done(obj)

	if lb, ok := obj.(*crdv1.MultiClusterLBService); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncMulticlusterLBService(lb); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD MultiClusterLBService %s, requeuing. Error: %v", lb.Name, err)
	}
	return true
}

func (m *Manager) syncMulticlusterLBService(lb *crdv1.MultiClusterLBService) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing MulticlusterLBService %s. (%v)", lb.Name, time.Since(startTime))
	}()

	_, err := m.multiclusterLBLister.Get(lb.Name)
	if err != nil {
		return m.deleteMulticlusterLBService(lb)
	}
	return m.addMulticlusterLBService(lb)
}

func (m *Manager) addMulticlusterLBService(lb *crdv1.MultiClusterLBService) error {
	// TODO: This code should be made into a function so that it can be reused.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	klog.Infof("ExternalIP: %v", lb.Spec.Model.Service.ExternalIP)
	klog.Infof("Port: %v", lb.Spec.Model.Service.Port)
	klog.Infof("Protocol: %v", lb.Spec.Model.Service.Protocol)
	klog.Infof("Sel: %v", lb.Spec.Model.Service.Sel)
	klog.Infof("Mode: %v", lb.Spec.Model.Service.Mode)
	klog.Infof("BGP: %v", lb.Spec.Model.Service.BGP)
	klog.Infof("Monitor: %v", lb.Spec.Model.Service.Monitor)
	klog.Infof("Timeout: %v", lb.Spec.Model.Service.Timeout)
	klog.Infof("Block: %v", lb.Spec.Model.Service.Block)
	klog.Infof("Managed: %v", lb.Spec.Model.Service.Managed)
	klog.Infof("ProbeType: %v", lb.Spec.Model.Service.ProbeType)
	klog.Infof("ProbePort: %v", lb.Spec.Model.Service.ProbePort)
	klog.Infof("ProbeReq: %v", lb.Spec.Model.Service.ProbeReq)
	klog.Infof("ProbeResp: %v", lb.Spec.Model.Service.ProbeResp)

	var errChList []chan error
	for _, client := range m.loxiClients {
		ch := make(chan error)
		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.LoadBalancer().Create(ctx, &lb.Spec.Model); err != nil {
				if !strings.Contains(err.Error(), "exist") {
					klog.Errorf("failed to create load-balancer(%s) :%v", c.Url, err)
				} else {
					err = nil
				}
			}
			h <- err
		}(client, ch)

		errChList = append(errChList, ch)
	}

	isError := true
	for _, errCh := range errChList {
		err := <-errCh
		if err == nil {
			isError = false
		}
	}
	if isError {
		klog.Errorf("failed to add load-balancer")
		return fmt.Errorf("failed to add loxiLB loadBalancer")
	}

	return nil
}

func (m *Manager) deleteMulticlusterLBService(lb *crdv1.MultiClusterLBService) error {
	var errChList []chan error
	for _, loxiClient := range m.loxiClients {
		ch := make(chan error)
		errChList = append(errChList, ch)

		go func(client *api.LoxiClient, ch chan error) {
			klog.Infof("called loxilb API: delete lb rule %v", lb.Spec.Model)
			ch <- client.LoadBalancer().Delete(context.Background(), &lb.Spec.Model)
		}(loxiClient, ch)
	}

	isError := true
	errStr := ""
	for _, errCh := range errChList {
		err := <-errCh
		if err == nil {
			isError = false
			break
		} else {
			errStr = err.Error()
		}
	}
	if isError {
		return fmt.Errorf("failed to delete loxiLB LoadBalancer. Error: %v", errStr)
	}
	return nil
}
