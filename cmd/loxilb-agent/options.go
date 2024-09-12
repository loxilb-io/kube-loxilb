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
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	lib "github.com/loxilb-io/loxilib"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v2"
)

var (
	loxiURLFlag     = ""
	cidrPools       = ""
	cidr6Pools      = ""
	secondaryCIDRs  = ""
	secondaryCIDRs6 = ""
	extBGPPeers     = ""
	excludeRoleList = ""
	Version         = "latest"
	BuildInfo       = "master"
)

type Options struct {
	// The path of configuration file.
	configFile string
	// The configuration object
	config *AgentConfig
}

func newOptions() *Options {
	return &Options{
		config: &AgentConfig{},
	}
}

// addFlags adds flags to fs and binds them to options.
func (o *Options) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.configFile, "config", o.configFile, "The path to the configuration file")
	fs.StringVar(&loxiURLFlag, "loxiURL", loxiURLFlag, "loxilb API server URL(s)")
	fs.StringVar(&o.config.ExternalCIDR, "externalCIDR", o.config.ExternalCIDR, "External CIDR Range")
	fs.StringVar(&cidrPools, "cidrPools", cidrPools, "CIDR Pools")
	fs.StringVar(&cidr6Pools, "cidr6Pools", cidr6Pools, "CIDR6 Pools")
	fs.BoolVar(&o.config.EnableGatewayAPI, "gatewayAPI", false, "Enable gateway API managers")
	fs.BoolVar(&o.config.EnableBGPCRDs, "enableBGPCRDs", false, "Enable BGP CRDs")
	fs.StringVar(&o.config.LoxilbGatewayClass, "loxilbGatewayClass", o.config.LoxilbGatewayClass, "GatewayClass manager Name")
	fs.Uint16Var(&o.config.SetBGP, "setBGP", o.config.SetBGP, "Use BGP routing")
	fs.Uint16Var(&o.config.ListenBGPPort, "listenBGPPort", o.config.ListenBGPPort, "Custom BGP listen port")
	fs.BoolVar(&o.config.EBGPMultiHop, "eBGPMultiHop", o.config.EBGPMultiHop, "Enable multi-hop eBGP")
	fs.StringVar(&extBGPPeers, "extBGPPeers", extBGPPeers, "External BGP Peer(s)")
	fs.BoolVar(&o.config.ExclIPAM, "setUniqueIP", false, "Use unique IPAM per service")
	fs.Uint16Var(&o.config.SetLBMode, "setLBMode", o.config.SetLBMode, "LB mode to use")
	fs.BoolVar(&o.config.Monitor, "monitor", o.config.Monitor, "Enable monitoring end-points of LB rule")
	fs.StringVar(&o.config.SetRoles, "setRoles", o.config.SetRoles, "Set LoxiLB node roles")
	fs.StringVar(&o.config.Zone, "zone", o.config.Zone, "The kube-loxilb zone instance")
	fs.StringVar(&o.config.PrivateCIDR, "privateCIDR", o.config.PrivateCIDR, "Specify aws secondary IP. Used when configuring HA in AWS and associate with EIP.")
	fs.StringVar(&excludeRoleList, "excludeRoleList", excludeRoleList, "List of nodes to exclude in role-selection")
	fs.BoolVar(&o.config.AppendEPs, "appendEPs", o.config.AppendEPs, "Attach and detach end-points of LB rule")
}

// complete completes all the required optionst

func (o *Options) complete(args []string) error {
	if len(o.configFile) > 0 {
		if err := o.loadConfigFromFile(); err != nil {
			return err
		}
	}
	o.updateConfigFromCommandLine()
	o.setDefaults()
	return nil
}

