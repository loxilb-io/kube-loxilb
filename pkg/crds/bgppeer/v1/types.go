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

type BGPPeerModel BGPNeigh

// BGPPeerServiceStatus defines the observed state of BGPPeerService
type BGPPeerServiceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPeerService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPPeerModel         `json:"spec,omitempty"`
	Status BGPPeerServiceStatus `json:"status,omitempty"`
}

// BGPPeerServiceSpec defines the desired state of LBService
type BGPPeerServiceSpec struct {
	Model BGPPeerModel `json:"bgpPeer"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPeerServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []*BGPPeerService `json:"items"`
}

type BGPNeigh struct {
	// BGP Neighbor IP address
	IPAddress string `json:"ipAddress,omitempty"`
	// Remote AS number
	RemoteAs int64 `json:"remoteAs,omitempty"`
	// Remote Connect Port (default 179)
	RemotePort int64 `json:"remotePort,omitempty"`
}

func (bpgModel *BGPPeerModel) GetKeyStruct() api.LoxiModel {
	return bpgModel
}
