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

package bgppolicyapply

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/bgp-client/clientset/versioned"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/bgp-client/informers/externalversions/bgppolicyapply/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/bgp-client/listers/bgppolicyapply/v1"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/bgppolicyapply/v1"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "BGPPolicyApplyService"
	defaultWorkers = 4
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	kubeClient                 kubernetes.Interface
	crdClient                  versioned.Interface
	BGPPolicyApplyInformer     crdInformer.BGPPolicyApplyServiceInformer
	BGPPolicyApplyLister       crdLister.BGPPolicyApplyServiceLister
	BGPPolicyApplyListerSynced cache.InformerSynced
	queue                      workqueue.RateLimitingInterface
	loxiClients                *api.LoxiClientPool
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewBGPPolicyApplyManager(
	kubeClient kubernetes.Interface,
	crdClient versioned.Interface,
	networkConfig *config.NetworkConfig,
	BGPPolicyApplyInformer crdInformer.BGPPolicyApplyServiceInformer,
	loxiClients *api.LoxiClientPool,
) *Manager {

	manager := &Manager{

		kubeClient:                 kubeClient,
		crdClient:                  crdClient,
		BGPPolicyApplyInformer:     BGPPolicyApplyInformer,
		BGPPolicyApplyLister:       BGPPolicyApplyInformer.Lister(),
		BGPPolicyApplyListerSynced: BGPPolicyApplyInformer.Informer().HasSynced,
		loxiClients:                loxiClients,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "BGPPolicyApply"),
	}
	BGPPolicyApplyInformer.Informer().AddEventHandler(
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
	)

	return manager
}

func (m *Manager) enqueueService(obj interface{}) {
	lb, ok := obj.(*crdv1.BGPPolicyApplyService)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.BGPPolicyApplyService)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-BGPPolicyApplyService object: %v", deletedState.Obj)
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
		m.BGPPolicyApplyListerSynced) {
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
	if bgpdf, ok := obj.(*crdv1.BGPPolicyApplyService); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncBGPPolicyApplyService(bgpdf); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD BGPPolicyApplyService %s, requeuing. Error: %v", bgpdf.Name, err)
	}

	// fmt.Printf("obj: %v\n", obj)
	// fmt.Printf("m.queue: %v\n", m.queue)
	return true
}

func (m *Manager) syncBGPPolicyApplyService(bgpdf *crdv1.BGPPolicyApplyService) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing BGPPolicyApplyService %s. (%v)", bgpdf.Name, time.Since(startTime))
	}()
	_, err := m.BGPPolicyApplyLister.Get(bgpdf.Name)
	if err != nil {
		return m.deleteBGPPolicyApplyService(bgpdf)
	}
	return m.addBGPPolicyApplyService(bgpdf)
}

func (m *Manager) addBGPPolicyApplyService(bgpdf *crdv1.BGPPolicyApplyService) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	klog.Infof("bgpdf.Spec.NeighIPAddress: %v\n", bgpdf.Spec.NeighIPAddress)
	klog.Infof("bgpdf.Spec.Policies: %v\n", bgpdf.Spec.Policies)
	klog.Infof("bgpdf.Spec.PolicyType: %v\n", bgpdf.Spec.PolicyType)
	klog.Infof("bgpdf.Spec.RouteAction: %v\n", bgpdf.Spec.RouteAction)

	var errChList []chan error
	for _, client := range m.loxiClients.Clients {
		ch := make(chan error)
		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.BGPPolicyApply().CreateBGPPolicyApply(ctx, &bgpdf.Spec); err != nil {
				if !strings.Contains(err.Error(), "exist") {
					klog.Errorf("failed to create BGPPolicyApply(%s) :%v", c.Url, err)
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
		klog.Errorf("failed to add BGPPolicyApply")
		return fmt.Errorf("failed to add loxiLB BGPPolicyApply")
	}

	return nil
}

func (m *Manager) deleteBGPPolicyApplyService(bgpdf *crdv1.BGPPolicyApplyService) error {

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	klog.Infof("bgpdf.Spec.NeighIPAddress: %v\n", bgpdf.Spec.NeighIPAddress)
	var errChList []chan error
	for _, client := range m.loxiClients.Clients {
		ch := make(chan error)
		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.BGPPolicyApply().DeleteBGPPolicyApply(ctx, &bgpdf.Spec); err != nil {
				if !strings.Contains(err.Error(), "exist") {
					klog.Errorf("failed to delete BGPPolicyApply(%s) :%v", c.Url, err)
				} else {
					err = nil
				}
			}
			h <- err
		}(client, ch)

		errChList = append(errChList, ch)
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
