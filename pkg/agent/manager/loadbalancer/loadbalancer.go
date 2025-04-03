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
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
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

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
	"github.com/loxilb-io/kube-loxilb/pkg/k8s"
	tk "github.com/loxilb-io/loxilib"
	"github.com/pkg/errors"
)

const (
	mgrName                       = "LoxilbLoadBalancerManager"
	resyncPeriod                  = 60 * time.Second
	minRetryDelay                 = 2 * time.Second
	maxRetryDelay                 = 120 * time.Second
	defaultWorkers                = 1
	LoxiMaxWeight                 = 10
	LoxiMultusServiceAnnotation   = "loxilb.io/multus-nets"
	PoolNameAnnotation            = "loxilb.io/poolSelect"
	SecPoolNameAnnotation         = "loxilb.io/poolSelectSecondary"
	secIPsAnnotation              = "loxilb.io/secondaryIPs"
	staticIPAnnotation            = "loxilb.io/staticIP"
	livenessAnnotation            = "loxilb.io/liveness"
	lbModeAnnotation              = "loxilb.io/lbmode"
	lbAddressAnnotation           = "loxilb.io/ipam"
	lbTimeoutAnnotation           = "loxilb.io/timeout"
	probeTypeAnnotation           = "loxilb.io/probetype"
	probePortAnnotation           = "loxilb.io/probeport"
	probeReqAnnotation            = "loxilb.io/probereq"
	probeRespAnnotation           = "loxilb.io/proberesp"
	probeTimeoutAnnotation        = "loxilb.io/probetimeout"
	probeRetriesAnnotation        = "loxilb.io/proberetries"
	endPointSelAnnotation         = "loxilb.io/epselect"
	zoneSelAnnotation             = "loxilb.io/zoneselect"
	prefLocalPodAnnotation        = "loxilb.io/prefLocalPod"
	matchNodeLabelAnnotation      = "loxilb.io/nodelabel"
	usePodNetworkAnnotation       = "loxilb.io/usepodnetwork"
	useExternalEndpointAnnotation = "loxilb.io/useExternalEndpoint"
	MaxExternalSecondaryIPsNum    = 4
	defaultPoolName               = "defaultPool"
	loxilbZoneLabelKey            = "loxilb.io/zonelabel"
	loxilbZoneInstance            = "loxilb.io/zoneinstance"
	enProxyProtov2Annotation      = "loxilb.io/useproxyprotov2"
	egressAnnotation              = "loxilb.io/egress"
)

type LoxiInstRole struct {
	instID int
}

type Manager struct {
	mtx                 sync.RWMutex
	kubeClient          clientset.Interface
	LoxiClients         *api.LoxiClientPool
	LoxiPeerClients     *api.LoxiClientPool
	networkConfig       *config.NetworkConfig
	serviceInformer     coreinformers.ServiceInformer
	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced
	nodeInformer        coreinformers.NodeInformer
	nodeLister          corelisters.NodeLister
	nodeListerSynced    cache.InformerSynced
	ElectionRunOnce     bool
	queue               workqueue.RateLimitingInterface
	lbCache             LbCacheTable
	ipPoolTbl           map[string]*ippool.IPPool
	ip6PoolTbl          map[string]*ippool.IPPool
	zoneInstRoleMap     map[string]*LoxiInstRole
	instAddrApplyCh     chan struct{}
	loxiInstAddrMap     map[string]net.IP
	zoneInstSelHint     int
	ClientAliveCh       chan *api.LoxiClient
	ClientPurgeCh       chan *api.LoxiClient
	ClientSelMasterCh   chan bool
	ClientDeadCh        chan struct{}
}

type LbArgs struct {
	externalIP          string
	privateIP           string
	livenessCheck       bool
	lbMode              int
	timeout             int
	sel                 api.EpSelect
	probeType           string
	probePort           uint16
	probeReq            string
	probeResp           string
	probeTimeo          uint32
	probeRetries        int
	secIPs              []string
	endpointIPs         []string
	needMultusEP        bool
	usePodNetwork       bool
	useExternalEndpoint bool
	inst                string
	ppv2En              bool
	egress              bool
}

type LbModelEnt struct {
	LbModel api.LoadBalancerModel
}

type LbServicePairEntry struct {
	ExternalIP  string
	Port        uint16
	Protocol    string
	StaticIP    bool
	InRange     bool
	IdentIPAM   string
	LbModelList []api.LoadBalancerModel
}

type LbCacheEntry struct {
	LbMode         int
	Timeout        int
	ActCheck       bool
	PrefLocal      bool
	ppv2En         bool
	egress         bool
	Inst           string
	Addr           string
	State          string
	NodeLabel      string
	ProbeType      string
	ProbePort      uint16
	ProbeReq       string
	ProbeResp      string
	ProbeTimeo     uint32
	ProbeRetries   int
	EpSelect       api.EpSelect
	IPPool         *ippool.IPPool
	SIPPools       []*ippool.IPPool
	SecIPs         []string
	LbServicePairs map[string]*LbServicePairEntry
}

type LbCacheTable map[string]*LbCacheEntry

type IPPoolTable map[string]*ippool.IPPool

type LbCacheKey struct {
	Namespace string
	Name      string
}

type SvcPair struct {
	IPString   string
	Port       int32
	Protocol   string
	InRange    bool
	StaticIP   bool
	IdentIPAM  string
	IPAllocd   bool
	K8sSvcPort corev1.ServicePort
}

func (s SvcPair) String() string {
	return fmt.Sprintf("\n  IPString: %s\n  Port: %d\n  Protocol: %s\n  InRange: %v\n  StaticIP: %v\n  IdentIPAM: %s\n  IPAllocd:  %v\n  K8sSvcPort: %v\n",
		s.IPString, s.Port, s.Protocol, s.InRange, s.StaticIP, s.IdentIPAM, s.IPAllocd, s.K8sSvcPort,
	)
}

// GenKey generate key for cache
func GenKey(ns, name string) string {
	return path.Join(ns, name)
}

// GenSPKey generate key for cache
func GenSPKey(IPString string, Port uint16, Protocol string) string {
	return fmt.Sprintf("%s:%v:%s", IPString, Port, Protocol)
}

func (m *Manager) genExtIPName(ipStr string) []string {
	var hosts []string
	prefix := m.networkConfig.Zone + "-"

	IP := net.ParseIP(ipStr)
	if IP != nil {
		if IP.IsUnspecified() {
			m.mtx.Lock()
			defer m.mtx.Unlock()
			if len(m.loxiInstAddrMap) <= 0 {
				return []string{"llbanyextip"}
			} else {
				for _, host := range m.loxiInstAddrMap {
					hosts = append(hosts, host.String())
				}
			}
			return hosts
		}
	}
	if tk.IsNetIPv6(ipStr) {
		ipStrSlice := strings.Split(ipStr, ":")
		ipStr = strings.Join(ipStrSlice, "-")
	}
	return []string{prefix + ipStr}
}

// Create and Init Manager.
// Manager is called by kube-loxilb when k8s service is created & updated.
func NewLoadBalancerManager(
	kubeClient clientset.Interface,
	loxiClients *api.LoxiClientPool,
	loxiPeerClients *api.LoxiClientPool,
	ipPoolTbl map[string]*ippool.IPPool,
	ip6PoolTbl map[string]*ippool.IPPool,
	networkConfig *config.NetworkConfig,
	informerFactory informers.SharedInformerFactory) *Manager {

	serviceInformer := informerFactory.Core().V1().Services()
	nodeInformer := informerFactory.Core().V1().Nodes()
	manager := &Manager{
		kubeClient:          kubeClient,
		LoxiClients:         loxiClients,
		LoxiPeerClients:     loxiPeerClients,
		ipPoolTbl:           ipPoolTbl,
		ip6PoolTbl:          ip6PoolTbl,
		networkConfig:       networkConfig,
		serviceInformer:     serviceInformer,
		serviceLister:       serviceInformer.Lister(),
		serviceListerSynced: serviceInformer.Informer().HasSynced,
		nodeInformer:        nodeInformer,
		nodeLister:          nodeInformer.Lister(),
		nodeListerSynced:    nodeInformer.Informer().HasSynced,
		instAddrApplyCh:     make(chan struct{}),
		loxiInstAddrMap:     make(map[string]net.IP),
		zoneInstRoleMap:     make(map[string]*LoxiInstRole),
		ClientAliveCh:       make(chan *api.LoxiClient, 50),
		ClientPurgeCh:       make(chan *api.LoxiClient, 5),
		ClientSelMasterCh:   make(chan bool),
		ClientDeadCh:        make(chan struct{}, 64),

		queue:   workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "loadbalancer"),
		lbCache: make(LbCacheTable),
	}

	for i := 0; i < networkConfig.NumZoneInst; i++ {
		name := api.GenZoneInstName(networkConfig.Zone, i)
		manager.zoneInstRoleMap[name] = &LoxiInstRole{instID: i}
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

func (m *Manager) Run(stopCh <-chan struct{}) {
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

	go m.manageLoxiLbLifeCycle(stopCh)

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
		klog.Errorf("Error syncing LoadBalancer %s, requeuing. Error: %v", key, err)
	}
	return true
}

func (m *Manager) syncLoadBalancer(lb LbCacheKey) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing LoadBalancer service %s. (%v)", lb.Name, time.Since(startTime))
	}()

	svcNs := lb.Namespace
	svcName := lb.Name
	svc, err := m.serviceLister.Services(svcNs).Get(svcName)
	if err != nil {
		return m.deleteLoadBalancer(svcNs, svcName, true)
	}
	return m.addLoadBalancer(svc)
}

func (m *Manager) getZoneInstName() string {
	return api.GenZoneInstName(m.networkConfig.Zone, m.zoneInstSelHint%m.networkConfig.NumZoneInst)
}

