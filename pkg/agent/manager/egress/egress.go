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

package egress

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/egress/v1"
	"github.com/loxilb-io/kube-loxilb/pkg/egress-client/clientset/versioned"
	egressCRDinformers "github.com/loxilb-io/kube-loxilb/pkg/egress-client/informers/externalversions"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/egress-client/informers/externalversions/egress/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/egress-client/listers/egress/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "LoxilbEgressManager"
	defaultWorkers = 4
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	kubeClient         kubernetes.Interface
	kubeExtClient      apiextensionclientset.Interface
	crdClient          versioned.Interface
	EgressInformer     crdInformer.EgressInformer
	EgressLister       crdLister.EgressLister
	EgressListerSynced cache.InformerSynced
	LoxiClients        *api.LoxiClientPool
	queue              workqueue.RateLimitingInterface
	ClientAliveCh      chan *api.LoxiClient
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewEgressManager(
	kubeClient kubernetes.Interface,
	kubeExtClient apiextensionclientset.Interface,
	crdClient versioned.Interface,
	networkConfig *config.NetworkConfig,
	EgressInformer crdInformer.EgressInformer,
	LoxiClients *api.LoxiClientPool,
) *Manager {

	manager := &Manager{
		kubeClient:         kubeClient,
		kubeExtClient:      kubeExtClient,
		crdClient:          crdClient,
		EgressInformer:     EgressInformer,
		EgressLister:       EgressInformer.Lister(),
		EgressListerSynced: EgressInformer.Informer().HasSynced,
		LoxiClients:        LoxiClients,
		ClientAliveCh:      make(chan *api.LoxiClient, 50),

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "Egress"),
	}

	EgressInformer.Informer().AddEventHandler(
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
	lb, ok := obj.(*crdv1.Egress)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.Egress)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Egress object: %v", deletedState.Obj)
		}
	}

	m.queue.Add(lb)
}

func (m *Manager) Run(stopCh <-chan struct{}) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(
		mgrName,
		stopCh,
		m.EgressListerSynced) {
		return
	}

	go m.manageLoxilbEgressLifeCycle(stopCh)

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *Manager) WaitForLoxiEgressCRDCreation(stopCh <-chan struct{}) {

	wait.PollImmediateUntil(time.Second*5,
		func() (bool, error) {
			_, err := m.kubeExtClient.ApiextensionsV1().CustomResourceDefinitions().
				Get(context.TODO(), "egresses.egress.loxilb.io", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			klog.Infof("loxilb-egress crd created")
			return true, nil
		},
		stopCh)
}

func (m *Manager) Start(informer egressCRDinformers.SharedInformerFactory, stopCh <-chan struct{}) {
	klog.Infof("Starting %s", mgrName)

	m.WaitForLoxiEgressCRDCreation(stopCh)
	informer.Start(stopCh)
	m.Run(stopCh)
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
	if egress, ok := obj.(*crdv1.Egress); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncEgress(egress); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD Egress %s, requeuing. Error: %v", egress.Name, err)
	}

	return true
}

func (m *Manager) syncEgress(egress *crdv1.Egress) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing Egress %s. (%v)", egress.Name, time.Since(startTime))
	}()

	_, err := m.EgressLister.Egresses(egress.Namespace).Get(egress.Name)
	if err != nil {
		return m.deleteEgress(egress)
	}
	return m.addEgress(egress)
}

func (m *Manager) addEgress(egress *crdv1.Egress) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// Create Loxilb firewall rule for egress
	klog.V(4).Infof("Adding Egress %s/%s", egress.Namespace, egress.Name)
	fwModels := m.makeLoxiFirewallModel(egress)
	klog.V(4).Infof("Make LoxiLB Firewall Models for Egress %s/%s: %v", egress.Namespace, egress.Name, fwModels)
	for _, fwModel := range fwModels {
		if err := m.callLoxiFirewallCreateAPI(ctx, fwModel); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) deleteEgress(egress *crdv1.Egress) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	klog.V(4).Infof("Deleting Egress %s/%s", egress.Namespace, egress.Name)
	fwModels := m.makeLoxiFirewallModel(egress)
	klog.V(4).Infof("Make LoxiLB Firewall Models for Egress %s/%s: %v", egress.Namespace, egress.Name, fwModels)
	for _, fwModel := range fwModels {
		if err := m.callLoxiFirewallDeleteAPI(ctx, fwModel); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) makeLoxiFirewallModel(egress *crdv1.Egress) []*api.FwRuleMod {
	newFwModels := []*api.FwRuleMod{}
	for _, address := range egress.Spec.Addresses {
		newFwModel := &api.FwRuleMod{
			Rule: api.FwRuleArg{
				SrcIP: address + "/32",
			},
			Opts: api.FwOptArg{
				DoSnat:    true,
				ToIP:      egress.Spec.Vip,
				ToPort:    0,
				OnDefault: true,
			},
		}
		newFwModels = append(newFwModels, newFwModel)
	}
	return newFwModels
}

func (m *Manager) callLoxiFirewallCreateAPI(ctx context.Context, fwModel *api.FwRuleMod) error {
	var errChList []chan error
	for _, client := range m.LoxiClients.Clients {
		ch := make(chan error)

		go func(c *api.LoxiClient, h chan error) {
			var err error
			if err = c.Firewall().Create(ctx, fwModel); err != nil {
				if !strings.Contains(err.Error(), "exist") {
					klog.Errorf("failed to create Egress(%s) :%v", c.Url, err)
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
		} else {
			klog.Errorf("failed to add Egress firewall rule (%v). err: %v", fwModel, err)
		}
	}
	if isError {
		return fmt.Errorf("failed to add loxiLB Egress firewall rule")
	}

	return nil
}

func (m *Manager) callLoxiFirewallDeleteAPI(ctx context.Context, fwModel *api.FwRuleMod) error {
	var errChList []chan error
	for _, loxiClient := range m.LoxiClients.Clients {
		ch := make(chan error)
		errChList = append(errChList, ch)

		go func(client *api.LoxiClient, ch chan error) {
			ch <- client.Firewall().Delete(ctx, fwModel)
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
		return fmt.Errorf("failed to delete loxiLB Firewall rule. Error: %v", errStr)
	}
	return nil
}

func (m *Manager) manageLoxilbEgressLifeCycle(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case c := <-m.ClientAliveCh:
			klog.V(4).Infof("Resync all Egresses (%s:alive)", c.Host)
			egresses, err := m.EgressLister.List(labels.Everything())
			if err != nil {
				klog.Errorf("failed to get egress list - err: %v", err)
			} else {
				klog.V(4).Infof("Resync all Egresses - (%v)", egresses)
				for _, egr := range egresses {
					m.addEgress(egr)
				}
			}
		}
	}
}