// validate validates all the required options. It must be called after complete.
func (o *Options) validate(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("no positional arguments are supported")
	}

	if len(o.config.LoxiURLs) > 0 {
		for _, loxiURL := range o.config.LoxiURLs {
			if _, err := url.Parse(loxiURL); err != nil {
				return fmt.Errorf("loxiURL %s is invalid. err: %v", loxiURL, err)
			}
		}
	}

	fmt.Errorf("ExternalCIDRPoolDefs %v", o.config.ExternalCIDRPoolDefs)

	if len(o.config.ExternalCIDRPoolDefs) > 0 {
		for _, pool := range o.config.ExternalCIDRPoolDefs {
			fmt.Errorf("ExternalCIDRPoolDefs Pool %s", pool)
			poolStrSlice := strings.Split(pool, "=")
			// Format is pool1=123.123.123.1/32,pool2=124.124.124.124.1/32
			if len(poolStrSlice) != 2 {
				return fmt.Errorf("externalCIDR %s config is invalid", o.config.ExternalCIDRPoolDefs)
			}
			if _, _, err := net.ParseCIDR(poolStrSlice[1]); err != nil {
				return fmt.Errorf("externalCIDR %s config is invalid", poolStrSlice[1])
			}
			if !lib.IsNetIPv4(poolStrSlice[1]) {
				return fmt.Errorf("externalCIDR %s config is invalid", poolStrSlice[1])
			}
		}
	}

	if len(o.config.ExternalCIDR6PoolDefs) > 0 {
		for _, pool := range o.config.ExternalCIDR6PoolDefs {
			poolStrSlice := strings.Split(pool, "=")
			// Format is pool1=3ffe1::1/64,pool2=2001::1/64
			if len(poolStrSlice) != 2 {
				return fmt.Errorf("externalCIDR6 %s config is invalid", o.config.ExternalCIDR6PoolDefs)
			}
			if _, _, err := net.ParseCIDR(poolStrSlice[1]); err != nil {
				return fmt.Errorf("externalCIDR6 %s config is invalid", poolStrSlice[1])
			}
			if !lib.IsNetIPv6(poolStrSlice[1]) {
				return fmt.Errorf("externalCIDR6 %s config is invalid", poolStrSlice[1])
			}
		}
	}

	if len(o.config.ExtBGPPeers) > 0 {
		if o.config.SetBGP == 0 {
			return fmt.Errorf("extBGPPeers %v config is invalid", o.config.ExtBGPPeers)
		}

		for _, bgpURL := range o.config.ExtBGPPeers {
			bgpPeer := strings.Split(bgpURL, ":")
			if len(bgpPeer) > 2 {
				return fmt.Errorf("bgpURL %s is invalid. format err", bgpURL)
			}

			if net.ParseIP(bgpPeer[0]) == nil {
				return fmt.Errorf("bgpURL %s is invalid. address err", bgpPeer[0])
			}

			asid, err := strconv.ParseInt(bgpPeer[1], 10, 0)
			if err != nil || asid == 0 {
				return fmt.Errorf("bgpURL %s is invalid. asid err", bgpPeer[1])
			}
		}
	}

	if o.config.LoxilbLoadBalancerClass != "" {
		if ok := strings.Contains(o.config.LoxilbLoadBalancerClass, "/"); !ok {
			return fmt.Errorf("loxilbLoadBalancerClass must be a label-style identifier")
		}
	}

	if o.config.EnableGatewayAPI {
		if o.config.LoxilbGatewayClass != "" {
			if ok := strings.Contains(o.config.LoxilbGatewayClass, "/"); !ok {
				return fmt.Errorf("LoxilbGatewayClass must be a label-style identifier")
			}
		}
	}

	if o.config.SetRoles != "" {
		if net.ParseIP(o.config.SetRoles) == nil {
			return fmt.Errorf("SetRoles %s config is invalid", o.config.SetRoles)
		}
	}

	if o.config.PrivateCIDR != "" {
		if _, _, err := net.ParseCIDR(o.config.PrivateCIDR); err != nil {
			return fmt.Errorf("privateCIDR %s config is invalid", o.config.PrivateCIDR)
		}
		if !lib.IsNetIPv4(o.config.PrivateCIDR) {
			return fmt.Errorf("privateCIDR %s config is invalid", o.config.PrivateCIDR)
		}
	}

	if len(o.config.ExcludeRoleList) > 0 {
		if len(o.config.ExcludeRoleList) > 128 {
			return fmt.Errorf("excludeRoleList %v config is invalid", o.config.ExcludeRoleList)
		}

		for _, nodeIP := range o.config.ExcludeRoleList {
			if ip := net.ParseIP(nodeIP); ip == nil {
				return fmt.Errorf("excludeRoleList %s config is invalid", nodeIP)
			}
			if !lib.IsNetIPv4(nodeIP) {
				return fmt.Errorf("excludeRoleList %s config is invalid", nodeIP)
			}
		}
	}

	return nil
}

func (o *Options) loadConfigFromFile() error {
	data, err := os.ReadFile(o.configFile)
	if err != nil {
		return err
	}

	if err := yaml.UnmarshalStrict(data, &o.config); err != nil {
		return err
	}

	return nil
}

func (o *Options) updateConfigFromCommandLine() {
	if loxiURLFlag != "" {
		o.config.LoxiURLs = strings.Split(loxiURLFlag, ",")
	}
	if cidrPools != "" {
		o.config.ExternalCIDRPoolDefs = strings.Split(cidrPools, ",")
	}
	if cidr6Pools != "" {
		o.config.ExternalCIDR6PoolDefs = strings.Split(cidr6Pools, ",")
	}
	if extBGPPeers != "" {
		o.config.ExtBGPPeers = strings.Split(extBGPPeers, ",")
	}
	if excludeRoleList != "" {
		o.config.ExcludeRoleList = strings.Split(excludeRoleList, ",")
	}
}

const (
	defaultHostProcPathPrefix    = "/host"
	defaultLoxiURL               = "http://127.0.0.1:11111"
	defaultnodePortServiceVirtIP = "11.187.0.1/32"
)

func (o *Options) setDefaults() {
	o.config.WithNamespace = true

	if o.config.HostProcPathPrefix == "" {
		o.config.HostProcPathPrefix = defaultHostProcPathPrefix
	}
	//if o.config.LoxiURLs == nil {
	//	o.config.LoxiURLs = []string{defaultLoxiURL}
	//}
	if o.config.NodePortServiceVirtIP == "" {
		o.config.NodePortServiceVirtIP = defaultnodePortServiceVirtIP
	}
	if o.config.LoxilbLoadBalancerClass == "" {
		o.config.LoxilbLoadBalancerClass = "loxilb.io/loxilb"
	}
	if o.config.LoxilbGatewayClass == "" {
		o.config.LoxilbGatewayClass = "loxilb.io/loxilb"
	}

	if o.config.ExternalCIDR != "" {
		poolStr := fmt.Sprintf("defaultPool=%s", o.config.ExternalCIDR)
		o.config.ExternalCIDRPoolDefs = []string{poolStr}
	} else {
		if o.config.ExternalCIDRPoolDefs == nil {
			o.config.ExternalCIDRPoolDefs = []string{"defaultPool=123.123.123.1/24"}
		}
	}

	if o.config.ExternalCIDR6PoolDefs == nil {
		o.config.ExternalCIDR6PoolDefs = []string{"defaultPool=3ffe:cafe::1/96"}
	}

	if o.config.ExtBGPPeers == nil {
		o.config.ExtBGPPeers = []string{}
	}
	if o.config.ListenBGPPort == 0 {
		o.config.ListenBGPPort = 179
	}
	if o.config.Zone == "" {
		o.config.Zone = "llb"
	}
}