func (m *Manager) addLoadBalancer(svc *corev1.Service) error {

	zone := svc.Annotations[zoneSelAnnotation]
	if zone != "" {
		if m.networkConfig.Zone != zone {
			return nil
		}
	}

	// check loxilb
	if len(m.LoxiClients.Clients) == 0 {
		return fmt.Errorf("service cannot be added because there are no loxilb instances")
	}

	// check LoadBalancerClass
	lbClassName := svc.Spec.LoadBalancerClass

	// Check for loxilb specific annotation - Multus Networks
	_, needMultusEP := svc.Annotations[LoxiMultusServiceAnnotation]
	if lbClassName == nil && !needMultusEP {
		klog.V(4).Infof("service %s/%s missing loadBalancerClass & multus annotation", svc.Namespace, svc.Name)
		return nil
	}

	var secIPs []string
	numSecondarySvc := 0
	livenessCheck := false
	lbMode := -1
	addrType := "ipv4"
	epAddrType := "ipv4"
	timeout := 30 * 60
	probeType := ""
	probePort := 0
	probeReq := ""
	probeResp := ""
	probeTimeout := uint32(0)
	probeRetries := 0
	prefLocal := false
	epSelect := api.LbSelRr
	matchNodeLabel := ""
	usePodNet := false
	useExternalEndpoint := false
	hasSharedPool := false
	overrideZoneInst := ""
	enProxyProtov2 := false
	isEgress := false

	if strings.Compare(*lbClassName, m.networkConfig.LoxilbLoadBalancerClass) != 0 && !needMultusEP {
		return nil
	}

	// Check for loxilb specific annotations - Addressing
	if lba := svc.Annotations[lbAddressAnnotation]; lba != "" {
		if lba == "ipv4" || lba == "ip" || lba == "ip4" {
			addrType = "ipv4"
		} else if lba == "ipv6" || lba == "ip6" || lba == "nat66" {
			addrType = "ipv6"
		} else if lba == "ipv6to4" || lba == "nat64" {
			addrType = "ipv6to4"
		}
	}

	var sipPools []*ippool.IPPool
	ipPool := m.ipPoolTbl[defaultPoolName]
	if addrType == "ipv6" || addrType == "ipv6to4" {
		ipPool = m.ip6PoolTbl[defaultPoolName]
		if addrType == "ipv6" {
			epAddrType = addrType
		}
	}

	// Check for loxilb specific annotations - poolName
	if pn := svc.Annotations[PoolNameAnnotation]; pn != "" {
		poolTbl := m.ipPoolTbl
		if addrType == "ipv6" || addrType == "ipv6to4" {
			poolTbl = m.ip6PoolTbl
		}
		if pool := poolTbl[pn]; pool != nil {
			ipPool = pool
		} else {
			return fmt.Errorf("%s pool not found", pn)
		}
	}

	if ipPool.Shared {
		hasSharedPool = true
	}

	// Check for loxilb specific annotations - SecondayPoolName
	if spn := svc.Annotations[SecPoolNameAnnotation]; spn != "" {
		poolTbl := m.ipPoolTbl
		if addrType == "ipv6" || addrType == "ipv6to4" {
			poolTbl = m.ip6PoolTbl
		}
		spools := strings.Split(spn, ",")
		for _, spool := range spools {
			if pool := poolTbl[spool]; pool != nil {
				sipPools = append(sipPools, pool)
				if pool.Shared {
					hasSharedPool = true
				}
			} else {
				return fmt.Errorf("%s secondary pool not found", spool)
			}
		}
		numSecondarySvc = len(spools)
	}

	// Check for loxilb specific annotations - Secondary IPs (user specified)
	if secIPsStr := svc.Annotations[secIPsAnnotation]; secIPsStr != "" {
		secIPs = strings.Split(secIPsStr, ",")
		if len(secIPs) >= 4 {
			klog.Errorf("%s annotation exceeds the maximum number(%d) allowed.", secIPsAnnotation, MaxExternalSecondaryIPsNum)
			secIPs = nil
		} else {
			for _, secIP := range secIPs {
				if net.ParseIP(secIP) == nil {
					klog.Errorf("%s annotation has invalid IP (%s)", secIPsAnnotation, secIP)
					secIPs = nil
					break
				}
			}
		}
	}

	if addrType != "ipv4" && len(sipPools) != 0 {
		return fmt.Errorf("SecondaryIP Svc not possible for %v", addrType)
	}

	if len(secIPs) != 0 && len(sipPools) != 0 {
		return fmt.Errorf("SecondaryIP is specified (%v)", secIPs)
	}

	// Check for loxilb specific annotations - usePodNet
	usePodNet = m.networkConfig.UsePodNetwork
	if upn := svc.Annotations[usePodNetworkAnnotation]; upn != "" {
		if upn == "yes" {
			usePodNet = true
		} else if upn == "no" {
			usePodNet = false
		}
	}

	// Check for loxilb specific annotations - useExternalEndpoint
	useExternalEndpoint = m.networkConfig.UseExternalEndpoint
	if uee := svc.Annotations[useExternalEndpointAnnotation]; uee != "" {
		if uee == "yes" {
			useExternalEndpoint = true
		} else if uee == "no" {
			useExternalEndpoint = false
		}
	}

	// Check for loxilb specific annotations - enProxyProtov2Annotation
	if ppv2 := svc.Annotations[enProxyProtov2Annotation]; ppv2 != "" {
		if ppv2 == "yes" {
			enProxyProtov2 = true
		} else if ppv2 == "no" {
			enProxyProtov2 = false
		}
	}

	if eg := svc.Annotations[egressAnnotation]; eg != "" {
		if eg == "yes" {
			isEgress = true
		} else if eg == "no" {
			isEgress = false
		}
	}

	// Check for loxilb specific annotations - loxilbZoneInstance
	if zni := svc.Annotations[loxilbZoneInstance]; zni != "" {
		overrideZoneInst = zni
		if _, found := m.zoneInstRoleMap[zni]; !found {
			klog.Infof("zone-instance(%s) not found", zni)
			return errors.New("zone-instance not found")
		}
	}

	// Check for loxilb specific annotations - MatchNodeLabel
	if mnl := svc.Annotations[matchNodeLabelAnnotation]; mnl != "" {
		matchNodeLabel = mnl
	}

	// Check for loxilb specific annotations - PreferLocalPodAlways
	if plp := svc.Annotations[prefLocalPodAnnotation]; plp != "" {
		if plp == "yes" {
			prefLocal = true
		}
	}

	// Check for loxilb specific annotations - Timeout
	if to := svc.Annotations[lbTimeoutAnnotation]; to != "" {
		num, err := strconv.Atoi(to)
		if err == nil {
			timeout = num
		}
	}

	// Check for loxilb specific annotations - NAT LB Mode
	if lbm := svc.Annotations[lbModeAnnotation]; lbm != "" {
		if lbm == "hostonearm" {
			lbMode = api.LBModeHostOneArm
		} else if lbm == "fullproxy" {
			lbMode = api.LBModeFullProxy
		} else if lbm == "dsr" {
			lbMode = api.LBModeDsr
		} else if lbm == "fullnat" {
			lbMode = api.LBModeFullNat
		} else if lbm == "onearm" {
			lbMode = api.LBModeOneArm
		} else if lbm == "default" {
			lbMode = api.LBModeDefault
		} else {
			lbMode = api.LBModeNotSupported
		}
	}

	// Check for loxilb specific annotations - Liveness Check
	if lchk := svc.Annotations[livenessAnnotation]; lchk != "" {
		if lchk == "yes" {
			livenessCheck = true
		}
	}

	// Check for loxilb specific annotations - Liveness Probe Type
	if pt := svc.Annotations[probeTypeAnnotation]; pt != "" {
		if pt != "none" &&
			pt != "ping" &&
			pt != "udp" &&
			pt != "tcp" &&
			pt != "http" &&
			pt != "https" {
			probeType = ""
		} else {
			probeType = pt
		}
	}

	// Check for loxilb specific annotations - Liveness Probe Port
	if pp := svc.Annotations[probePortAnnotation]; pp != "" {
		num, err := strconv.Atoi(pp)
		if err != nil || probeType == "icmp" || probeType == "none" || probeType == "" {
			probePort = 0
		} else {
			probePort = num
		}
	}

	// Check for loxilb specific annotations - Liveness Request message
	if preq := svc.Annotations[probeReqAnnotation]; preq != "" {
		if probeType == "icmp" || probeType == "none" || probeType == "" {
			probeReq = ""
		} else {
			probeReq = preq
		}
	}

	// Check for loxilb specific annotations - Liveness Response message
	if pres := svc.Annotations[probeRespAnnotation]; pres != "" {
		if probeType == "icmp" || probeType == "none" || probeType == "" {
			probeResp = ""
		} else {
			probeResp = pres
		}
	}

	// Check for loxilb specific annotations - Liveness Probe Duration
	if pto := svc.Annotations[probeTimeoutAnnotation]; pto != "" {
		num, err := strconv.Atoi(pto)
		if err != nil {
			probeTimeout = 0
		} else {
			probeTimeout = uint32(num)
		}
	}

	// Check for loxilb specific annotations - Liveness Probe Retries
	if prt := svc.Annotations[probeRetriesAnnotation]; prt != "" {
		num, err := strconv.Atoi(prt)
		if err != nil {
			probeRetries = 0
		} else {
			probeRetries = num
		}
	}

	// Check for loxilb specific annotations - Endpoint selection algo
	eps := svc.Annotations[endPointSelAnnotation]
	switch eps {
	case "hash":
		epSelect = api.LbSelHash
	case "persist":
		epSelect = api.LbSelRrPersist
	case "lc":
		epSelect = api.LbSelLeastConnections
	case "n2":
		epSelect = api.LbSelN2
	case "n3":
		epSelect = api.LbSelN3
	case "rr":
		epSelect = api.LbSelRr
	case "roundrobin":
		epSelect = api.LbSelRr
	default:
		epSelect = api.LbSelRr
	}

	cacheKey := GenKey(svc.Namespace, svc.Name)
	lbCacheEntry, added := m.lbCache[cacheKey]

	endpointIPs, err := m.getEndpoints(svc, usePodNet, useExternalEndpoint, needMultusEP, epAddrType, matchNodeLabel)
	if err != nil {
		if !added {
			klog.V(4).Infof("endpointIPs: %v", endpointIPs)
			return errors.Wrap(err, "getEndpoints return error")
		}
	}

	if !added {
		if len(endpointIPs) <= 0 {
			return errors.New("no active endpoints")
		}

		addNewLbCacheEntryChan := make(chan *LbCacheEntry)
		defer close(addNewLbCacheEntryChan)
		go func() {
			zoneInstName := api.GenZoneInstName(m.networkConfig.Zone, 0)
			if overrideZoneInst != "" {
				zoneInstName = overrideZoneInst
			} else {
				if !hasSharedPool {
					zoneInstName = m.getZoneInstName()
				}
			}
			addNewLbCacheEntryChan <- &LbCacheEntry{
				LbMode:         lbMode,
				ActCheck:       livenessCheck,
				PrefLocal:      prefLocal,
				Timeout:        timeout,
				State:          "Added",
				NodeLabel:      matchNodeLabel,
				ProbeType:      probeType,
				ProbePort:      uint16(probePort),
				ProbeReq:       probeReq,
				ProbeResp:      probeResp,
				ProbeTimeo:     probeTimeout,
				ProbeRetries:   probeRetries,
				EpSelect:       epSelect,
				Addr:           addrType,
				SecIPs:         []string{},
				IPPool:         ipPool,
				SIPPools:       sipPools,
				Inst:           zoneInstName,
				ppv2En:         enProxyProtov2,
				LbServicePairs: make(map[string]*LbServicePairEntry),
			}
		}()

		m.lbCache[cacheKey] = <-addNewLbCacheEntryChan
		lbCacheEntry = m.lbCache[cacheKey]
		m.zoneInstSelHint++
		klog.Infof("New LbCache %s Added", cacheKey)
	} else {
		if len(endpointIPs) <= 0 {
			err := m.deleteLoadBalancer(svc.Namespace, svc.Name, false)
			if err == nil {
				m.removeAllCacheEndpoints(cacheKey)
			}
			return errors.Wrap(err, "deleteLoadBalancer return error")
		}
	}

	retIPAMOnErr := false

	//oldsvc := svc.DeepCopy()

	// Check if service has ingress IP already allocated
	ingSvcPairs, err, hasExistingEIP := m.getIngressSvcPairs(svc, lbCacheEntry)
	if err != nil {
		if !hasExistingEIP {
			retIPAMOnErr = true
		}
	}

	// set defer for deallocating IP on error
	defer func() {
		if retIPAMOnErr {
			klog.Infof("deallocateOnFailure defer function called by service %s", svc.Name)
			klog.V(4).Infof("error: %v", err)
			klog.V(4).Infof("ingSvcPairs: %v", ingSvcPairs)
			klog.V(4).Infof("hasExistingEIP: %v", hasExistingEIP)
			for i, sp := range ingSvcPairs {
				if sp.InRange && sp.IPAllocd {
					klog.Infof("Returning ip %s to free pool", sp.IPString)
					ipPool.ReturnIPAddr(sp.IPString, sp.IdentIPAM)
				}

				if i == 0 {
					for idx, ingSecIP := range m.lbCache[cacheKey].SecIPs {
						if idx < len(sipPools) {
							sipPools[idx].ReturnIPAddr(ingSecIP, sp.IdentIPAM)
						}
					}
				}
			}
		}
	}()

	if err != nil {
		return errors.Wrap(err, "getIngressSvcPairs return error")
	}

	update := false
	needDelete := false
	if len(m.lbCache[cacheKey].LbServicePairs) <= 0 {
		update = true
	} else {
		for _, lbServPair := range m.lbCache[cacheKey].LbServicePairs {
			if len(lbServPair.LbModelList) <= 0 {
				update = true
			}
		}
	}

	if addrType != m.lbCache[cacheKey].Addr {
		m.lbCache[cacheKey].Addr = addrType
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: addr-type update", cacheKey)
	}

	if matchNodeLabel != m.lbCache[cacheKey].NodeLabel {
		m.lbCache[cacheKey].NodeLabel = matchNodeLabel
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: nodelabel update", cacheKey)
	}

	if timeout != m.lbCache[cacheKey].Timeout {
		m.lbCache[cacheKey].Timeout = timeout
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: Timeout update", cacheKey)
	}

	if livenessCheck != m.lbCache[cacheKey].ActCheck {
		m.lbCache[cacheKey].ActCheck = livenessCheck
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: Liveness update", cacheKey)
	}

	if lbMode != m.lbCache[cacheKey].LbMode {
		m.lbCache[cacheKey].LbMode = lbMode
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: LbMode update", cacheKey)
	}

	if probeType != m.lbCache[cacheKey].ProbeType {
		m.lbCache[cacheKey].ProbeType = probeType
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbeType update", cacheKey)
	}

	if probePort != int(m.lbCache[cacheKey].ProbePort) {
		m.lbCache[cacheKey].ProbePort = uint16(probePort)
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbePort update", cacheKey)
	}

	if probeReq != m.lbCache[cacheKey].ProbeReq {
		m.lbCache[cacheKey].ProbeReq = probeReq
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbeReq update", cacheKey)
	}

	if probeResp != m.lbCache[cacheKey].ProbeResp {
		m.lbCache[cacheKey].ProbeResp = probeResp
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbeResp update", cacheKey)
	}

	if probeTimeout != m.lbCache[cacheKey].ProbeTimeo {
		m.lbCache[cacheKey].ProbeTimeo = probeTimeout
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbeTimeo update", cacheKey)
	}

	if probeRetries != m.lbCache[cacheKey].ProbeRetries {
		m.lbCache[cacheKey].ProbeRetries = probeRetries
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: ProbeRetries update", cacheKey)
	}

	if epSelect != m.lbCache[cacheKey].EpSelect {
		m.lbCache[cacheKey].EpSelect = epSelect
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: EpSelect update", cacheKey)
	}

	if enProxyProtov2 != m.lbCache[cacheKey].ppv2En {
		m.lbCache[cacheKey].ppv2En = enProxyProtov2
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: enProxyProtov2 update", cacheKey)
	}

	if isEgress != m.lbCache[cacheKey].egress {
		m.lbCache[cacheKey].egress = isEgress
		update = true
		if added {
			needDelete = true
		}
		klog.Infof("%s: egress update", cacheKey)
	}

	// If the user specifies a secondary IP in the annotation, update the existing secondary IP.
	if len(secIPs) > 0 {
		if !added {
			m.returnSecondaryIPs(svc, m.lbCache[cacheKey].SecIPs, m.lbCache[cacheKey].SIPPools)
			m.lbCache[cacheKey].SecIPs = secIPs
		}
	} else if len(m.lbCache[cacheKey].SecIPs) != numSecondarySvc {
		update = true
		ingSecSvcPairs, err := m.getIngressSecSvcPairs(svc, numSecondarySvc, m.lbCache[cacheKey])
		if err != nil {
			retIPAMOnErr = true
			return errors.Wrap(err, "getIngressSecSvcPairs return error")
		}

		for idx, ingSecIP := range m.lbCache[cacheKey].SecIPs {
			if idx < len(sipPools) {
				for _, sp := range m.lbCache[cacheKey].LbServicePairs {
					sipPools[idx].ReturnIPAddr(ingSecIP, sp.IdentIPAM)
				}
			}
		}

		m.lbCache[cacheKey].SecIPs = []string{}

		for _, ingSecSvcPair := range ingSecSvcPairs {
			m.lbCache[cacheKey].SecIPs = append(m.lbCache[cacheKey].SecIPs, ingSecSvcPair.IPString)
		}
	}

	if !update {
		update = m.checkUpdateEndpoints(svc, cacheKey, endpointIPs, useExternalEndpoint) || m.checkUpdateExternalIP(ingSvcPairs, svc)
	}

	if !update {
		// TODO: Some cloud providers(e.g: K3d) delete external IPs assigned by kube-loxilb, so you can reach this syntax:
		if !hasExistingEIP {
			retIPAMOnErr = true
		}
		ingSvcPairs = nil
		return nil
	} else {
		if needDelete {
			m.deleteLoadBalancer(svc.Namespace, svc.Name, false)
		}
		if added {
			for _, sp := range m.lbCache[cacheKey].LbServicePairs {
				for idx := range ingSvcPairs {
					ingSvcPair := &ingSvcPairs[idx]
					if ingSvcPair.IPString == sp.ExternalIP &&
						ingSvcPair.Port == int32(sp.Port) &&
						ingSvcPair.Protocol == sp.Protocol {
						ingSvcPair.InRange = sp.InRange
						ingSvcPair.StaticIP = sp.StaticIP
						ingSvcPair.IdentIPAM = sp.IdentIPAM
					}
				}
				delete(m.lbCache[cacheKey].LbServicePairs, GenSPKey(sp.ExternalIP, sp.Port, sp.Protocol))
			}
			m.lbCache[cacheKey].LbServicePairs = make(map[string]*LbServicePairEntry)
		}
		if !hasExistingEIP {
			svc.Status.LoadBalancer.Ingress = nil
		}
		klog.Infof("%s: Added(%v) Update(%v) needDelete(%v)", cacheKey, added, update, needDelete)
		klog.Infof("Endpoint IP Pairs %v", endpointIPs)
		klog.Infof("Secondary IP Pairs %v", m.lbCache[cacheKey].SecIPs)
	}

	privateIP, _, _ := net.ParseCIDR(m.networkConfig.PrivateCIDR)
	for _, ingSvcPair := range ingSvcPairs {
		var errChList []chan error
		lbArgs := LbArgs{
			externalIP:          ingSvcPair.IPString,
			livenessCheck:       m.lbCache[cacheKey].ActCheck,
			lbMode:              m.lbCache[cacheKey].LbMode,
			timeout:             m.lbCache[cacheKey].Timeout,
			probeType:           m.lbCache[cacheKey].ProbeType,
			probePort:           m.lbCache[cacheKey].ProbePort,
			probeReq:            m.lbCache[cacheKey].ProbeReq,
			probeResp:           m.lbCache[cacheKey].ProbeResp,
			probeTimeo:          m.lbCache[cacheKey].ProbeTimeo,
			probeRetries:        m.lbCache[cacheKey].ProbeRetries,
			sel:                 m.lbCache[cacheKey].EpSelect,
			inst:                m.lbCache[cacheKey].Inst,
			ppv2En:              m.lbCache[cacheKey].ppv2En,
			egress:              m.lbCache[cacheKey].egress,
			needMultusEP:        needMultusEP,
			usePodNetwork:       usePodNet,
			useExternalEndpoint: useExternalEndpoint,
		}
		lbArgs.secIPs = append(lbArgs.secIPs, m.lbCache[cacheKey].SecIPs...)
		lbArgs.endpointIPs = append(lbArgs.endpointIPs, endpointIPs...)
		if privateIP != nil {
			lbArgs.privateIP = privateIP.String()
		}

		sp := LbServicePairEntry{
			ExternalIP: ingSvcPair.IPString,
			Port:       uint16(ingSvcPair.Port),
			Protocol:   ingSvcPair.Protocol,
			StaticIP:   ingSvcPair.StaticIP,
			InRange:    ingSvcPair.InRange,
			IdentIPAM:  ingSvcPair.IdentIPAM,
		}

		lbModel, err := m.makeLoxiLoadBalancerModel(&lbArgs, svc, ingSvcPair.K8sSvcPort)
		if err != nil {
			retIPAMOnErr = true
			return errors.Wrap(err, "makeLoxiLoadBalancerModel return error")
		}

		for _, client := range m.LoxiClients.Clients {
			ch := make(chan error)
			go func(c *api.LoxiClient, h chan error) {
				err := m.installLB(c, lbModel, m.lbCache[cacheKey].PrefLocal)
				h <- err
			}(client, ch)

			errChList = append(errChList, ch)
		}

		var loxilbAPIErr error
		errCount := 0
		for _, errCh := range errChList {
			err := <-errCh
			if err != nil {
				loxilbAPIErr = err
				errCount++
			}
		}
		if loxilbAPIErr != nil && errCount >= len(m.LoxiClients.Clients) {
			retIPAMOnErr = true
			return fmt.Errorf("failed to add loxiLB loadBalancer - spair(%s). err: %v", GenSPKey(sp.ExternalIP, sp.Port, sp.Protocol), loxilbAPIErr)
		}

		sp.LbModelList = append(sp.LbModelList, lbModel)
		m.lbCache[cacheKey].LbServicePairs[GenSPKey(sp.ExternalIP, sp.Port, sp.Protocol)] = &sp
	}

	// Update service.Status.LoadBalancer.Ingress
	for retry := 0; retry < 5; retry++ {
		err := m.updateService(svc.Namespace, svc.Name, ingSvcPairs)
		if !k8sErrors.IsConflict(err) {
			break
		}

		time.Sleep(1 * time.Second)
	}

	return nil
}

