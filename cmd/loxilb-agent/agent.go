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
	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/agent/manager/loadbalancer"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
	"github.com/loxilb-io/kube-loxilb/pkg/k8s"
	"github.com/loxilb-io/kube-loxilb/pkg/log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"

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
	k8sClient, _, _, err := k8s.CreateClients(o.config.ClientConnection, "")
	if err != nil {
		return fmt.Errorf("error creating k8s clients: %v", err)
	}

	informerFactory := informers.NewSharedInformerFactory(k8sClient, informerDefaultResync)

	// networkReadyCh is used to notify that the Node's network is ready.
	// Functions that rely on the Node's network should wait for the channel to close.
	// networkReadyCh := make(chan struct{})
	// set up signal capture: the first SIGTERM / SIGINT signal is handled gracefully and will
	// cause the stopCh channel to be closed; if another signal is received before the program
	// exits, we will force exit.
	stopCh := RegisterSignalHandlers()

	klog.Infof("URLs: %v", o.config.LoxiURLs)
	klog.Infof("LB Class: %s", o.config.LoxilbLoadBalancerClass)
	klog.Infof("CIDR: %s", o.config.ExternalCIDR)
	klog.Infof("SetBGP: %v", o.config.SetBGP)
	klog.Infof("SetLBMode: %v", o.config.SetLBMode)
	klog.Infof("ExclIPAM: %v", o.config.ExclIPAM)
	klog.Infof("Monitor: %v", o.config.Monitor)
	klog.Infof("SecondaryCIDRs: %v", o.config.ExternalSecondaryCIDRs)
	klog.Infof("ExtBGPPeers: %v", o.config.ExtBGPPeers)
	klog.Infof("SetRoles: %v", o.config.SetRoles)

	networkConfig := &config.NetworkConfig{
		LoxilbURLs:              o.config.LoxiURLs,
		LoxilbLoadBalancerClass: o.config.LoxilbLoadBalancerClass,
		ExternalCIDR:            o.config.ExternalCIDR,
		ExternalCIDR6:           o.config.ExternalCIDR6,
		SetBGP:                  o.config.SetBGP,
		SetRoles:                o.config.SetRoles,
		ExtBGPPeers:             o.config.ExtBGPPeers,
		SetLBMode:               o.config.SetLBMode,
		Monitor:                 o.config.Monitor,
		ExternalSecondaryCIDRs:  o.config.ExternalSecondaryCIDRs,
		ExternalSecondaryCIDRs6: o.config.ExternalSecondaryCIDRs6,
	}

	ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), networkConfig.ExternalCIDR, !o.config.ExclIPAM)
	if err != nil {
		klog.Errorf("failed to create external IP Pool (CIDR: %s)", networkConfig.ExternalCIDR)
		return err
	}

	var sipPools []*ippool.IPPool
	if len(o.config.ExternalSecondaryCIDRs) != 0 {

		if len(o.config.ExternalSecondaryCIDRs) > 4 {
			return fmt.Errorf("externalSecondaryCIDR %s config is invalid", o.config.ExternalSecondaryCIDRs)
		}

		for _, CIDR := range o.config.ExternalSecondaryCIDRs {
			ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), CIDR, !o.config.ExclIPAM)
			if err != nil {
				klog.Errorf("failed to create external secondary IP Pool (CIDR: %s)", CIDR)
				return err
			}

			networkConfig.ExternalSecondaryCIDRs = append(networkConfig.ExternalSecondaryCIDRs, CIDR)
			sipPools = append(sipPools, ipPool)
			klog.Infof("create external secondary IP Pool (CIDR: %s) %v", CIDR, len(sipPools))
		}
	}

	ipPool6, err := ippool.NewIPPool(tk.IpAllocatorNew(), networkConfig.ExternalCIDR6, !o.config.ExclIPAM)
	if err != nil {
		klog.Errorf("failed to create external IP Pool (CIDR: %s)", networkConfig.ExternalCIDR6)
		return err
	}

	var sipPools6 []*ippool.IPPool
	if len(o.config.ExternalSecondaryCIDRs6) != 0 {

		if len(o.config.ExternalSecondaryCIDRs6) > 4 {
			return fmt.Errorf("externalSecondaryCIDR %s config is invalid", o.config.ExternalSecondaryCIDRs6)
		}

		for _, CIDR := range o.config.ExternalSecondaryCIDRs6 {
			ipPool, err := ippool.NewIPPool(tk.IpAllocatorNew(), CIDR, !o.config.ExclIPAM)
			if err != nil {
				klog.Errorf("failed to create external secondary IP Pool (CIDR: %s)", CIDR)
				return err
			}

			networkConfig.ExternalSecondaryCIDRs = append(networkConfig.ExternalSecondaryCIDRs6, CIDR)
			sipPools6 = append(sipPools6, ipPool)
			klog.Infof("create external secondary IP Pool (CIDR: %s) %v", CIDR, len(sipPools6))
		}
	}

	loxilbClients := make([]*api.LoxiClient, 0)
	loxilbPeerClients := make([]*api.LoxiClient, 0)
	loxiLBLiveCh := make(chan *api.LoxiClient, 2)
	loxiLBSelMasterEvent := make(chan bool)

	if len(networkConfig.LoxilbURLs) > 0 {
		for _, lbURL := range networkConfig.LoxilbURLs {
			loxilbClient, err := api.NewLoxiClient(lbURL, loxiLBLiveCh, false)
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
		ipPool,
		sipPools,
		ipPool6,
		sipPools6,
		networkConfig,
		informerFactory,
	)

	go wait.Until(func() {
		if len(networkConfig.LoxilbURLs) <= 0 {
			var tmploxilbClients []*api.LoxiClient
			// DNS lookup (not used now)
			// ips, err := net.LookupIP("loxilb-lb-service")
			ips, err := k8s.GetServiceEndPoints(k8sClient, "loxilb-lb-service", "kube-system")
			klog.Infof("loxilb-service end-points:  %v", ips)
			if err == nil {
				for _, v := range lbManager.LoxiClients {
					v.Purge = true
					for _, ip := range ips {
						if v.Host == ip.String() {
							v.Purge = false
						}
					}
				}

				for _, ip := range ips {
					found := false
					for _, v := range lbManager.LoxiClients {
						if v.Host == ip.String() {
							found = true
						}
					}
					if !found {
						client, err2 := api.NewLoxiClient("http://"+ip.String()+":11111", loxiLBLiveCh, false)
						if err2 != nil {
							continue
						}
						tmploxilbClients = append(tmploxilbClients, client)
					}
				}
				if len(tmploxilbClients) > 0 {
					for _, v := range tmploxilbClients {
						lbManager.LoxiClients = append(lbManager.LoxiClients, v)
					}
				}
				tmp := lbManager.LoxiClients[:0]
				for _, v := range lbManager.LoxiClients {
					if !v.Purge {
						tmp = append(tmp, v)
					} else {
						v.StopLoxiHealthCheckChan()
					}
				}
				lbManager.LoxiClients = tmp
			}

			var tmploxilbPeerClients []*api.LoxiClient
			ips, err = k8s.GetServiceEndPoints(k8sClient, "loxilb-peer-service", "kube-system")
			klog.Infof("loxilb-peer-service end-points:  %v", ips)
			if err == nil {
				for _, v := range lbManager.LoxiPeerClients {
					v.Purge = true
					for _, ip := range ips {
						if v.Host == ip.String() {
							v.Purge = false
						}
					}
				}

				for _, ip := range ips {
					found := false
					for _, v := range lbManager.LoxiPeerClients {
						if v.Host == ip.String() {
							found = true
						}
					}
					if !found {
						client, err2 := api.NewLoxiClient("http://"+ip.String()+":11111", loxiLBLiveCh, true)
						if err2 != nil {
							continue
						}
						tmploxilbPeerClients = append(tmploxilbPeerClients, client)
					}
				}
				if len(tmploxilbPeerClients) > 0 {
					for _, v := range tmploxilbPeerClients {
						lbManager.LoxiPeerClients = append(lbManager.LoxiPeerClients, v)
					}
				}
				tmp := lbManager.LoxiPeerClients[:0]
				for _, v := range lbManager.LoxiPeerClients {
					if !v.Purge {
						tmp = append(tmp, v)
					} else {
						v.StopLoxiHealthCheckChan()
					}
				}
				lbManager.LoxiPeerClients = tmp
			}
		}

		if networkConfig.SetRoles {
			reElect := false
			hasMaster := false
			for i := range lbManager.LoxiClients {
				v := lbManager.LoxiClients[i]
				if v.MasterLB && !v.IsAlive {
					v.MasterLB = false
					reElect = true
				} else if v.MasterLB {
					hasMaster = true
				}
			}
			if reElect || !hasMaster {
				selMaster := false
				for i := range lbManager.LoxiClients {
					v := lbManager.LoxiClients[i]
					if selMaster {
						v.MasterLB = false
						continue
					}
					if v.IsAlive {
						v.MasterLB = true
						selMaster = true
						klog.Infof("loxilb-peer(%v) set-role master", v.Url)
					}
				}
				if selMaster {
					lbManager.ElectionRunOnce = true
					loxiLBSelMasterEvent <- true
				}
			}
		}
	}, time.Second*20, stopCh)

	log.StartLogFileNumberMonitor(stopCh)
	informerFactory.Start(stopCh)

	go lbManager.Run(stopCh, loxiLBLiveCh, loxiLBSelMasterEvent)

	<-stopCh

	lbManager.DeleteAllLoadBalancer()

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
