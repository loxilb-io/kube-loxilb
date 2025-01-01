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

package loxiurl

import (
	"context"
	"fmt"
	"net"
	urllib "net/url"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/loadbalancer"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	crdv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/loxiurl/v1"
	"github.com/loxilb-io/kube-loxilb/pkg/klb-client/clientset/versioned"
	klbCRDinformers "github.com/loxilb-io/kube-loxilb/pkg/klb-client/informers/externalversions"
	crdInformer "github.com/loxilb-io/kube-loxilb/pkg/klb-client/informers/externalversions/loxiurl/v1"
	crdLister "github.com/loxilb-io/kube-loxilb/pkg/klb-client/listers/loxiurl/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "LoxiLBURLManager"
	defaultWorkers = 1
	resyncPeriod   = 60 * time.Second
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
)

type Manager struct {
	kubeClient            kubernetes.Interface
	kubeExtClient         apiextensionclientset.Interface
	crdClient             versioned.Interface
	loxiLBURLInformer     crdInformer.LoxiURLInformer
	loxiLBURLLister       crdLister.LoxiURLLister
	loxiLBURLListerSynced cache.InformerSynced
	queue                 workqueue.RateLimitingInterface
	networkConfig         *config.NetworkConfig
	lbManager             *loadbalancer.Manager
	loxiLBURLPurgeCh      chan *api.LoxiClient
	loxiLBURLAliveCh      chan *api.LoxiClient
	loxiLBURLDeadCh       chan struct{}
	crdControlOn          bool
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewLoxiLBURLManager(
	kubeClient kubernetes.Interface,
	kubeExtClient apiextensionclientset.Interface,
	crdClient versioned.Interface,
	networkConfig *config.NetworkConfig,
	loxLBURLInformer crdInformer.LoxiURLInformer,
	lbManager *loadbalancer.Manager,
) *Manager {

	manager := &Manager{

		kubeClient:            kubeClient,
		kubeExtClient:         kubeExtClient,
		crdClient:             crdClient,
		loxiLBURLInformer:     loxLBURLInformer,
		loxiLBURLLister:       loxLBURLInformer.Lister(),
		loxiLBURLListerSynced: loxLBURLInformer.Informer().HasSynced,
		networkConfig:         networkConfig,
		lbManager:             lbManager,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
	}

	loxLBURLInformer.Informer().AddEventHandlerWithResyncPeriod(
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
	lb, ok := obj.(*crdv1.LoxiURL)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		lb, ok = deletedState.Obj.(*crdv1.LoxiURL)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-BGPPeerService object: %v", deletedState.Obj)
		}
	}

	m.queue.Add(lb)
}