func (m *Manager) updateService(svcNs, svcName string, ingSvcPairs []SvcPair) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	cur, err := m.kubeClient.CoreV1().Services(svcNs).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		// Service is deleted
		return err
	}

	for _, ingSvcPair := range ingSvcPairs {
		if ingSvcPair.InRange || ingSvcPair.StaticIP {
			retIPs := m.genExtIPName(ingSvcPair.IPString)
			klog.V(4).Infof("updateService get retIPs: %v", retIPs)
			for _, retIP := range retIPs {
				var retIngress corev1.LoadBalancerIngress
				validIP := net.ParseIP(retIP)
				if validIP == nil {
					retIngress = corev1.LoadBalancerIngress{Hostname: retIP}
				} else {
					retIngress = corev1.LoadBalancerIngress{IP: retIP}
				}
				if !m.checkServiceIngressIPExists(cur, retIP) {
					cur.Status.LoadBalancer.Ingress = append(cur.Status.LoadBalancer.Ingress, retIngress)
				}
			}
		}
	}

	_, err = m.kubeClient.CoreV1().Services(cur.Namespace).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
	klog.V(4).Infof("service %s is updated status: %v", cur.Name, cur.Status.LoadBalancer.Ingress)
	if err != nil {
		klog.Errorf("failed to update service %s.status. err: %v", cur.Name, err)
	}

	return err
}

