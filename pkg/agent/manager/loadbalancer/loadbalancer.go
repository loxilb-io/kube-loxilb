/*
 * Copyright (c) 2022 NetLOX Inc
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

package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"net"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"kube-loxilb/pkg/agent/config"
	"kube-loxilb/pkg/api"
	"kube-loxilb/pkg/ippool"
	"kube-loxilb/pkg/k8s"
)

const (
	mgrName                     = "LoxilbLoadBalancerManager"
	resyncPeriod                = 60 * time.Second
	minRetryDelay               = 2 * time.Second
	maxRetryDelay               = 120 * time.Second
	defaultWorkers              = 4
	LoxiMaxWeight               = 10
	LoxiMultusServiceAnnotation = "loxilb.io/multus-nets"
	numSecIPAnnotation          = "loxilb.io/num-secondary-networks"
)

type Manager struct {
	kubeClient          clientset.Interface
	loxiClients         []*api.LoxiClient
	networkConfig       *config.NetworkConfig
	serviceInformer     coreinformers.ServiceInformer
	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced
	nodeInformer        coreinformers.NodeInformer
	nodeLister          corelisters.NodeLister
	nodeListerSynced    cache.InformerSynced
	ExternalIPPool      *ippool.IPPool
	ExtSecondaryIPPools []*ippool.IPPool

	queue   workqueue.RateLimitingInterface
	lbCache LbCacheTable
}

type LbCacheEntry struct {
	State       string
	SecIPs      []string
	LbModelList []api.LoadBalancerModel
}

type LbCacheTable map[string]*LbCacheEntry

type LbCacheKey struct {
	Namespace string
	Name      string
}

type SvcPair struct {
	IPString string
	Port     int32
	Protocol string
}

// GenKey generate key for cache
func GenKey(ns, name string) string {
	return path.Join(ns, name)
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewLoadBalancerManager(
	kubeClient clientset.Interface,
	loxiClients []*api.LoxiClient,
	externalIPPool *ippool.IPPool,
	externalSecondaryIPPools []*ippool.IPPool,
	networkConfig *config.NetworkConfig,
	informerFactory informers.SharedInformerFactory) *Manager {

	serviceInformer := informerFactory.Core().V1().Services()
	nodeInformer := informerFactory.Core().V1().Nodes()
	manager := &Manager{
		kubeClient:          kubeClient,
		loxiClients:         loxiClients,
		ExternalIPPool:      externalIPPool,
		ExtSecondaryIPPools: externalSecondaryIPPools,
		networkConfig:       networkConfig,
		serviceInformer:     serviceInformer,
		serviceLister:       serviceInformer.Lister(),
		serviceListerSynced: serviceInformer.Informer().HasSynced,
		nodeInformer:        nodeInformer,
		nodeLister:          nodeInformer.Lister(),
		nodeListerSynced:    nodeInformer.Informer().HasSynced,

		queue:   workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
		lbCache: make(LbCacheTable),
	}

	serviceInformer.Informer().AddEventHandlerWithResyncPeriod(
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
	svc, ok := obj.(*corev1.Service)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Received unexpected object: %v", obj)
			return
		}
		svc, ok = deletedState.Obj.(*corev1.Service)
		if !ok {
			klog.Errorf("DeletedFinalStateUnknown contains non-Service object: %v", deletedState.Obj)
		}
	}

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return
	}

	key := LbCacheKey{
		Namespace: svc.Namespace,
		Name:      svc.Name,
	}
	m.queue.Add(key)
}

func (m *Manager) Run(stopCh <-chan struct{}, loxiAliveCh <-chan *api.LoxiClient) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(
		mgrName,
		stopCh,
		m.serviceListerSynced,
		m.nodeListerSynced) {
		return
	}

	go m.reinstallLoxiLbRules(stopCh, loxiAliveCh)

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

	if key, ok := obj.(LbCacheKey); !ok {
		m.queue.Forget(obj)
		klog.Errorf("Expected string in work queue but got %#v", obj)
		return true
	} else if err := m.syncLoadBalancer(key); err == nil {
		m.queue.Forget(key)
	} else {
		m.queue.AddRateLimited(key)
		klog.Errorf("Error syncing Node %s, requeuing. Error: %v", key, err)
	}
	return true
}

func (m *Manager) syncLoadBalancer(lb LbCacheKey) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing endpoints %s. (%v)", lb.Name, time.Since(startTime))
	}()

	svcNs := lb.Namespace
	svcName := lb.Name
	svc, err := m.serviceLister.Services(svcNs).Get(svcName)
	if err != nil {
		return m.deleteLoadBalancer(svcNs, svcName)
	}
	return m.addLoadBalancer(svc)
}

func (m *Manager) addLoadBalancer(svc *corev1.Service) error {
	// check LoadBalancerClass
	lbClassName := svc.Spec.LoadBalancerClass
	if lbClassName == nil {
		return nil
	}

	numSecondarySvc := 0

	if strings.Compare(*lbClassName, m.networkConfig.LoxilbLoadBalancerClass) != 0 {
		return nil
	}

	// Check for loxilb specific annotations
	if na := svc.Annotations[numSecIPAnnotation]; na != "" {
		num, err := strconv.Atoi(na)
		if err != nil {
			numSecondarySvc = 0
		} else {
			numSecondarySvc = num
		}
	}

	// Check for loxilb specific annotation
	_, needPodEP := svc.Annotations[LoxiMultusServiceAnnotation]
	endpointIPs, err := m.getEndpoints(svc, needPodEP)
	if err != nil {
		return err
	}

	cacheKey := GenKey(svc.Namespace, svc.Name)
	lbCache, added := m.lbCache[cacheKey]
	if !added {
		//c.lbCache[cacheKey] = make([]api.LoadBalancerModel, 0)
		m.lbCache[cacheKey] = &LbCacheEntry{
			State: "Added",
		}
	}

	oldsvc := svc.DeepCopy()

	// Check if service has ingress IP already allocated
	ingSvcPairs, err := m.getIngressSvcPairs(svc)
	if err != nil {
		return err
	}

	if !added {
		ingSecSvcPairs, err := m.getIngressSecSvcPairs(svc, numSecondarySvc)
		if err != nil {
			return err
		}
		klog.Infof("Secondary IP Pairs %v", ingSecSvcPairs)

		for _, ingSecSvcPair := range ingSecSvcPairs {
			lbCache.SecIPs = append(lbCache.SecIPs, ingSecSvcPair.IPString)
		}
	}

	update := false
	if len(m.lbCache[cacheKey].LbModelList) <= 0 {
		update = true
	}

	// Update endpoint list if the list has changed
	for _, lbModel := range m.lbCache[cacheKey].LbModelList {
		if len(endpointIPs) == len(lbModel.Endpoints) {
			nEps := 0
			for _, ep := range endpointIPs {
				found := false
				for _, oldEp := range lbModel.Endpoints {
					if ep == oldEp.EndpointIP {
						found = true
						nEps++
						break
					}
				}
				if !found {
					break
				}
			}
			if nEps != len(endpointIPs) {
				update = true
			}
		} else {
			update = true
		}
	}

	if !update {
		ingSvcPairs = nil
		return nil
	} else {
		m.lbCache[cacheKey].LbModelList = nil
		svc.Status.LoadBalancer.Ingress = nil
	}

	// set defer for deallocate IP when get error
	isFailed := false
	defer func() {
		if isFailed {
			klog.Infof("deallocateOnFailure defer function called")
			for _, sp := range ingSvcPairs {
				klog.Infof("ip %s is newIP so retrieve pool", sp.IPString)
				m.ExternalIPPool.ReturnIPAddr(sp.IPString, uint32(sp.Port), sp.Protocol)
			}
		}
	}()

	for _, ingSvcPair := range ingSvcPairs {
		var errChList []chan error
		var lbModelList []api.LoadBalancerModel
		for _, port := range svc.Spec.Ports {
			lbModel, err := m.makeLoxiLoadBalancerModel(ingSvcPair.IPString, lbCache.SecIPs, svc, port, endpointIPs, needPodEP)
			if err != nil {
				return err
			}
			lbModelList = append(lbModelList, lbModel)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		for _, client := range m.loxiClients {
			ch := make(chan error)
			go func(c *api.LoxiClient, h chan error) {
				var err error
				for _, lbModel := range lbModelList {
					if err = c.LoadBalancer().Create(ctx, &lbModel); err != nil {
						break
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
			isFailed = isError
			klog.Errorf("failed to add load-balancer")
			return fmt.Errorf("failed to add loxiLB loadBalancer")
		}
		m.lbCache[cacheKey].LbModelList = append(m.lbCache[cacheKey].LbModelList, lbModelList...)
		retIngress := corev1.LoadBalancerIngress{IP: ingSvcPair.IPString}
		retIngress.Ports = append(retIngress.Ports, corev1.PortStatus{Port: ingSvcPair.Port, Protocol: corev1.Protocol(strings.ToUpper(ingSvcPair.Protocol))})
		svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress, retIngress)
		klog.Infof("added load-balancer")
	}

	// Update service.Status.LoadBalancer.Ingress
	m.updateService(oldsvc, svc)

	return nil
}

func (m *Manager) updateService(old, new *corev1.Service) error {
	if !reflect.DeepEqual(old.Status, new.Status) {
		_, err := m.kubeClient.CoreV1().Services(new.Namespace).UpdateStatus(context.TODO(), new, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to update service %s.status. err: %v", new.Name, err)
			return err
		}
	}

	return nil
}

func (m *Manager) deleteLoadBalancer(ns, name string) error {
	cacheKey := GenKey(ns, name)
	lbEntry, ok := m.lbCache[cacheKey]
	if !ok {
		klog.Warningf("not found service %s", name)
		return nil
	}

	for _, lb := range lbEntry.LbModelList {
		var errChList []chan error
		for _, loxiClient := range m.loxiClients {
			ch := make(chan error)
			errChList = append(errChList, ch)

			go func(client *api.LoxiClient, ch chan error) {
				klog.Infof("called loxilb API: delete lb rule %v", lb)
				ch <- client.LoadBalancer().Delete(context.Background(), &lb)
			}(loxiClient, ch)
		}

		isError := true
		for _, errCh := range errChList {
			err := <-errCh
			if err == nil {
				isError = false
				break
			}
		}
		if isError {
			return fmt.Errorf("failed to delete loxiLB LoadBalancer")
		}
		m.ExternalIPPool.ReturnIPAddr(lb.Service.ExternalIP, uint32(lb.Service.Port), lb.Service.Protocol)
		for idx, ingSecIP := range lbEntry.SecIPs {
			if idx < len(m.ExtSecondaryIPPools) {
				m.ExtSecondaryIPPools[idx].ReturnIPAddr(ingSecIP, uint32(lb.Service.Port), lb.Service.Protocol)
			}
		}
	}

	delete(m.lbCache, cacheKey)
	return nil
}

// getEndpoints return LB's endpoints IP list.
// If podEP is true, return multus endpoints list.
// If false, return worker nodes IP list.
func (m *Manager) getEndpoints(svc *corev1.Service, podEP bool) ([]string, error) {
	if podEP {
		return m.getMultusEndpoints(svc)
	}

	return m.getNodeEndpoints()
}

// getNodeEndpoints returns the IP list of nodes available as nodePort service.
func (m *Manager) getNodeEndpoints() ([]string, error) {
	req, err := labels.NewRequirement("node.kubernetes.io/exclude-from-external-load-balancers", selection.DoesNotExist, []string{})
	if err != nil {
		klog.Infof("getEndpoints: failed to make label requirement. err: %v", err)
		return nil, err
	}

	nodes, err := m.nodeLister.List(labels.NewSelector().Add(*req))
	if err != nil {
		klog.Infof("getEndpoints: failed to get nodeList. err: %v", err)
		return nil, err
	}

	return m.getEndpointsForLB(nodes), nil
}

// getMultusEndpoints returns the IP list of the Pods connected to the multus network.
func (m *Manager) getMultusEndpoints(svc *corev1.Service) ([]string, error) {
	netListStr, ok := svc.Annotations[LoxiMultusServiceAnnotation]
	if !ok {
		return nil, errors.New("not found multus annotations")
	}
	netList := strings.Split(netListStr, ",")

	return k8s.GetMultusEndpoints(m.kubeClient, svc, netList)
}

func (m *Manager) getNodeAddress(node corev1.Node) (string, error) {
	addrs := node.Status.Addresses
	if len(addrs) == 0 {
		return "", errors.New("no address found for host")
	}

	for _, addr := range addrs {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}

	return addrs[0].Address, nil
}

func (m *Manager) getEndpointsForLB(nodes []*corev1.Node) []string {
	var endpoints []string
	for _, node := range nodes {
		addr, err := m.getNodeAddress(*node)
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}
		endpoints = append(endpoints, addr)
	}

	return endpoints
}

func (m *Manager) getLBIngressSvcPairs(service *corev1.Service) []SvcPair {
	var spairs []SvcPair
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		for _, port := range service.Spec.Ports {
			sp := SvcPair{ingress.IP, port.Port, strings.ToLower(string(port.Protocol))}
			spairs = append(spairs, sp)
		}
	}

	return spairs
}

// getIngressSvcPairs check validation if service have ingress IP already.
// If service have no ingress IP, assign new IP in IP pool
func (m *Manager) getIngressSvcPairs(service *corev1.Service) ([]SvcPair, error) {
	var sPairs []SvcPair
	inSPairs := m.getLBIngressSvcPairs(service)
	isHasLoxiExternalIP := false

	// k8s service has ingress IP already
	if len(inSPairs) >= 1 {
		for _, inSPair := range inSPairs {
			ident := inSPair.Port
			proto := inSPair.Protocol

			inRange, _ := m.ExternalIPPool.CheckAndReserveIP(inSPair.IPString, uint32(ident), proto)
			if inRange {
				sp := SvcPair{inSPair.IPString, ident, inSPair.Protocol}
				sPairs = append(sPairs, sp)
				isHasLoxiExternalIP = true
			}
		}
	}

	// If isHasLoxiExternalIP is false, that means:
	//    1. k8s service has no ingress IP
	//    2. k8s service has ingress IPs, but that is outside the range of kube-loxilb's externalIP.
	// so that service need to be allowed new external IP
	if !isHasLoxiExternalIP {
		for _, port := range service.Spec.Ports {
			proto := strings.ToLower(string(port.Protocol))
			portNum := port.Port
			newIP := m.ExternalIPPool.GetNewIPAddr(uint32(portNum), proto)
			if newIP == nil {
				// This is a safety code in case the service has the same port.
				for _, s := range sPairs {
					if s.Port == portNum && s.Protocol == proto {
						continue
					}
				}
				klog.Errorf("failed to generate external IP. IP Pool is full")
				return nil, errors.New("failed to generate external IP. IP Pool is full")
			}
			sp := SvcPair{newIP.String(), portNum, proto}
			sPairs = append(sPairs, sp)
		}
	}

	return sPairs, nil
}

// getIngressSecSvcPairs returns a set of secondary IPs
func (m *Manager) getIngressSecSvcPairs(service *corev1.Service, numSecondary int) ([]SvcPair, error) {
	var sPairs []SvcPair

	if len(m.ExtSecondaryIPPools) < numSecondary {
		klog.Errorf("failed to generate external secondary IP. No IP pools")
		return nil, errors.New("failed to generate external secondary IP. No IP pools")
	}

	for i := 0; i < numSecondary; i++ {
		for _, port := range service.Spec.Ports {
			pool := m.ExtSecondaryIPPools[i]
			proto := strings.ToLower(string(port.Protocol))
			portNum := port.Port
			newIP := pool.GetNewIPAddr(uint32(portNum), proto)
			if newIP == nil {
				// This is a safety code in case the service has the same port.
				for _, s := range sPairs {
					if s.Port == portNum && s.Protocol == proto {
						continue
					}
				}
				for j := 0; j < i; j++ {
					rpool := m.ExtSecondaryIPPools[j]
					rpool.ReturnIPAddr(sPairs[j].IPString, uint32(portNum), proto)
				}
				klog.Errorf("failed to generate external secondary IP. IP Pool is full")
				return nil, errors.New("failed to generate external secondary IP. IP Pool is full")
			}
			sp := SvcPair{newIP.String(), portNum, proto}
			sPairs = append(sPairs, sp)
		}
	}

	return sPairs, nil
}

func (m *Manager) getLoadBalancerServiceIngressIPs(service *corev1.Service) []string {
	var ips []string
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ips = append(ips, ingress.IP)
	}

	return ips
}

func (m *Manager) makeLoxiLoadBalancerModel(externalIP string, secIPs []string,  svc *corev1.Service, port corev1.ServicePort, endpointIPs []string, needPodEP bool) (api.LoadBalancerModel, error) {
	loxiEndpointModelList := []api.LoadBalancerEndpoint{}
	loxiSecIPModelList := []api.LoadBalancerSecIp{}

	if len(endpointIPs) > 0 {
		endpointWeight := uint8(LoxiMaxWeight / len(endpointIPs))
		remainderWeight := uint8(LoxiMaxWeight % len(endpointIPs))

		for _, endpoint := range endpointIPs {
			weight := endpointWeight
			if remainderWeight > 0 {
				weight++
				remainderWeight--
			}

			tport := uint16(port.NodePort)
			if needPodEP {
				portNum, err := k8s.GetServicePortIntValue(m.kubeClient, svc, port)
				if err != nil {
					return api.LoadBalancerModel{}, err
				}
				tport = uint16(portNum)
			}

			loxiEndpointModelList = append(loxiEndpointModelList, api.LoadBalancerEndpoint{
				EndpointIP: endpoint,
				TargetPort: tport,
				Weight:     weight,
			})
		}
	}

	if len(secIPs) > 0 {
		for _, secIP := range secIPs {
			loxiSecIPModelList = append(loxiSecIPModelList, api.LoadBalancerSecIp{SecondaryIP: secIP})
		}
	}

	return api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: externalIP,
			Port:       uint16(port.Port),
			Protocol:   strings.ToLower(string(port.Protocol)),
			BGP:        m.networkConfig.SetBGP,
			Mode:       api.LbMode(m.networkConfig.SetLBMode),
			Monitor:    m.networkConfig.Monitor,
		},
		SecondaryIPs: loxiSecIPModelList,
		Endpoints:    loxiEndpointModelList,
	}, nil
}

func (m *Manager) addIngress(service *corev1.Service, newIP net.IP) {
	service.Status.LoadBalancer.Ingress =
		append(service.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: newIP.String()})
}

func (m *Manager) reinstallLoxiLbRules(stopCh <-chan struct{}, loxiAliveCh <-chan *api.LoxiClient) {
loop:
	for {
		select {
		case <-stopCh:
			break loop
		case aliveClient := <-loxiAliveCh:
			isSuccess := false
			for _, value := range m.lbCache {
				for _, lbModel := range value.LbModelList {
					klog.Infof("reinstallLoxiLbRules: lbModel: %v", lbModel)
					for retry := 0; retry < 5; retry++ {
						klog.Infof("retry reinstall LB rule...count %d", retry)
						if err := aliveClient.LoadBalancer().Create(context.Background(), &lbModel); err == nil {
							klog.Infof("reinstall success")
							isSuccess = true
							break
						} else {
							time.Sleep(1 * time.Second)
						}
					}
					if !isSuccess {
						klog.Exit("restart loxi-ccm")
					}
				}
			}
		}
	}
}