func (m *Manager) WaitForLoxiURLCRDCreation(stopCh <-chan struct{}) {

	wait.PollImmediateUntil(time.Second*5,
		func() (bool, error) {
			_, err := m.kubeExtClient.ApiextensionsV1().CustomResourceDefinitions().
				Get(context.TODO(), "loxiurls.loxiurl.loxilb.io", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			klog.Infof("loxilb-url crd created")
			return true, nil
		},
		stopCh)
}

func (m *Manager) Run(stopCh <-chan struct{}, loxiLBLiveCh chan *api.LoxiClient, loxiLBDeadCh chan struct{}, loxiLBPurgeCh chan *api.LoxiClient) {
	defer m.queue.ShutDown()

	if m.loxiLBURLPurgeCh == nil {
		m.loxiLBURLPurgeCh = loxiLBPurgeCh
	}

	if m.loxiLBURLAliveCh == nil {
		m.loxiLBURLAliveCh = loxiLBLiveCh
	}

	if m.loxiLBURLDeadCh == nil {
		m.loxiLBURLDeadCh = loxiLBDeadCh
	}

	klog.Infof("Running %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(
		mgrName,
		stopCh,
		m.loxiLBURLListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (m *Manager) Start(informer klbCRDinformers.SharedInformerFactory, stopCh <-chan struct{}, loxiLBLiveCh chan *api.LoxiClient, loxiLBDeadCh chan struct{}, loxiLBPurgeCh chan *api.LoxiClient) {
	klog.Infof("Starting %s", mgrName)

	m.WaitForLoxiURLCRDCreation(stopCh)
	informer.Start(stopCh)
	m.Run(stopCh, loxiLBLiveCh, loxiLBDeadCh, loxiLBPurgeCh)
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

	if lburl, ok := obj.(*crdv1.LoxiURL); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncLBURLs(lburl); err == nil {
		m.queue.Forget(obj)
	} else {
		m.queue.AddRateLimited(obj)
		klog.Errorf("Error syncing CRD LoxiURL %s, requeuing. Error: %v", lburl.Name, err)
	}
	return true
}

func (m *Manager) syncLBURLs(url *crdv1.LoxiURL) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing syncLBURLs %s. (%v)", url.Name, time.Since(startTime))
	}()
	_, err := m.loxiLBURLLister.Get(url.Name)
	if err != nil {
		return m.deleteLoxiLBURL(url)
	}
	return m.addLoxiLBURL(url)
}

func (m *Manager) deleteAllLoxiClients() error {
	for _, v := range m.lbManager.LoxiClients.Clients {
		v.StopLoxiHealthCheckChan()
		klog.Infof("loxi-client (%v) removed", v.Host)
		m.loxiLBURLPurgeCh <- v
	}
	m.lbManager.LoxiClients.Clients = nil
	return nil
}

func (m *Manager) deleteSingleLoxiClientsWithName(name string) bool {
	var validLoxiClients []*api.LoxiClient

	match := false
	for _, v := range m.lbManager.LoxiClients.Clients {
		if v.Name == name {
			match = true
			v.StopLoxiHealthCheckChan()
			klog.Infof("loxi-client (%v) removed", v.Host)
			m.loxiLBURLPurgeCh <- v
		} else {
			validLoxiClients = append(validLoxiClients, v)
		}
	}

	if match {
		m.lbManager.LoxiClients.Clients = validLoxiClients
	}

	return match
}

func (m *Manager) updateLoxiLBURL(url *crdv1.LoxiURL) error {
	fmt.Println(m.loxiLBURLLister.Get(url.Name))
	return nil
}

func (m *Manager) addLoxiLBURL(url *crdv1.LoxiURL) error {
	var tmploxilbClients []*api.LoxiClient
	var currLoxiURLs []string
	var validLoxiURLs []string

	if url.Spec.LoxiZone != "" && url.Spec.LoxiZone != m.networkConfig.Zone {
		return nil
	}

	if url.Spec.LoxiURLType == "cidrpool" {
		return m.lbManager.AddLoxiCIDRPool(url.Name, url.Spec.LoxiURL)
	}

	if url.Spec.LoxiURLType == "hostcidr" {
		ip := net.ParseIP(url.Spec.LoxiURL)
		if ip == nil {
			klog.Infof("loxilb-url crd add (%s) host cidr parse failed", url.Spec.LoxiURL)
			return fmt.Errorf("loxilb-url crd add (%s) host cidr parse failed", url.Spec.LoxiURL)
		}
		return m.lbManager.AddLoxiInstAddr(url.Name, ip)
	}

	if url.Spec.LoxiURLType != "" && url.Spec.LoxiURLType != "default" {
		klog.Errorf("loxilb-url crd %s add (%s) type not support", url.Name, url.Spec.LoxiURLType)
		return nil
	}

	if len(m.networkConfig.LoxilbURLs) <= 0 {
		klog.Infof("loxilb-url crd add (%v) : incompatible with incluster mode", url)
		return nil
	}

	if !m.crdControlOn {
		m.deleteAllLoxiClients()
		m.crdControlOn = true
	}

	for _, client := range m.lbManager.LoxiClients.Clients {
		currLoxiURLs = append(currLoxiURLs, client.Url)
	}

	newloxiURLS := strings.Split(url.Spec.LoxiURL, ",")

	for _, u := range newloxiURLS {
		if _, err := urllib.Parse(u); err != nil {
			klog.Errorf("loxiURL %s is invalid. err: %v", u, err)
			return fmt.Errorf("loxiURL %s is invalid. err: %v", u, err)
		}
	}

	urlChg := false
nextURL:
	for _, nurl := range newloxiURLS {
		for _, ourl := range currLoxiURLs {
			if ourl == nurl {
				continue nextURL
			}
		}
		validLoxiURLs = append(validLoxiURLs, nurl)
		urlChg = true
	}

	if urlChg {
		m.deleteSingleLoxiClientsWithName(url.Name)
		klog.Infof("loxilb-url Add (%v)", url)
		for _, nurl := range validLoxiURLs {
			client, err2 := api.NewLoxiClient(nurl, m.loxiLBURLAliveCh, m.loxiLBURLDeadCh, false, false, url.Name, m.networkConfig.Zone, m.networkConfig.NumZoneInst)
			if err2 != nil {
				continue
			}
			tmploxilbClients = append(tmploxilbClients, client)
		}

		m.lbManager.LoxiClients.Clients = append(m.lbManager.LoxiClients.Clients, tmploxilbClients...)
	}

	return nil
}

func (m *Manager) deleteLoxiLBURL(url *crdv1.LoxiURL) error {
	type validLoxiURLwName struct {
		url  string
		name string
	}
	var validLoxiURLs []validLoxiURLwName
	var currLoxiURLs []validLoxiURLwName
	var deletedloxiURLS []validLoxiURLwName

	if url.Spec.LoxiZone != "" && url.Spec.LoxiZone != m.networkConfig.Zone {
		return nil
	}

	if url.Spec.LoxiURLType == "cidrpool" {
		return m.lbManager.DeleteLoxiCIDRPool(url.Name, url.Spec.LoxiURL)
	}

	if url.Spec.LoxiURLType == "hostcidr" {
		ip := net.ParseIP(url.Spec.LoxiURL)
		if ip == nil {
			klog.Infof("loxilb-url crd add (%s) host cidr parse failed", url.Spec.LoxiURL)
			return fmt.Errorf("loxilb-url crd add (%s) host cidr parse failed", url.Spec.LoxiURL)
		}
		return m.lbManager.DeleteLoxiInstAddr(url.Name)
	}

	if url.Spec.LoxiURLType != "" && url.Spec.LoxiURLType != "default" {
		klog.Errorf("loxilb-url crd %s delete (%s) type not support", url.Name, url.Spec.LoxiURLType)
		return nil
	}

	if len(m.networkConfig.LoxilbURLs) <= 0 {
		klog.Infof("loxilb-url crd delete (%v) : incompatible with incluster mode", url)
		return nil
	}

	klog.Infof("loxilb-url delete (%v)", url)

	for _, client := range m.lbManager.LoxiClients.Clients {
		currLoxiURLs = append(currLoxiURLs, validLoxiURLwName{url: client.Url, name: client.Name})
	}

	deleted := strings.Split(url.Spec.LoxiURL, ",")

	for _, u := range deleted {
		if _, err := urllib.Parse(u); err != nil {
			klog.Errorf("loxiURL %s is invalid. err: %v", u, err)
			return fmt.Errorf("loxiURL %s is invalid. err: %v", u, err)
		}
		deletedloxiURLS = append(deletedloxiURLS, validLoxiURLwName{url: u, name: url.Name})
	}

	matchCount := 0
nextURL:
	for _, durl := range deletedloxiURLS {
		for _, ourl := range currLoxiURLs {
			if ourl == durl {
				matchCount++
				continue nextURL
			}
		}
	}

nextURL1:
	for _, ourl := range currLoxiURLs {
		for _, durl := range deletedloxiURLS {
			if ourl == durl {
				continue nextURL1
			}
		}
		validLoxiURLs = append(validLoxiURLs, ourl)
	}

	if matchCount == len(deletedloxiURLS) {
		m.deleteAllLoxiClients()

		for _, nurl := range validLoxiURLs {
			client, err2 := api.NewLoxiClient(nurl.url, m.loxiLBURLAliveCh, m.loxiLBURLDeadCh, false, false, nurl.name, m.networkConfig.Zone, m.networkConfig.NumZoneInst)
			if err2 != nil {
				continue
			}
			m.lbManager.LoxiClients.Clients = append(m.lbManager.LoxiClients.Clients, client)
		}
	}

	return nil
}