func (m *Manager) updateAllLoxiLBServiceStatus() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	nsList, err := m.kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to list service in namespace err: %s", err)
		return err
	}

	for _, ns := range nsList.Items {
		svcList, err := m.kubeClient.CoreV1().Services(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Service is deleted
			continue
		}

		for _, svc := range svcList.Items {
			if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass == m.networkConfig.LoxilbLoadBalancerClass {
				update := false

				hasZeroCIDR := m.serviceHasZeroCIDRPool(&svc)
				if !hasZeroCIDR {
					continue
				}

				retIPs := m.genExtIPName("0.0.0.0")
				update = true

				svc.Status.LoadBalancer.Ingress = nil

				for _, retIP := range retIPs {
					retIngress := corev1.LoadBalancerIngress{IP: retIP}
					svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress, retIngress)
				}

				klog.Infof("service %s:%s : updating status : retIngress %v", ns.Name, svc.Name, svc.Status.LoadBalancer.Ingress)

				if update {
					_, err = m.kubeClient.CoreV1().Services(svc.Namespace).UpdateStatus(ctx, &svc, metav1.UpdateOptions{})
					if err != nil {
						klog.Errorf("failed to update service %s:%s err: %s", ns.Name, svc.Name, err)
					}
				}
			}
		}
	}

	return nil
}

func (m *Manager) deleteLoadBalancer(ns, name string, releaseAll bool) error {
	cacheKey := GenKey(ns, name)
	lbEntry, ok := m.lbCache[cacheKey]
	if !ok {
		klog.Warningf("not found service %s", name)
		return nil
	}

	var rErr error
	isError := false
	ipPool := lbEntry.IPPool
	sipPools := lbEntry.SIPPools

	for _, sp := range lbEntry.LbServicePairs {
		var errChList []chan error
		for _, lb := range sp.LbModelList {
			for _, loxiClient := range m.LoxiClients.Clients {
				ch := make(chan error)
				errChList = append(errChList, ch)

				go func(client *api.LoxiClient, ch chan error, lbModel api.LoadBalancerModel) {
					klog.Infof("loxilb-lb(%s): delete lb %v", client.Host, lbModel)
					if m.networkConfig.AppendEPs {
						lbModel.Service.Oper = api.LBOPDetach
						ch <- client.LoadBalancer().Create(context.Background(), &lbModel)
					} else {
						ch <- client.LoadBalancer().Delete(context.Background(), &lbModel)
					}
				}(loxiClient, ch, lb)
			}
		}

		var err error

		for _, errCh := range errChList {
			err = <-errCh
			if err != nil {
				isError = true
				rErr = err
				break
			}
		}
	}

	if isError {
		return fmt.Errorf("failed to delete loxiLB LoadBalancer. err: %v", rErr)
	}

	for _, sp := range lbEntry.LbServicePairs {
		if releaseAll {
			if sp.InRange {
				ipPool.ReturnIPAddr(sp.ExternalIP, sp.IdentIPAM)
			}
			for idx, ingSecIP := range lbEntry.SecIPs {
				if idx < len(sipPools) {
					sipPools[idx].ReturnIPAddr(ingSecIP, sp.IdentIPAM)
				}
			}
		}
	}

	if releaseAll {
		delete(m.lbCache, cacheKey)
	}
	return nil
}

func (m *Manager) DeleteAllLoadBalancer() {

	klog.Infof("Len %d", len(m.lbCache))
	for _, lbEntry := range m.lbCache {

		ipPool := lbEntry.IPPool
		sipPools := lbEntry.SIPPools

		for _, sp := range lbEntry.LbServicePairs {
			for _, loxiClient := range m.LoxiClients.Clients {
				for _, lb := range sp.LbModelList {
					klog.Infof("loxilb(%s): deleteAll lb %v", loxiClient.Host, lb)
					loxiClient.LoadBalancer().Delete(context.Background(), &lb)
				}
				if sp.InRange {
					ipPool.ReturnIPAddr(sp.ExternalIP, sp.IdentIPAM)
				}
				for idx, ingSecIP := range lbEntry.SecIPs {
					if idx < len(sipPools) {
						sipPools[idx].ReturnIPAddr(ingSecIP, sp.IdentIPAM)
					}
				}
			}
		}
	}
	m.lbCache = nil
}

func (m *Manager) installLB(c *api.LoxiClient, lb api.LoadBalancerModel, prefLocal bool) error {
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	Model := lb
	model := &Model

	// Optimization for local Preference
	if prefLocal {
		model.Endpoints = nil
		for _, ep := range lb.Endpoints {
			if ep.EndpointIP == c.Host {
				model.Endpoints = append(model.Endpoints, ep)
			}
		}
		if len(model.Endpoints) <= 0 {
			model.Endpoints = lb.Endpoints
		}
	}
	if err = c.LoadBalancer().Create(ctx, model); err != nil {
		if !strings.Contains(err.Error(), "exist") {
			klog.Errorf("failed to create load-balancer(%s) :%v", c.Url, err)
		} else {
			err = nil
		}
	}

	if err == nil {
		klog.Infof("loxilb-lb(%s): add lb %v", c.Host, lb)
	}

	return err
}

// getNodeEndpointsWithLabel returns the IP list of nodes available with match labels
func (m *Manager) getNodeEndpointsWithLabel(addrType string, matchLabel string) ([]string, error) {
	klog.V(4).Infof("getNodeEndpointsWithLabel: label %s", matchLabel)
	req, err := labels.NewRequirement(matchLabel, selection.Exists, []string{})
	if err != nil {
		klog.Infof("getNodeEndpointsWithLabel: failed to make label requirement. err: %v", err)
		return nil, err
	}

	nodes, err := m.nodeLister.List(labels.NewSelector().Add(*req))
	if err != nil {
		klog.Infof("getNodeEndpointsWithLabel: failed to get nodeList. err: %v", err)
		return nil, err
	}

	var endpoints []string
	for _, node := range nodes {
		addr, err := m.getNodeAddress(*node, addrType)
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}
		klog.V(4).Infof("getNodeEndpointsWithLabel: found node %s with label %s", addr, matchLabel)
		endpoints = append(endpoints, addr)
	}

	return endpoints, nil
}

// getNodeEndpointsWithLabelWithKey returns the IP list of nodes available with match labels
func (m *Manager) getNodeEndpointsWithLabelWithKey(addrType string, key, matchLabel string) ([]string, error) {
	klog.V(4).Infof("getNodeEndpointsWithLabelWithKey: label %s:%s", key, matchLabel)
	req, err := labels.NewRequirement(key, selection.In, []string{matchLabel})
	if err != nil {
		klog.Infof("getNodeEndpointsWithLabelWithKey: failed to make label requirement. err: %v", err)
		return nil, err
	}

	nodes, err := m.nodeLister.List(labels.NewSelector().Add(*req))
	if err != nil {
		klog.Infof("getNodeEndpointsWithLabelWithKey: failed to get nodeList. err: %v", err)
		return nil, err
	}

	var endpoints []string
	for _, node := range nodes {
		addr, err := m.getNodeAddress(*node, addrType)
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}
		klog.V(4).Infof("getNodeEndpointsWithLabelWithKey: found node %s with label %s:%s", addr, key, matchLabel)
		endpoints = append(endpoints, addr)
	}

	return endpoints, nil
}

// getEndpoints return LB's endpoints IP list.
// If podEP is true, return multus endpoints list.
// If false, return worker nodes IP list.
func (m *Manager) getEndpoints(svc *corev1.Service, usePodNet, useExternalEndpoint, useMultusNet bool, addrType, matchNodeLabel string) ([]string, error) {
	if useMultusNet {
		//klog.Infof("getEndpoints: Pod end-points")
		return m.getMultusEndpoints(svc, addrType)
	}

	var matchNodeList []string
	var err error
	if matchNodeLabel != "" {
		matchNodeList, err = m.getNodeEndpointsWithLabel(addrType, matchNodeLabel)
		if err != nil {
			return nil, err
		}
		if len(matchNodeList) <= 0 {
			matchNodeList = append(matchNodeList, "xdeadbeefx")
		}
	}

	if usePodNet || useExternalEndpoint {
		klog.V(4).Infof("usePodNet: %v. useExternalEndpoint: %v", usePodNet, useExternalEndpoint)
		return k8s.GetServiceEndPoints(m.kubeClient, svc, addrType, matchNodeList)
		//return k8s.GetServicePodEndpoints(m.kubeClient, svc, addrType, matchNodeList)
	}

	if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
		//klog.Infof("getEndpoints: Traffic Policy Local %d", len(matchNodeList))
		return k8s.GetServiceLocalEndpoints(m.kubeClient, svc, addrType, matchNodeList)
	}
	return m.getNodeEndpoints(addrType, matchNodeList)
}

// getNodeEndpoints returns the IP list of nodes available as nodePort service.
func (m *Manager) getNodeEndpoints(addrType string, nodeMatchList []string) ([]string, error) {
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

	return m.getEndpointsForLB(nodes, addrType, nodeMatchList), nil
}

// getLocalEndpoints returns the IP list of the Pods connected to the multus network.
func (m *Manager) getLocalEndpoints(svc *corev1.Service, addrType string) ([]string, error) {
	netListStr, ok := svc.Annotations[LoxiMultusServiceAnnotation]
	if !ok {
		return nil, errors.New("not found multus annotations")
	}
	netList := strings.Split(netListStr, ",")

	return k8s.GetMultusEndpoints(m.kubeClient, svc, netList, addrType)
}

// getMultusEndpoints returns the IP list of the Pods connected to the multus network.
func (m *Manager) getMultusEndpoints(svc *corev1.Service, addrType string) ([]string, error) {
	netListStr, ok := svc.Annotations[LoxiMultusServiceAnnotation]
	if !ok {
		return nil, errors.New("not found multus annotations")
	}
	netList := strings.Split(netListStr, ",")

	return k8s.GetMultusEndpoints(m.kubeClient, svc, netList, addrType)
}

func (m *Manager) getNodeAddress(node corev1.Node, addrType string) (string, error) {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			if condition.Status != corev1.ConditionTrue {
				return "", fmt.Errorf("node %s %sstatus = %s", node.Name, string(condition.Type), string(condition.Status))
			}
		}
	}

	addrs := node.Status.Addresses
	if len(addrs) == 0 {
		return "", errors.New("no address found for host")
	}

	for _, addr := range addrs {
		if addr.Type == corev1.NodeInternalIP {
			if k8s.AddrInFamily(addrType, addr.Address) {
				return addr.Address, nil
			}
		}
	}

	if k8s.AddrInFamily(addrType, addrs[0].Address) {
		return addrs[0].Address, nil
	}

	return "", errors.New("no address with family found for host")
}

