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

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/bgppeer"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/bgppolicyapply"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/bgppolicydefinedsets"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/bgppolicydefinition"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/gatewayapi"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/loadbalancer"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	crdinformers "github.com/loxilb-io/kube-loxilb/pkg/client/informers/externalversions"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
	"github.com/loxilb-io/kube-loxilb/pkg/k8s"
	"github.com/loxilb-io/kube-loxilb/pkg/log"

	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"

	sigsInformer "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	tk "github.com/loxilb-io/loxilib"
)

// informerDefaultResync is the default resync period if a handler doesn't specify one.
// Use the same default value as kube-controller-manager:
// https://github.com/kubernetes/kubernetes/blob/release-1.17/pkg/controller/apis/config/v1alpha1/defaults.go#L120
const informerDefaultResync = 12 * time.Hour

var (
	capturedSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT}
	notifyCh        = make(chan os.Signal, 2)
)

// run starts kube-loxilb with the given options and waits for termination signal.
func run(o *Options) error {
	klog.Info("Starting kube-loxilb:")
	klog.Infof("  Version: %s", Version)
	klog.Infof("  Build: %s", BuildInfo)

	// create k8s Clientset, CRD Clientset and SharedInformerFactory for the given config.
	k8sClient, _, crdClient, _, sigsClient, err := k8s.CreateClients(o.config.ClientConnection, "")
	if err != nil {
		return fmt.Errorf("error creating k8s clients: %v", err)
	}

	informerFactory := informers.NewSharedInformerFactory(k8sClient, informerDefaultResync)
	crdInformerFactory := crdinformers.NewSharedInformerFactory(crdClient, informerDefaultResync)
	BGPPeerInformer := crdInformerFactory.Bgppeer().V1().BGPPeerServices()
	BGPPolicyDefinedSetInformer := crdInformerFactory.Bgppolicydefinedsets().V1().BGPPolicyDefinedSetsServices()
	BGPPolicyDefinitionInformer := crdInformerFactory.Bgppolicydefinition().V1().BGPPolicyDefinitionServices()
	BGPPolicyApplyInformer := crdInformerFactory.Bgppolicyapply().V1().BGPPolicyApplyServices()
	sigsInformerFactory := sigsInformer.NewSharedInformerFactory(sigsClient, informerDefaultResync)

	// networkReadyCh is used to notify that the Node's network is ready.
	// Functions that rely on the Node's network should wait for the channel to close.
	// networkReadyCh := make(chan struct{})
	// set up signal capture: the first SIGTERM / SIGINT signal is handled gracefully and will
	// cause the stopCh channel to be closed; if another signal is received before the program
	// exits, we will force exit.
	stopCh := RegisterSignalHandlers()

	klog.Infof("URLs: %v", o.config.LoxiURLs)
	klog.Infof("LB Class: %s", o.config.LoxilbLoadBalancerClass)
	klog.Infof("CIDR Pools: %s", o.config.ExternalCIDRPoolDefs)
	klog.Infof("SetBGP: %v", o.config.SetBGP)
	klog.Infof("ListenBGPPort: %v", o.config.ListenBGPPort)
	klog.Infof("eBGPMultiHop: %v", o.config.EBGPMultiHop)
	klog.Infof("SetLBMode: %v", o.config.SetLBMode)
	klog.Infof("ExclIPAM: %v", o.config.ExclIPAM)
	klog.Infof("Monitor: %v", o.config.Monitor)
	klog.Infof("ExtBGPPeers: %v", o.config.ExtBGPPeers)
	klog.Infof("SetRoles: %s", o.config.SetRoles)
	klog.Infof("Zone: %s", o.config.Zone)
	klog.Infof("AppendEPs: %v", o.config.AppendEPs)

	networkConfig := &config.NetworkConfig{
		LoxilbURLs:              o.config.LoxiURLs,
		LoxilbLoadBalancerClass: o.config.LoxilbLoadBalancerClass,
		LoxilbGatewayClass:      o.config.LoxilbGatewayClass,
		ExternalCIDRPoolDefs:    o.config.ExternalCIDRPoolDefs,
		ExternalCIDR6PoolDefs:   o.config.ExternalCIDR6PoolDefs,
		SetBGP:                  o.config.SetBGP,
		ListenBGPPort:           o.config.ListenBGPPort,
		EBGPMultiHop:            o.config.EBGPMultiHop,
		SetRoles:                o.config.SetRoles,
		Zone:                    o.config.Zone,
		ExtBGPPeers:             o.config.ExtBGPPeers,
		SetLBMode:               o.config.SetLBMode,
		Monitor:                 o.config.Monitor,
		AppendEPs:               o.config.AppendEPs,
		PrivateCIDR:             o.config.PrivateCIDR,
	}

	ipPoolTbl := make(map[string]*ippool.IPPool)

	if len(o.config.ExternalCIDRPoolDefs) > 0 {
		for _, pool := range o.config.ExternalCIDRPoolDefs {
			poolStrSlice := strings.Split(pool, "-")
			// Format is pool1-123.123.123.1/32,pool2-124.124.124.124.1/32
			if len(poolStrSlice) <= 0 || len(poolStrSlice) > 2 {
				return fmt.Errorf("externalCIDR %s config is invalid", o.config.ExternalCIDRPoolDefs)
			}
			ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), poolStrSlice[1], !o.config.ExclIPAM)
			if err != nil {
				klog.Errorf("failed to create external IP Pool (CIDR: %s)", networkConfig.ExternalCIDRPoolDefs)
				return err
			}

			ipPoolTbl[poolStrSlice[0]] = ipPool
		}
	}

	ip6PoolTbl := make(map[string]*ippool.IPPool)

	if len(o.config.ExternalCIDRPoolDefs) > 0 {
		for _, pool := range o.config.ExternalCIDR6PoolDefs {
			poolStrSlice := strings.Split(pool, "-")
			// Format is pool1-3ffe::1/64,pool2-2001::1/64
			if len(poolStrSlice) <= 0 || len(poolStrSlice) > 2 {
				return fmt.Errorf("externalCIDR %s config is invalid", o.config.ExternalCIDR6PoolDefs)
			}
			ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), poolStrSlice[1], !o.config.ExclIPAM)
			if err != nil {
				klog.Errorf("failed to create external IP Pool (CIDR: %s)", networkConfig.ExternalCIDR6PoolDefs)
				return err
			}
			ip6PoolTbl[poolStrSlice[0]] = ipPool
		}
	}

	loxilbClients := make([]*api.LoxiClient, 0)
	loxilbPeerClients := make([]*api.LoxiClient, 0)
	loxiLBLiveCh := make(chan *api.LoxiClient, 50)
	loxiLBPurgeCh := make(chan *api.LoxiClient, 5)
	loxiLBSelMasterEvent := make(chan bool)
	loxiLBDeadCh := make(chan struct{}, 64)
	ticker := time.NewTicker(20 * time.Second)

	if len(networkConfig.LoxilbURLs) > 0 {
		for _, lbURL := range networkConfig.LoxilbURLs {
			loxilbClient, err := api.NewLoxiClient(lbURL, loxiLBLiveCh, loxiLBDeadCh, false, false)
			if err != nil {
				return err
			}
			loxilbClients = append(loxilbClients, loxilbClient)
		}
	}

	lbManager := loadbalancer.NewLoadBalancerManager(
		k8sClient,
		loxilbClients,
		loxilbPeerClients,
		ipPoolTbl,
		ip6PoolTbl,
		networkConfig,
		informerFactory,
	)

	BgpPeerManager := bgppeer.NewBGPPeerManager(
		k8sClient,
		crdClient,
		networkConfig,
		BGPPeerInformer,
		lbManager,
	)

	BGPPolicyDefinedSetsManager := bgppolicydefinedsets.NewBGPPolicyDefinedSetsManager(
		k8sClient,
		crdClient,
		networkConfig,
		BGPPolicyDefinedSetInformer,
		lbManager,
	)

	BGPPolicyDefinitionManager := bgppolicydefinition.NewBGPPolicyDefinitionManager(
		k8sClient,
		crdClient,
		networkConfig,
		BGPPolicyDefinitionInformer,
		lbManager,
	)
	BGPPolicyApplyManager := bgppolicyapply.NewBGPPolicyApplyManager(
		k8sClient,
		crdClient,
		networkConfig,
		BGPPolicyApplyInformer,
		lbManager,
	)

	go func() {
		for {
			select {
			case <-loxiLBDeadCh:
				if networkConfig.SetRoles != "" {
					klog.Infof("Running select-roles")
					lbManager.SelectLoxiLBRoles(true, loxiLBSelMasterEvent)
				}
			case <-ticker.C:
				if len(networkConfig.LoxilbURLs) <= 0 {
					lbManager.DiscoverLoxiLBServices(loxiLBLiveCh, loxiLBDeadCh, loxiLBPurgeCh, o.config.ExcludeRoleList)
				}
				lbManager.DiscoverLoxiLBPeerServices(loxiLBLiveCh, loxiLBDeadCh, loxiLBPurgeCh)

				if networkConfig.SetRoles != "" {
					lbManager.SelectLoxiLBRoles(true, loxiLBSelMasterEvent)
				}
			case <-stopCh:
				return
			}
		}
	}()
	log.StartLogFileNumberMonitor(stopCh)
	informerFactory.Start(stopCh)

	go lbManager.Run(stopCh, loxiLBLiveCh, loxiLBPurgeCh, loxiLBSelMasterEvent)
	if o.config.EnableBGPCRDs {
		crdInformerFactory.Start(stopCh)
		go BgpPeerManager.Run(stopCh, loxiLBLiveCh, loxiLBPurgeCh, loxiLBSelMasterEvent)
		go BGPPolicyDefinedSetsManager.Run(stopCh, loxiLBLiveCh, loxiLBPurgeCh, loxiLBSelMasterEvent)
		go BGPPolicyDefinitionManager.Run(stopCh, loxiLBLiveCh, loxiLBPurgeCh, loxiLBSelMasterEvent)
		go BGPPolicyApplyManager.Run(stopCh, loxiLBLiveCh, loxiLBPurgeCh, loxiLBSelMasterEvent)
	}

	// Run gateway API managers
	if o.config.EnableGatewayAPI {
		gatewayClassManager := gatewayapi.NewGatewayClassManager(
			k8sClient, sigsClient, networkConfig, sigsInformerFactory)

		gatewayManager := gatewayapi.NewGatewayManager(
			k8sClient, sigsClient, networkConfig, ipPoolTbl["defaultPool"], sigsInformerFactory)

		tcpRouteManager := gatewayapi.NewTCPRouteManager(
			k8sClient, sigsClient, networkConfig, sigsInformerFactory)

		udpRouteManager := gatewayapi.NewUDPRouteManager(
			k8sClient, sigsClient, networkConfig, sigsInformerFactory)

		httpRouteManager := gatewayapi.NewHTTPRouteManager(
			k8sClient, sigsClient, networkConfig, sigsInformerFactory)

		sigsInformerFactory.Start(stopCh)

		go gatewayClassManager.Run(stopCh)
		go gatewayManager.Run(stopCh)
		go tcpRouteManager.Run(stopCh)
		go udpRouteManager.Run(stopCh)
		go httpRouteManager.Run(stopCh)
	}

	<-stopCh

	klog.Info("Stopping loxilb Agent")
	return nil
}

func RegisterSignalHandlers() <-chan struct{} {
	stopCh := make(chan struct{})

	go func() {
		<-notifyCh
		close(stopCh)
		<-notifyCh
		klog.Warning("Received second signal, will force exit")
		klog.Flush()
		os.Exit(1)
	}()

	signal.Notify(notifyCh, capturedSignals...)

	return stopCh
}
