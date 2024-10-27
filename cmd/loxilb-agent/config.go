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
	componentbaseconfig "k8s.io/component-base/config"
)

type AgentConfig struct {
	// clientConnection specifies the kubeconfig file and client connection settings for the agent
	// to communicate with the apiserver.
	ClientConnection componentbaseconfig.ClientConnectionConfiguration `yaml:"clientConnection"`
	// Mount location of the /proc directory. The default is "/host", which is appropriate when
	// loxilb-agent is run as part of the LoxiAgent DaemonSet (and the host's /proc directory is mounted
	// as /host/proc in the loxilb-agent container). When running loxilb-agent as a process,
	// hostProcPathPrefix should be set to "/" in the YAML config.
	HostProcPathPrefix string `yaml:"hostProcPathPrefix,omitempty"`
	// withNamespace specifies loxilb run in namespace.
	WithNamespace bool `yaml:"withNamespace,omitempty"`
	// loxiURL specific URL to access loxilb API server
	// TODO: support HTTPS
	LoxiURLs []string `yaml:"loxiURL,omitempty"`
	// nodePortServiceVirtIP is
	NodePortServiceVirtIP string `yaml:"nodePortServiceVirtIP,omitempty"`
	// support LoadBalancerClass
	LoxilbLoadBalancerClass string `yaml:"loxilbLoadBalancerClass,omitempty"`
	// enable Gateway API
	EnableGatewayAPI bool `yaml:"gatewayAPI"`
	// support GatewayClass manager name
	LoxilbGatewayClass string `yaml:"loxilbGatewayClass,omitempty"`
	// support LoadBalancer external IP
	ExternalCIDR string `yaml:"externalCIDR,omitempty"`
	// support LoadBalancer external IP Pool Definitions
	ExternalCIDRPoolDefs []string `yaml:"cidrPools,omitempty"`
	// support LoadBalancer external IP6 Pool Definitions
	ExternalCIDR6PoolDefs []string `yaml:"cidr6Pools,omitempty"`
	// external BGP Peers. This is a comma separated list e.g. IP1:ASID1,IP2:ASID2
	ExtBGPPeers []string `yaml:"extBGPPeers,omitempty"`
	// support BGP protocol
	SetBGP uint16 `yaml:"setBGP,omitempty"`
	// Custom BGP Port
	ListenBGPPort uint16 `yaml:"listenBGPPort,omitempty"`
	// Set eBGP multi-hop
	EBGPMultiHop bool `yaml:"eBGPMultiHop"`
	// loxilb loadbalancer mode
	SetLBMode uint16 `yaml:"setLBMode,omitempty"`
	// Shared or exclusive IPAM
	ExclIPAM bool `yaml:"setExclIPAM"`
	// Enable monitoring end-points of LB rule
	Monitor bool `yaml:"monitor"`
	// Enable appending end-points of LB rule
	AppendEPs bool `yaml:"appendEPs"`
	// Set loxilb node roles
	SetRoles string `yaml:"setRoles,omitempty"`
	// Set loxilb zone
	Zone string `yaml:"zone,omitempty"`
	// NodeIPs to exclude from role-selection. This is a comma separated list
	ExcludeRoleList []string `yaml:"excludeRoleList,omitempty"`
	// Specify aws secondary IP. Used when configuring HA in AWS.
	// The specified private IP is assigned to the loxilb instance and is associated with EIP.
	PrivateCIDR string `yaml:"privateCIDR,omitempty"`
	// enable Gateway API
	EnableBGPCRDs bool `yaml:"enableBGPCRDs,omitempty"`
	// Number of zone instances for HA
	NumZoneInst int `yaml:"numZoneInstances,omitempty"`
}