func (m *Manager) getEndpointsForLB(nodes []*corev1.Node, addrType string, nodeMatchList []string) []string {
	var endpoints []string
	for _, node := range nodes {
		addr, err := m.getNodeAddress(*node, addrType)
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}
		if len(nodeMatchList) > 0 && !k8s.MatchNodeinNodeList(addr, nodeMatchList) {
			continue
		}
		endpoints = append(endpoints, addr)
	}

	return endpoints
}

func (m *Manager) checkUpdateExternalIP(ingSvcPairs []SvcPair, svc *corev1.Service) bool {
	for _, ingSvcPair := range ingSvcPairs {
		if ingSvcPair.InRange || ingSvcPair.StaticIP {
			retIPs := m.genExtIPName(ingSvcPair.IPString)
			for _, retIP := range retIPs {
				if !m.checkServiceIngressIPExists(svc, retIP) {
					klog.V(4).Infof("checkUpdateExternalIP: ingSvcPair %v has external IP but service %s has no IP. need update.", ingSvcPair, svc.Name)
					return true
				}
			}
		}
	}

	return false
}

func (m *Manager) checkUpdateEndpoints(svc *corev1.Service, cacheKey string, endpointIPs []string, matchPorts bool) bool {
	var update bool

	if matchPorts {
		loxiEndpointModelList := []api.LoadBalancerEndpoint{}
		var tports []int
		var err error
		tports, err = k8s.GetServiceEndPointsPorts(m.kubeClient, svc)
		if err != nil {
			return true
		}
		for _, endpoint := range endpointIPs {
			for _, tport := range tports {
				loxiEndpointModelList = append(loxiEndpointModelList, api.LoadBalancerEndpoint{
					EndpointIP: endpoint,
					TargetPort: uint16(tport),
					Weight:     1,
				})
			}
		}

		for _, sp := range m.lbCache[cacheKey].LbServicePairs {
			// Check if external-endpoint list has changed
			for _, lb := range sp.LbModelList {
				if len(loxiEndpointModelList) == len(lb.Endpoints) {
					for _, endpoint := range loxiEndpointModelList {
						found := false
						for _, oldEp := range lb.Endpoints {
							if oldEp.EndpointIP == endpoint.EndpointIP &&
								oldEp.TargetPort == endpoint.TargetPort {
								found = true
								break
							}
						}
						if !found {
							update = true
						}
					}
				} else {
					update = true
				}
			}
			if update {
				klog.Infof("%s: Ext-Endpoint update", cacheKey)
			}
		}
		return update
	}

	for _, sp := range m.lbCache[cacheKey].LbServicePairs {
		// Check if endpoint list has changed
		for _, lb := range sp.LbModelList {
			if len(endpointIPs) == len(lb.Endpoints) {
				nEps := 0
				for _, ep := range endpointIPs {
					found := false
					for _, oldEp := range lb.Endpoints {
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
		if update {
			klog.Infof("%s: Endpoint update", cacheKey)
		}
	}

	return update
}

func (m *Manager) checkServiceIngressIPExists(service *corev1.Service, newIngress string) bool {
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			if ingress.IP == newIngress {
				return true
			}
		}
		if ingress.Hostname != "" {
			if ingress.Hostname == newIngress {
				return true
			}
		}
	}

	return false
}

func (m *Manager) serviceHasZeroCIDRPool(service *corev1.Service) bool {
	poolName := defaultPoolName

	// Check for loxilb specific annotations - poolName
	if pn := service.Annotations[PoolNameAnnotation]; pn != "" {
		poolName = pn
	}

	hasZeroCIDR := false
	for _, pool := range m.networkConfig.ExternalCIDRPoolDefs {
		poolStrSlice := strings.Split(pool, "=")
		if len(poolStrSlice) != 2 {
			break
		}

		if poolName != poolStrSlice[0] {
			continue
		}

		_, ipn, err := net.ParseCIDR(poolStrSlice[1])
		if err != nil {
			break
		}

		if ipn.IP.String() == "0.0.0.0" {
			if lba := service.Annotations[lbAddressAnnotation]; lba != "" {
				if lba == "ipv6" || lba == "ip6" || lba == "nat66" {
					break
				}
			}
			hasZeroCIDR = true
		}
	}

	return hasZeroCIDR
}

func (m *Manager) getServiceIngressIPs(service *corev1.Service) []string {
	var ingressIPs []string

	hasZeroCIDR := m.serviceHasZeroCIDRPool(service)

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		var ingressIP string
		if ingress.IP != "" {
			if hasZeroCIDR {
				ingressIP = "0.0.0.0"
			} else {
				ingressIP = ingress.IP
			}
		} else if ingress.Hostname != "" {
			if ingress.Hostname == "llbanyextip" || hasZeroCIDR {
				ingressIP = "0.0.0.0"
			} else {
				llbHost := strings.Split(ingress.Hostname, "-")

				if len(llbHost) < 2 {
					if net.ParseIP(llbHost[0]) != nil {
						ingressIP = llbHost[0]
					}
				} else {
					if llbHost[0] == m.networkConfig.Zone {
						if net.ParseIP(llbHost[1]) != nil {
							ingressIP = llbHost[1]
						} else if len(llbHost) > 2 {
							ipStrSlice := llbHost[1:]
							if len(ipStrSlice) > 0 {
								ipStr := strings.Join(ipStrSlice, ":")
								if net.ParseIP(ipStr) != nil {
									ingressIP = ipStr
								}
							}
						}
					} else {
						continue
					}
				}
			}
		}

		ingressIPs = append(ingressIPs, ingressIP)
	}

	return ingressIPs
}

func (m *Manager) getServiceExternalIPs(service *corev1.Service) []string {
	return service.Spec.ExternalIPs
}

func (m *Manager) getServiceLoxiStaticIP(service *corev1.Service) string {
	if staticIPStr := service.Annotations[staticIPAnnotation]; staticIPStr != "" {
		if net.ParseIP(staticIPStr) == nil {
			klog.Errorf("%s annotation has invalid IP (%s)", staticIPAnnotation, staticIPStr)
		} else {
			return staticIPStr
		}
	}

	return ""
}

func (m *Manager) getLBServiceExternalIPs(service *corev1.Service) []string {
	var lbExternalIPs []string
	if ingressIPs := m.getServiceIngressIPs(service); len(ingressIPs) > 0 {
		lbExternalIPs = append(lbExternalIPs, ingressIPs...)
	}

	if extIPs := m.getServiceExternalIPs(service); len(extIPs) > 0 {
		lbExternalIPs = append(lbExternalIPs, extIPs...)
	}

	if staticIPStr := m.getServiceLoxiStaticIP(service); staticIPStr != "" {
		lbExternalIPs = append(lbExternalIPs, staticIPStr)
	}

	return lbExternalIPs
}

func (m *Manager) getLBIngressSvcPairs(service *corev1.Service) []SvcPair {
	var spairs []SvcPair

	extIPs := m.getLBServiceExternalIPs(service)
	for _, extIP := range extIPs {
		for _, port := range service.Spec.Ports {
			sp := SvcPair{extIP, port.Port, strings.ToLower(string(port.Protocol)), false, true, "", false, port}
			spairs = append(spairs, sp)
		}
	}

	return spairs
}

// getIngressSvcPairs check validation if service have ingress or externalIPs already.
// If service have no ingress IP, assign new IP in IP pool
func (m *Manager) getIngressSvcPairs(service *corev1.Service, lbCacheEntry *LbCacheEntry) ([]SvcPair, error, bool) {
	var sPairs []SvcPair
	inSPairs := m.getLBIngressSvcPairs(service)
	hasExtIPAllocated := false
	poolReserved := false
	cacheKey := GenKey(service.Namespace, service.Name)

	ipPool := lbCacheEntry.IPPool

	// k8s service has ingress IP already
	if len(inSPairs) >= 1 {
		poolReserved = false
		klog.V(4).Infof("getIngressSvcPairs: service %s has servicePairs: %v", cacheKey, inSPairs)
		klog.V(4).Infof("getIngressSvcPairs: service %s has externalIP: %v", cacheKey, service.Status.LoadBalancer.Ingress)
	checkSvcPortLoop:
		for _, inSPair := range inSPairs {

			hasExtIPAllocated = true
			inRange := false
			identStr := ""
			for _, sp := range lbCacheEntry.LbServicePairs {
				if GenSPKey(inSPair.IPString, uint16(inSPair.Port), inSPair.Protocol) == GenSPKey(sp.ExternalIP, sp.Port, sp.Protocol) {
					oldsp := SvcPair{sp.ExternalIP, int32(sp.Port), sp.Protocol, sp.InRange, sp.StaticIP, sp.IdentIPAM, false, inSPair.K8sSvcPort}
					sPairs = append(sPairs, oldsp)
					klog.V(4).Infof("getIngressSvcPairs: LB cache %s already has servicePairs: %v", cacheKey, sp)
					continue checkSvcPortLoop
				}
			}

			if !poolReserved {
				inRange, _, identStr = ipPool.CheckAndReserveIP(inSPair.IPString, cacheKey, uint32(inSPair.Port), inSPair.Protocol)
			} else {
				identStr = tk.IPAMNoIdent
				if ipPool.Shared {
					identStr = tk.MakeIPAMIdent("", uint32(inSPair.Port), inSPair.Protocol)
				}
			}
			newsp := SvcPair{inSPair.IPString, inSPair.Port, inSPair.Protocol, inRange, true, identStr, !poolReserved, inSPair.K8sSvcPort}
			klog.V(4).Infof("getIngressSvcPairs: LB cache %s is added servicePairs: %v", cacheKey, newsp)
			sPairs = append(sPairs, newsp)
			if inRange && !poolReserved {
				poolReserved = true
			}
		}
	}

	var newIP net.IP = nil
	identIPAM := ""

	// If hasExtIPAllocated is false, that means k8s service has no ingress IP
	if !hasExtIPAllocated {
		poolReserved = false
		klog.V(4).Infof("getIngressSvcPairs: service %s has no externalIP: %v", cacheKey, service.Status.LoadBalancer.Ingress)
	checkServicePortLoop:
		for _, port := range service.Spec.Ports {
			proto := strings.ToLower(string(port.Protocol))
			portNum := port.Port

			for _, sp := range lbCacheEntry.LbServicePairs {
				if sp.Port == uint16(portNum) && proto == sp.Protocol {
					oldsp := SvcPair{sp.ExternalIP, int32(sp.Port), sp.Protocol, sp.InRange, sp.StaticIP, sp.IdentIPAM, false, port}
					sPairs = append(sPairs, oldsp)
					klog.V(4).Infof("getIngressSvcPairs: LB cache %s already has servicePairs: %v", cacheKey, sp)
					continue checkServicePortLoop
				}
			}

			if !poolReserved {
				newIP, identIPAM = ipPool.GetNewIPAddr(cacheKey, uint32(portNum), proto)
				if newIP == nil {
					errMsg := fmt.Sprintf("failed to generate external IP. %s:%d:%s already used for %s", cacheKey, portNum, proto, identIPAM)
					klog.Errorf(errMsg)
					klog.Exit("kube-loxilb cant run optimally anymore")
					return nil, errors.New(errMsg), hasExtIPAllocated
				}

				klog.V(4).Infof("getIngressSvcPairs: service %s :%s generated new externalIP: %s", cacheKey, identIPAM, newIP.String())

			} else {
				identIPAM = tk.IPAMNoIdent
				if ipPool.Shared {
					identIPAM = tk.MakeIPAMIdent("", uint32(portNum), proto)
				}

				klog.V(4).Infof("getIngressSvcPairs: service %s :%s reuse externalIP: %s", cacheKey, identIPAM, newIP.String())
			}

			newsp := SvcPair{newIP.String(), portNum, proto, true, false, identIPAM, !poolReserved, port}
			sPairs = append(sPairs, newsp)
			if !poolReserved {
				poolReserved = true
			}
		}
	}
	return sPairs, nil, hasExtIPAllocated
}

// returnSecondaryIPs
func (m *Manager) returnSecondaryIPs(service *corev1.Service, secIPs []string, sipPools []*ippool.IPPool) error {
	cacheKey := GenKey(service.Namespace, service.Name)

	for idx, ingSecIP := range secIPs {
		if idx < len(sipPools) {
			for _, sp := range m.lbCache[cacheKey].LbServicePairs {
				sipPools[idx].ReturnIPAddr(ingSecIP, sp.IdentIPAM)
			}
		}
	}

	return nil
}

// getIngressSecSvcPairs returns a set of secondary IPs
func (m *Manager) getIngressSecSvcPairs(service *corev1.Service, numSecondary int, lbCacheEntry *LbCacheEntry) ([]SvcPair, error) {
	var sPairs []SvcPair

	sipPools := lbCacheEntry.SIPPools

	if len(sipPools) < numSecondary {
		klog.Errorf("failed to generate external secondary IP. No IP pools")
		return sPairs, errors.New("failed to generate external secondary IP. No IP pools")
	}

	cacheKey := GenKey(service.Namespace, service.Name)
	poolReserved := false

	for i := 0; i < numSecondary; i++ {
		poolReserved = false
		for _, port := range service.Spec.Ports {
			pool := sipPools[i]
			proto := strings.ToLower(string(port.Protocol))
			portNum := port.Port

			if !poolReserved {
				newIP, identIPAM := pool.GetNewIPAddr(cacheKey, uint32(portNum), proto)
				if newIP == nil {
					for j := 0; j < i; j++ {
						rpool := sipPools[j]
						rpool.ReturnIPAddr(sPairs[j].IPString, sPairs[j].IdentIPAM)
					}
					errMsg := fmt.Sprintf("failed to generate secondary external IP. %s:%d:%s already used for %s", cacheKey, portNum, proto, identIPAM)
					klog.Errorf(errMsg)
					klog.Exit("kube-loxilb cant run optimally anymore")
					return nil, errors.New(errMsg)
				}
				newsp := SvcPair{newIP.String(), portNum, proto, true, false, identIPAM, true, port}
				sPairs = append(sPairs, newsp)
				poolReserved = true
			}
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

func (m *Manager) makeLoxiLoadBalancerModel(lbArgs *LbArgs, svc *corev1.Service, port corev1.ServicePort) (api.LoadBalancerModel, error) {
	loxiEndpointModelList := []api.LoadBalancerEndpoint{}
	loxiSecIPModelList := []api.LoadBalancerSecIp{}
	loxiLbAllowedSrcIpList := []api.LbAllowedSrcIPArg{}

	lbModeSvc := api.LbMode(m.networkConfig.SetLBMode)
	lbOper := api.LBOPAdd

	if m.networkConfig.AppendEPs {
		lbOper = api.LBOPAttach
	}

	if len(lbArgs.endpointIPs) > 0 {

		for _, endpoint := range lbArgs.endpointIPs {

			var tports []int
			var err error
			if lbArgs.needMultusEP || lbArgs.usePodNetwork {
				tports, err = k8s.GetServicePortIntValue(m.kubeClient, svc, port)
				if err != nil {
					return api.LoadBalancerModel{}, err
				}
			} else if lbArgs.useExternalEndpoint {
				tports, err = k8s.GetServiceEndPointsPorts(m.kubeClient, svc)
				if err != nil {
					return api.LoadBalancerModel{}, err
				}
			} else {
				tports = append(tports, int(port.NodePort))
			}

			if len(tports) <= 0 {
				return api.LoadBalancerModel{}, errors.New("no endpoints to make lbmodel")
			}

			for _, tport := range tports {
				loxiEndpointModelList = append(loxiEndpointModelList, api.LoadBalancerEndpoint{
					EndpointIP: endpoint,
					TargetPort: uint16(tport),
					Weight:     1,
				})
			}
		}
	}

	if len(lbArgs.secIPs) > 0 {
		for _, secIP := range lbArgs.secIPs {
			loxiSecIPModelList = append(loxiSecIPModelList, api.LoadBalancerSecIp{SecondaryIP: secIP})
		}
	}

	if m.networkConfig.Monitor {
		lbArgs.livenessCheck = true
	}

	if lbArgs.lbMode >= 0 {
		lbModeSvc = api.LbMode(lbArgs.lbMode)
	}

	bgpMode := false
	if m.networkConfig.SetBGP != 0 {
		bgpMode = true
	}

	// for support k8s service.spec.loadBalancerSourceRanges
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		for _, allowedSrc := range svc.Spec.LoadBalancerSourceRanges {
			if _, _, err := net.ParseCIDR(allowedSrc); err == nil {
				loxiLbAllowedSrcIpList = append(loxiLbAllowedSrcIpList, api.LbAllowedSrcIPArg{Prefix: allowedSrc})
			}
		}
	}

	return api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP:   lbArgs.externalIP,
			PrivateIP:    lbArgs.privateIP,
			Port:         uint16(port.Port),
			Protocol:     strings.ToLower(string(port.Protocol)),
			BGP:          bgpMode,
			Mode:         lbModeSvc,
			Oper:         lbOper,
			Monitor:      lbArgs.livenessCheck,
			Timeout:      uint32(lbArgs.timeout),
			Managed:      true,
			ProbeType:    lbArgs.probeType,
			ProbePort:    lbArgs.probePort,
			ProbeReq:     lbArgs.probeReq,
			ProbeResp:    lbArgs.probeResp,
			ProbeTimeout: lbArgs.probeTimeo,
			ProbeRetries: int32(lbArgs.probeRetries),
			PpV2:         lbArgs.ppv2En,
			Egress:       lbArgs.egress,
			Sel:          lbArgs.sel,
			Name:         fmt.Sprintf("%s_%s:%s", svc.Namespace, svc.Name, lbArgs.inst),
		},
		SrcIPs:       loxiLbAllowedSrcIpList,
		SecondaryIPs: loxiSecIPModelList,
		Endpoints:    loxiEndpointModelList,
	}, nil
}

func (m *Manager) makeLoxiLBCIStatusModel(instance, vip string, client *api.LoxiClient) (api.CIStatusModel, error) {

	state := "BACKUP"
	inst, found := client.InstRoles[instance]
	if found {
		if inst.MasterLB {
			state = "MASTER"
		}
	}

	if vip == "" {
		vip = "0.0.0.0"
	}
	return api.CIStatusModel{
		Instance: instance,
		State:    state,
		Vip:      vip,
	}, nil
}

func (m *Manager) makeLoxiLBBGPGlobalModel(localAS int, selfID string, setNHSelf bool, lPort uint16) (api.BGPGlobalConfig, error) {

	port := lPort
	if lPort == 0 {
		port = 179
	}

	return api.BGPGlobalConfig{
		LocalAs:    int64(localAS),
		RouterID:   selfID,
		SetNHSelf:  setNHSelf,
		ListenPort: port,
	}, nil
}

func (m *Manager) makeLoxiLBBGNeighModel(remoteAS int, IPString string, rPort uint16, mHopEn bool) (api.BGPNeigh, error) {

	port := rPort
	if rPort == 0 {
		port = 179
	}

	return api.BGPNeigh{
		RemoteAs:    int64(remoteAS),
		IPAddress:   IPString,
		RemotePort:  int64(port),
		SetMultiHop: mHopEn,
	}, nil
}

func (m *Manager) addIngress(service *corev1.Service, newIP net.IP) {
	service.Status.LoadBalancer.Ingress =
		append(service.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: newIP.String()})
}

