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

package v1

import (
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type EpSelect uint
type LbMode int32

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MultiClusterLBService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MultiClusterLBServiceSpec `json:"spec,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MultiClusterLBServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []*MultiClusterLBService `json:"items"`
}

type LoadBalancerService struct {
	ExternalIP string   `json:"externalIP" key:"externalipaddress"`
	Port       uint16   `json:"port" key:"port"`
	Protocol   string   `json:"protocol" key:"protocol"`
	Sel        EpSelect `json:"sel"`
	Mode       LbMode   `json:"mode"`
	BGP        bool     `json:"BGP,omitempty" options:"bgp"`
	Monitor    bool     `json:"Monitor,omitempty"`
	Timeout    uint32   `json:"inactiveTimeOut"`
	Block      uint16   `json:"block" options:"block"`
	Managed    bool     `json:"managed,omitempty"`
	ProbeType  string   `json:"probetype,omitempty"`
	ProbePort  uint16   `json:"probeport,omitempty"`
	ProbeReq   string   `json:"probereq,omitempty"`
	ProbeResp  string   `json:"proberesp,omitempty"`
}

type LoadBalancerEndpoint struct {
	EndpointIP string `json:"endpointIP"`
	TargetPort uint16 `json:"targetPort"`
	Weight     uint8  `json:"weight"`
	State      string `json:"state,omitempty"`
}

type LoadBalancerSecIp struct {
	SecondaryIP string `json:"secondaryIP"`
}

// MultiClusterLBServiceSpec defines the desired state of MultiClusterLBService
type MultiClusterLBServiceSpec struct {
	Model LoadBalancerModel `json:"lbModel"`
}

type LoadBalancerModel struct {
	Service      LoadBalancerService    `json:"serviceArguments"`
	SecondaryIPs []LoadBalancerSecIp    `json:"secondaryIPs,omitempty"`
	Endpoints    []LoadBalancerEndpoint `json:"endpoints"`
}

func (l *LoadBalancerModel) GetKeyStruct() api.LoxiModel {
	return &l.Service
}

func (lbService *LoadBalancerService) GetKeyStruct() api.LoxiModel {
	return lbService
}
