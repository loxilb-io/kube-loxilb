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
	// support LoadBalancer external IP
	ExternalCIDR string `yaml:"externalCIDR,omitempty"`
	// support LoadBalancer external secondary IP. This is a comma separated list
	ExternalSecondaryCIDRs []string `yaml:"externalSecondaryCIDRs,omitempty"`
	// support BGP protocol
	SetBGP bool `yaml:"setBGP,omitempty"`
	// loxilb loadbalancer mode
	SetLBMode uint16 `yaml:"setLBMode,omitempty"`
	// Shared or exclusive IPAM
	ExclIPAM bool `yaml:"setExclIPAM"`
	// Enable monitoring end-points of LB rule
	Monitor bool `yaml:"monitor"`
}