func (m *Manager) DiscoverLoxiLBServices(loxiLBAliveCh chan *api.LoxiClient, loxiLBDeadCh chan struct{}, loxiLBPurgeCh chan *api.LoxiClient, excludeList []string) {
	var tmploxilbClients []*api.LoxiClient
	var matchNodeList []string
	if m.networkConfig.Zone != "" && m.networkConfig.Zone != "llb" {
		matchNodeList, _ = m.getNodeEndpointsWithLabelWithKey("ipv4", loxilbZoneLabelKey, m.networkConfig.Zone)
		if len(matchNodeList) <= 0 {
			matchNodeList = append(matchNodeList, "xdeadbeefx")
		}
	}

	// DNS lookup (not used now)
	// ips, err := net.LookupIP("loxilb-lb-service")
	ips, err := k8s.GetLoxilbServiceEndPoints(m.kubeClient, "loxilb-lb-service", "", matchNodeList)
	if err != nil {
		klog.Infof("loxilb-service failed: %s", err)
		ips = []string{}
	}

	if len(ips) != len(m.LoxiClients.Clients) {
		klog.Infof("loxilb-service end-points:  %v", ips)
	}

	for _, v := range m.LoxiClients.Clients {
		v.Purge = true
		for _, ip := range ips {
			if v.Host == ip {
				v.Purge = false
			}
		}
	}

	for _, ip := range ips {
		found := false
		noRole := false

		for _, eNode := range excludeList {
			if eNode == ip {
				noRole = true
				break
			}
		}
		for _, v := range m.LoxiClients.Clients {
			if v.Host == ip {
				found = true
			}
		}
		if !found {
			client, err2 := api.NewLoxiClient("http://"+ip+":11111", loxiLBAliveCh, loxiLBDeadCh, false, noRole, "", m.networkConfig.Zone, m.networkConfig.NumZoneInst)
			if err2 != nil {
				continue
			}
			tmploxilbClients = append(tmploxilbClients, client)
		}
	}
	if len(tmploxilbClients) > 0 {
		m.LoxiClients.Clients = append(m.LoxiClients.Clients, tmploxilbClients...)
	}
	tmp := m.LoxiClients.Clients[:0]
	for _, v := range m.LoxiClients.Clients {
		if !v.Purge {
			tmp = append(tmp, v)
		} else {
			v.StopLoxiHealthCheckChan()
			klog.Infof("loxilb-service(%v) removed", v.Host)
			loxiLBPurgeCh <- v
		}
	}
	m.LoxiClients.Clients = tmp
}

