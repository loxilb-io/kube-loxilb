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

package bgppolicydefinition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/bgp-client/clientset/versioned"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/bgp-client/informers/externalversions/bgppolicydefinition/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/bgp-client/listers/bgppolicydefinition/v1"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/bgppolicydefinition/v1"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "BGPPolicyDefinitionService"
	defaultWorkers = 4
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	kubeClient                      kubernetes.Interface
	crdClient                       versioned.Interface
	BGPPolicyDefinitionInformer     crdInformer.BGPPolicyDefinitionServiceInformer
	BGPPolicyDefinitionLister       crdLister.BGPPolicyDefinitionServiceLister
	BGPPolicyDefinitionListerSynced cache.InformerSynced
	queue                           workqueue.RateLimitingInterface
	loxiClients                     *api.LoxiClientPool
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewBGPPolicyDefinitionManager(
	kubeClient kubernetes.Interface,
	crdClient versioned.Interface,
	networkConfig *config.NetworkConfig,
	BGPPolicyDefinitionInformer crdInformer.BGPPolicyDefinitionServiceInformer,
	loxiClients *api.LoxiClientPool,
) *Manager {

	manager := &Manager{

		kubeClient:                      kubeClient,
		crdClient:                       crdClient,
		BGPPolicyDefinitionInformer:     BGPPolicyDefinitionInformer,
		BGPPolicyDefinitionLister:       BGPPolicyDefinitionInformer.Lister(),
		BGPPolicyDefinitionListerSynced: BGPPolicyDefinitionInformer.Informer().HasSynced,
		loxiClients:                     loxiClients,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "BGPPolicyDefinition"),
	}
	BGPPolicyDefinitionInformer.Informer().AddEventHandler(
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
	lb, ok := obj.(*crdv1.BGPPolicyDefinitionService)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.BGPPolicyDefinitionService)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-BGPPolicyDefinitionService object: %v", deletedState.Obj)
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
		m.BGPPolicyDefinitionListerSynced) {
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
	if bgpdf, ok := obj.(*crdv1.BGPPolicyDefinitionService); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncBGPPolicyDefinitionService(bgpdf); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD BGPPolicyDefinitionService %s, requeuing. Error: %v", bgpdf.Name, err)
	}

	// fmt.Printf("obj: %v\n", obj)
	// fmt.Printf("m.queue: %v\n", m.queue)
	return true
}

func (m *Manager) syncBGPPolicyDefinitionService(bgpdf *crdv1.BGPPolicyDefinitionService) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing BGPPolicyDefinitionService %s. (%v)", bgpdf.Name, time.Since(startTime))
	}()
	_, err := m.BGPPolicyDefinitionLister.Get(bgpdf.Name)
	if err != nil {
		return m.deleteBGPPolicyDefinitionService(bgpdf)
	}
	return m.addBGPPolicyDefinitionService(bgpdf)
}

func (m *Manager) addBGPPolicyDefinitionService(bgpdf *crdv1.BGPPolicyDefinitionService) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	klog.Infof("bgpdf.Spec.Name: %v\n", bgpdf.Spec.Name)
	fmt.Printf("bgpdf.Spec: %v\n", bgpdf.Spec)
	var errChList []chan error
	for _, client := range m.loxiClients.Clients {
		ch := make(chan error)
		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.BGPPolicyDefinition().CreateBGPPolicyDefinition(ctx, &bgpdf.Spec); err != nil {
				if !strings.Contains(err.Error(), "exist") {
					klog.Errorf("failed to create BGPPolicyDefinition(%s) :%v", c.Url, err)
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
		klog.Errorf("failed to add BGPPolicyDefinition")
		return fmt.Errorf("failed to add loxiLB BGPPolicyDefinition")
	}

	return nil
}

func (m *Manager) deleteBGPPolicyDefinitionService(bgpdf *crdv1.BGPPolicyDefinitionService) error {
	var errChList []chan error
	for _, loxiClient := range m.loxiClients.Clients {
		ch := make(chan error)
		errChList = append(errChList, ch)

		go func(client *api.LoxiClient, ch chan error) {
			klog.Infof("called loxilb API: delete bgpdf rule %v", bgpdf.Spec)
			ch <- client.BGPPolicyDefinition().DeleteBGPPolicyDefinition(context.Background(), bgpdf.Spec.Name)
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
