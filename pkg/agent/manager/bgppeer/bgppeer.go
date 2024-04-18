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

package bgppeer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/loadbalancer"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/client/clientset/versioned"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/client/informers/externalversions/bgppeer/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/client/listers/bgppeer/v1"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/bgppeer/v1"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "BGPPeerServiceManager"
	defaultWorkers = 4
	resyncPeriod   = 60 * time.Second
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	kubeClient          kubernetes.Interface
	crdClient           versioned.Interface
	bgpPeerInformer     crdInformer.BGPPeerServiceInformer
	bgpPeerLister       crdLister.BGPPeerServiceLister
	bgpPeerListerSynced cache.InformerSynced
	queue               workqueue.RateLimitingInterface
	lbManager           *loadbalancer.Manager
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewBGPPeerManager(
	kubeClient kubernetes.Interface,
	crdClient versioned.Interface,
	networkConfig *config.NetworkConfig,
	bgpPeerInformer crdInformer.BGPPeerServiceInformer,
	lbManager *loadbalancer.Manager,
) *Manager {

	manager := &Manager{

		kubeClient:          kubeClient,
		crdClient:           crdClient,
		bgpPeerInformer:     bgpPeerInformer,
		bgpPeerLister:       bgpPeerInformer.Lister(),
		bgpPeerListerSynced: bgpPeerInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
	}

	bgpPeerInformer.Informer().AddEventHandlerWithResyncPeriod(
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
	lb, ok := obj.(*crdv1.BGPPeerService)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.BGPPeerService)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-BGPPeerService object: %v", deletedState.Obj)
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
		m.bgpPeerListerSynced) {
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

	if lb, ok := obj.(*crdv1.BGPPeerService); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncBGPPeerService(lb); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD BGPPeerService %s, requeuing. Error: %v", lb.Name, err)
	}
	return true
}

func (m *Manager) syncBGPPeerService(lb *crdv1.BGPPeerService) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing BGPPeerService %s. (%v)", lb.Name, time.Since(startTime))
	}()
	_, err := m.bgpPeerLister.Get(lb.Name)
	if err != nil {
		return m.deleteBGPPeerService(lb)
	}
	return m.addBGPPeerService(lb)
}

func (m *Manager) updateBGPPeerService() error {
	bgp := crdv1.BGPPeerService{}
	//m.crdClient.BgppeerV1().BGPPeerServices().Update(context.TODO(), &bgp, v1.UpdateOptions{})
	fmt.Println(m.bgpPeerLister.Get(bgp.Name))

	klog.Infof("IPAddress: %v", bgp.Spec.IPAddress)
	klog.Infof("RemoteAs: %v", bgp.Spec.RemoteAs)
	klog.Infof("RemotePort: %v", bgp.Spec.RemotePort)

	return nil
}

func (m *Manager) addBGPPeerService(lb *crdv1.BGPPeerService) error {
	// TODO: This code should be made into a function so that it can be reused.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	klog.Infof("IPAddress: %v", lb.Spec.IPAddress)
	klog.Infof("RemoteAs: %v", lb.Spec.RemoteAs)
	klog.Infof("RemotePort: %v", lb.Spec.RemotePort)

	var errChList []chan error
	for _, client := range m.lbManager.LoxiClients {
		ch := make(chan error)
		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.BGP().CreateNeigh(ctx, &lb.Spec); err != nil {
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

func (m *Manager) deleteBGPPeerService(lb *crdv1.BGPPeerService) error {
	var errChList []chan error
	for _, loxiClient := range m.lbManager.LoxiClients {
		ch := make(chan error)
		errChList = append(errChList, ch)

		go func(client *api.LoxiClient, ch chan error) {
			klog.Infof("called loxilb API: delete lb rule %v", lb.Spec)
			ch <- client.BGP().DeleteNeigh(context.Background(), lb.Spec.IPAddress, int(lb.Spec.RemoteAs))
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