func (m *Manager) DiscoverLoxiLBPeerServices(loxiLBAliveCh chan *api.LoxiClient, loxiLBDeadCh chan struct{}, loxiLBPurgeCh chan *api.LoxiClient) {
	var tmploxilbPeerClients []*api.LoxiClient
	ips, err := k8s.GetLoxilbServiceEndPoints(m.kubeClient, "loxilb-peer-service", "", []string{})
	if len(ips) > 0 {
		klog.Infof("loxilb-peer-service end-points:  %v", ips)
	}
	if err != nil {
		ips = []string{}
	}

	for _, v := range m.LoxiPeerClients.Clients {
		v.Purge = true
		for _, ip := range ips {
			if v.Host == ip {
				v.Purge = false
			}
		}
	}

	for _, ip := range ips {
		found := false
		for _, v := range m.LoxiPeerClients.Clients {
			if v.Host == ip {
				found = true
			}
		}
		if !found {
			client, err2 := api.NewLoxiClient("http://"+ip+":11111", loxiLBAliveCh, loxiLBDeadCh, true, true, "", m.networkConfig.Zone, m.networkConfig.NumZoneInst)
			if err2 != nil {
				continue
			}
			tmploxilbPeerClients = append(tmploxilbPeerClients, client)
		}
	}
	if len(tmploxilbPeerClients) > 0 {
		m.LoxiPeerClients.Clients = append(m.LoxiPeerClients.Clients, tmploxilbPeerClients...)
	}
	tmp1 := m.LoxiPeerClients.Clients[:0]
	for _, v := range m.LoxiPeerClients.Clients {
		if !v.Purge {
			tmp1 = append(tmp1, v)
		} else {
			klog.Infof("loxilb-peer-service(%v) removed", v.Host)
			v.StopLoxiHealthCheckChan()
			loxiLBPurgeCh <- v
		}
	}
	m.LoxiPeerClients.Clients = tmp1
}

func (m *Manager) removeAllCacheEndpoints(cacheKey string) {
	for _, sp := range m.lbCache[cacheKey].LbServicePairs {
		for i := range sp.LbModelList {
			sp.LbModelList[i].Endpoints = nil
		}
	}
	klog.V(4).Infof("service %s loxilb rule's endpoints are removed", cacheKey)
}

func (m *Manager) SelectInstLoxiLBRoles(instName string, selhint int) (bool, int) {
	reElect := false
	hasMaster := false

	for i := range m.LoxiClients.Clients {
		v := m.LoxiClients.Clients[i]
		vi := v.InstRoles[instName]
		if vi == nil {
			return false, 0
		}
		if vi.MasterLB && !v.IsAlive {
			vi.MasterLB = false
			reElect = true
		} else if vi.MasterLB {
			hasMaster = true
		}
	}
	sel := -1
	selMaster := false
	if reElect || !hasMaster {
		nproc := 0
		for i := selhint; nproc < len(m.LoxiClients.Clients); i++ {
			nproc++
			if i >= len(m.LoxiClients.Clients) {
				i = 0
			}
			v := m.LoxiClients.Clients[i]
			vi := v.InstRoles[instName]
			if v.NoRole {
				continue
			}
			if selMaster {
				vi.MasterLB = false
				continue
			}
			if v.IsAlive {
				vi.MasterLB = true
				selMaster = true
				sel = i
				klog.Infof("loxilb-lb(%v): %s set-role master", instName, v.Host)
			}
		}
	}
	return selMaster, sel
}

func (m *Manager) ResetRolesOnNeedRebalanceInstLoxiLBRoles() bool {

	numInstRoles := len(m.zoneInstRoleMap)
	maxMasterPerClient := 0
	activeClients := 0
	for _, client := range m.LoxiClients.Clients {
		if client.IsAlive {
			activeClients++
		}
		masterRoles := 0
		for _, val := range client.InstRoles {
			if val.MasterLB {
				masterRoles++
			}
		}
		if masterRoles > maxMasterPerClient {
			maxMasterPerClient = masterRoles
		}
	}

	if activeClients >= numInstRoles && maxMasterPerClient >= numInstRoles && m.networkConfig.ExclIPAM {
		for _, client := range m.LoxiClients.Clients {
			for _, val := range client.InstRoles {
				if val.MasterLB {
					val.MasterLB = false
				}
			}
		}
		klog.Infof("set-roles need rebalance (%d:%d)", activeClients, maxMasterPerClient)
		return true
	}

	return false
}

func (m *Manager) SelectLoxiLBRoles(sendSigCh bool, loxiLBSelMasterEvent chan bool) {
	selMaster := false
	pselhint := 0
	if m.networkConfig.SetRoles != "" {
		m.ResetRolesOnNeedRebalanceInstLoxiLBRoles()
		for inst, val := range m.zoneInstRoleMap {
			sel := false
			if pselhint == 0 {
				pselhint = val.instID
			} else {
				pselhint++
			}
			sel, pselhint = m.SelectInstLoxiLBRoles(inst, pselhint)
			if sel {
				selMaster = true
			}
		}
	}
	if selMaster {
		m.ElectionRunOnce = true
		if sendSigCh {
			loxiLBSelMasterEvent <- true
		}
	}
}

func (m *Manager) checkHandleBGPCfgErrors(loxiAliveCh chan *api.LoxiClient, peer *api.LoxiClient, err error) {
	if strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "deadline") {
		time.Sleep(2 * time.Second)
		if !peer.DoBGPCfg {
			klog.Infof(" client (%s) requeued ", peer.Host)
			peer.DoBGPCfg = true
			loxiAliveCh <- peer
		}
	}
}

func (m *Manager) manageLoxiLbLifeCycle(stopCh <-chan struct{}) {
loop:
	for {
		select {
		case <-stopCh:
			break loop
		case <-m.instAddrApplyCh:
			m.updateAllLoxiLBServiceStatus()
		case <-m.ClientSelMasterCh:
			for _, lc := range m.LoxiClients.Clients {
				if !lc.IsAlive {
					continue
				}
				for instName, vi := range lc.InstRoles {
					firstZoneName := api.GenZoneInstName(m.networkConfig.Zone, 0)
					if instName == firstZoneName && m.networkConfig.Zone != api.CIDefaultZone {
						defaultZoneName := api.GenZoneInstName(api.CIDefaultZone, 0)
						cisModel, err := m.makeLoxiLBCIStatusModel(instName, m.networkConfig.SetRoles, lc)
						cisModel.Instance = defaultZoneName
						if err == nil {
							for retry := 0; retry < 5; retry++ {
								err = func(cisModel *api.CIStatusModel) error {
									ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
									defer cancel()
									return lc.CIStatus().Create(ctx, cisModel)
								}(&cisModel)
								if err == nil {
									klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - OK", lc.Host, defaultZoneName, vi.MasterLB)
									break
								} else {
									klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - failed(%d)", lc.Host, defaultZoneName, vi.MasterLB, retry)
									time.Sleep(1 * time.Second)
								}
							}
						}
					}
					cisModel, err := m.makeLoxiLBCIStatusModel(instName, m.networkConfig.SetRoles, lc)
					if err == nil {
						for retry := 0; retry < 5; retry++ {
							err = func(cisModel *api.CIStatusModel) error {
								ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
								defer cancel()
								return lc.CIStatus().Create(ctx, cisModel)
							}(&cisModel)
							if err == nil {
								klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - OK", lc.Host, instName, vi.MasterLB)
								break
							} else {
								klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - failed(%d)", lc.Host, instName, vi.MasterLB, retry)
								time.Sleep(1 * time.Second)
							}
						}
					}
				}
			}
		case purgedClient := <-m.ClientPurgeCh:
			klog.Infof("loxilb-lb(%s): purged", purgedClient.Host)
			if m.networkConfig.SetBGP != 0 {
				deleteNeigh := func(client *api.LoxiClient, neighIP string, remoteAs int) error {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
					defer cancel()
					return client.BGP().DeleteNeigh(ctx, neighIP, remoteAs)
				}

				for _, otherClient := range m.LoxiClients.Clients {
					if purgedClient.Host == otherClient.Host {
						continue
					}

					err := deleteNeigh(otherClient, purgedClient.Host, int(m.networkConfig.SetBGP))
					klog.Infof("loxilb-lb(%s): delete neigh peer %s", otherClient.Host, purgedClient.Host)
					if err != nil {
						klog.Errorf("loxilb-lb(%s): delete neigh peer error: %v", otherClient.Host, err)
					}
				}
			}
		case aliveClient := <-m.ClientAliveCh:
			aliveClient.DoBGPCfg = false
			if m.networkConfig.SetRoles != "" && !aliveClient.PeeringOnly {

				if !m.ElectionRunOnce {
					m.SelectLoxiLBRoles(false, nil)
				}

				for instName, vi := range aliveClient.InstRoles {
					firstZoneName := api.GenZoneInstName(m.networkConfig.Zone, 0)
					if instName == firstZoneName && m.networkConfig.Zone != api.CIDefaultZone {
						defaultZoneName := api.GenZoneInstName(api.CIDefaultZone, 0)
						cisModel, err := m.makeLoxiLBCIStatusModel(instName, m.networkConfig.SetRoles, aliveClient)
						cisModel.Instance = defaultZoneName
						if err == nil {
							for retry := 0; retry < 5; retry++ {
								err = func(cisModel *api.CIStatusModel) error {
									ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
									defer cancel()
									return aliveClient.CIStatus().Create(ctx, cisModel)
								}(&cisModel)
								if err == nil {
									klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - OK", aliveClient.Host, defaultZoneName, vi.MasterLB)
									break
								} else {
									klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - failed(%d)", aliveClient.Host, defaultZoneName, vi.MasterLB, retry)
									time.Sleep(1 * time.Second)
								}
							}
						}
					}
					cisModel, err := m.makeLoxiLBCIStatusModel(instName, m.networkConfig.SetRoles, aliveClient)
					if err == nil {
						for retry := 0; retry < 5; retry++ {
							err = func(cisModel *api.CIStatusModel) error {
								ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
								defer cancel()
								return aliveClient.CIStatus().Create(ctx, cisModel)
							}(&cisModel)
							if err == nil {
								klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - OK", aliveClient.Host, instName, vi.MasterLB)
								break
							} else {
								klog.Infof("loxilb-lb(%s): set-role-master(%s:%v) - failed(%d)", aliveClient.Host, instName, vi.MasterLB, retry)
								time.Sleep(1 * time.Second)
							}
						}
					}
				}
			}

			if m.networkConfig.SetBGP != 0 {
				var bgpPeers []*api.LoxiClient

				if aliveClient.PeeringOnly {
					for _, lc := range m.LoxiClients.Clients {
						if aliveClient.Host != lc.Host {
							bgpPeers = append(bgpPeers, lc)
						}
					}
				} else {
					for _, lpc := range m.LoxiPeerClients.Clients {
						if aliveClient.Host != lpc.Host {
							bgpPeers = append(bgpPeers, lpc)
						}
					}
					if len(m.networkConfig.LoxilbURLs) <= 0 {
						for _, lc := range m.LoxiClients.Clients {
							if aliveClient.Host != lc.Host {
								bgpPeers = append(bgpPeers, lc)
							}
						}
					}
				}

				var bgpGlobalCfg api.BGPGlobalConfig
				if aliveClient.PeeringOnly {
					bgpGlobalCfg, _ = m.makeLoxiLBBGPGlobalModel(int(m.networkConfig.SetBGP), aliveClient.Host, false, m.networkConfig.ListenBGPPort)
				} else {
					bgpGlobalCfg, _ = m.makeLoxiLBBGPGlobalModel(int(m.networkConfig.SetBGP), aliveClient.Host, true, m.networkConfig.ListenBGPPort)
				}
				err := func(bgpGlobalCfg *api.BGPGlobalConfig) error {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
					defer cancel()
					return aliveClient.BGP().CreateGlobalConfig(ctx, bgpGlobalCfg)
				}(&bgpGlobalCfg)

				if err == nil {
					klog.Infof("loxilb(%s) set-bgp-global success", aliveClient.Host)
				} else {
					klog.Infof("loxilb(%s) set-bgp-global failed(%s)", aliveClient.Host, err)
					m.checkHandleBGPCfgErrors(m.ClientAliveCh, aliveClient, err)
				}

				for _, bgpPeer := range bgpPeers {
					bgpNeighCfg, _ := m.makeLoxiLBBGNeighModel(int(m.networkConfig.SetBGP), bgpPeer.Host, m.networkConfig.ListenBGPPort, false)
					err := func(bgpNeighCfg *api.BGPNeigh) error {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
						defer cancel()
						return aliveClient.BGP().CreateNeigh(ctx, bgpNeighCfg)
					}(&bgpNeighCfg)
					if err == nil {
						klog.Infof("set-bgp-neigh(%s->%s) success", aliveClient.Host, bgpPeer.Host)
					} else {
						klog.Infof("set-bgp-neigh(%s->%s) failed(%s)", aliveClient.Host, bgpPeer.Host, err)
						m.checkHandleBGPCfgErrors(m.ClientAliveCh, aliveClient, err)
					}

					bgpNeighCfg1, _ := m.makeLoxiLBBGNeighModel(int(m.networkConfig.SetBGP), aliveClient.Host, m.networkConfig.ListenBGPPort, false)
					err = func(bgpNeighCfg1 *api.BGPNeigh) error {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
						defer cancel()
						return bgpPeer.BGP().CreateNeigh(ctx, bgpNeighCfg1)
					}(&bgpNeighCfg1)
					if err == nil {
						klog.Infof("set-bgp-neigh(%s->%s) success", bgpPeer.Host, aliveClient.Host)
					} else {
						klog.Infof("set-bgp-neigh(%s->%s) failed(%s)", bgpPeer.Host, aliveClient.Host, err)
						m.checkHandleBGPCfgErrors(m.ClientAliveCh, bgpPeer, err)
					}
				}

				if !aliveClient.PeeringOnly {
					for _, bgpPeerURL := range m.networkConfig.ExtBGPPeers {
						bgpPeer := strings.Split(bgpPeerURL, ":")
						if len(bgpPeer) > 2 {
							continue
						}

						bgpRemoteIP := net.ParseIP(bgpPeer[0])
						if bgpRemoteIP == nil {
							continue
						}

						asid, err := strconv.ParseInt(bgpPeer[1], 10, 0)
						if err != nil || asid == 0 {
							continue
						}

						bgpNeighCfg, _ := m.makeLoxiLBBGNeighModel(int(asid), bgpRemoteIP.String(), 0, m.networkConfig.EBGPMultiHop)
						err = func(bgpNeighCfg *api.BGPNeigh) error {
							ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
							defer cancel()
							return aliveClient.BGP().CreateNeigh(ctx, bgpNeighCfg)
						}(&bgpNeighCfg)
						if err == nil {
							klog.Infof("set-ebgp-neigh(%s:%v) cfg success", bgpRemoteIP.String(), asid)
						} else {
							klog.Infof("set-ebgp-neigh(%s:%v) cfg - failed (%s)", bgpRemoteIP.String(), asid, err)
							m.checkHandleBGPCfgErrors(m.ClientAliveCh, aliveClient, err)
						}
					}
				}
			}

			if !aliveClient.PeeringOnly {
				isSuccess := false
				for _, value := range m.lbCache {
					for _, sp := range value.LbServicePairs {
						for _, lb := range sp.LbModelList {
							for retry := 0; retry < 5; retry++ {
								err := m.installLB(aliveClient, lb, value.PrefLocal)
								if err == nil {
									klog.Infof("reinstallLoxiLbRules: lbModel: %v success", lb)
									isSuccess = true
									break
								} else {
									if !strings.Contains(err.Error(), "exist") {
										klog.Infof("reinstallLoxiLbRules: lbModel: %v retry(%d)", lb, retry)
										time.Sleep(1 * time.Second)
									} else {
										isSuccess = true
										break
									}
								}
							}
							if !isSuccess && aliveClient.IsAlive {
								klog.Exit("restart kube-loxilb")
							}
						}
					}
				}
			}
		}
	}
}

func (m *Manager) AddLoxiInstAddr(name string, IP net.IP) error {

	m.mtx.Lock()
	defer m.mtx.Unlock()

	if ip, exists := m.loxiInstAddrMap[name]; exists {
		if ip.Equal(IP) {
			return nil
		}
		m.loxiInstAddrMap[name] = IP
		klog.Infof("updated cidr host name: %s:%s", name, IP.String())
		return nil
	}

	m.loxiInstAddrMap[name] = IP
	m.instAddrApplyCh <- struct{}{}
	klog.Infof("added cidr host name: %s:%s", name, IP.String())
	return nil
}

func (m *Manager) DeleteLoxiInstAddr(name string) error {

	m.mtx.Lock()
	defer m.mtx.Unlock()

	if _, exists := m.loxiInstAddrMap[name]; !exists {
		return nil
	}

	delete(m.loxiInstAddrMap, name)
	m.instAddrApplyCh <- struct{}{}
	klog.Infof("removed cidr host name: %s", name)
	return nil
}

func (m *Manager) AddLoxiCIDRPool(poolName string, cidr string) error {

	addr, _, err := net.ParseCIDR(cidr)
	if err != nil {
		if strings.Contains(cidr, "-") {
			ipBlock := strings.Split(cidr, "-")
			if len(ipBlock) != 2 {
				return fmt.Errorf("invalid ip-range")
			}

			startIP := net.ParseIP(ipBlock[0])
			lastIP := net.ParseIP(ipBlock[1])
			if startIP == nil || lastIP == nil {
				return fmt.Errorf("invalid ip-range ips")
			}
			if tk.IsNetIPv4(startIP.String()) && tk.IsNetIPv6(lastIP.String()) ||
				tk.IsNetIPv6(startIP.String()) && tk.IsNetIPv4(lastIP.String()) {
				return fmt.Errorf("invalid ip-types ips")
			}
		} else {
			klog.Errorf("failed to parse (CIDR: %s)", cidr)
			return err
		}
	}

	newIPPoolTbl := make(map[string]*ippool.IPPool)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	currIPPoolTbl := m.ipPoolTbl

	if tk.IsNetIPv6(addr.String()) {
		currIPPoolTbl = m.ip6PoolTbl
	}

	for key, value := range currIPPoolTbl {
		newIPPoolTbl[key] = value
	}

	if _, ok := newIPPoolTbl[poolName]; ok {
		if newIPPoolTbl[poolName].CIDR != cidr {
			ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), cidr, !m.networkConfig.ExclIPAM)
			if err != nil {
				klog.Errorf("failed to create updated external IP Pool (CIDR: %s)", cidr)
				return err
			}
			newIPPoolTbl[poolName] = ipPool
			klog.Infof("cidr pool: %s:%s updated", poolName, cidr)
			return nil
		}
		//klog.Infof("cidr pool: %s:%s exists", poolName, cidr)
		return nil
	}

	ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), cidr, !m.networkConfig.ExclIPAM)
	if err != nil {
		klog.Errorf("failed to create external IP Pool (CIDR: %s)", cidr)
		return err
	}

	newIPPoolTbl[poolName] = ipPool

	if tk.IsNetIPv6(addr.String()) {
		m.ip6PoolTbl = newIPPoolTbl
	} else {
		m.ipPoolTbl = newIPPoolTbl
	}

	klog.Infof("added new cidr pool %s:%s", poolName, cidr)
	return nil
}

func (m *Manager) DeleteLoxiCIDRPool(poolName string, cidr string) error {

	addr, _, err := net.ParseCIDR(cidr)
	if err != nil {
		if strings.Contains(cidr, "-") {
			ipBlock := strings.Split(cidr, "-")
			if len(ipBlock) != 2 {
				return fmt.Errorf("invalid ip-range")
			}

			startIP := net.ParseIP(ipBlock[0])
			lastIP := net.ParseIP(ipBlock[1])
			if startIP == nil || lastIP == nil {
				return fmt.Errorf("invalid ip-range ips")
			}
			if tk.IsNetIPv4(startIP.String()) && tk.IsNetIPv6(lastIP.String()) ||
				tk.IsNetIPv6(startIP.String()) && tk.IsNetIPv4(lastIP.String()) {
				return fmt.Errorf("invalid ip-types ips")
			}
		} else {
			klog.Errorf("failed to parse (CIDR: %s)", cidr)
			return err
		}
	}

	newIPPoolTbl := make(map[string]*ippool.IPPool)

	m.mtx.Lock()
	defer m.mtx.Unlock()

	currIPPoolTbl := m.ipPoolTbl

	if tk.IsNetIPv6(addr.String()) {
		currIPPoolTbl = m.ip6PoolTbl
	}

	for key, value := range currIPPoolTbl {
		newIPPoolTbl[key] = value
	}

	if _, ok := newIPPoolTbl[poolName]; !ok {
		klog.Infof("cidr pool: %s:%s does not exists", poolName, cidr)
		return nil
	}

	delete(newIPPoolTbl, poolName)

	if tk.IsNetIPv6(addr.String()) {
		m.ip6PoolTbl = newIPPoolTbl
	} else {
		m.ipPoolTbl = newIPPoolTbl
	}

	klog.Infof("deleted cidr pool %s:%s", poolName, cidr)
	return nil
}

func (m *Manager) compareLoxiLBToServiceList(ctx context.Context) error {
	for _, c := range m.LoxiClients.Clients {
		lbList, err := c.LoadBalancer().List(ctx)
		if err != nil {
			return err
		}

		for _, l := range lbList.Item {
			var ns, name, inst string
			fmt.Sscanf(l.Service.Name, "%s_%s:%s", &ns, &name, &inst)
			if len(ns) == 0 || len(name) == 0 {
				continue
			}

			_, err := m.serviceLister.Services(ns).Get(name)
			if err != nil {
				klog.Infof("LoxiLB has the %s/%s rule, but Kubernetes does not. The corresponding LoxiLB rule will be deleted.", ns, name)
				if err := c.LoadBalancer().Delete(ctx, &l); err != nil {
					klog.Errorf("Failed to delete LoxiLB rule %s: %v", l.Service.Name, err)
					return err
				}
			}
		}
	}

	return nil
}
