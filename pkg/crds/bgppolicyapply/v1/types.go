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

package v1

import (
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type BGPPolicyApplyModel api.BGPPolicyApply

// BGPPolicyapplyServiceStatus defines the observed state of FireWallService
type BGPPolicyApplyServiceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPolicyApplyService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPPolicyApplyModel         `json:"spec,omitempty"`
	Status BGPPolicyApplyServiceStatus `json:"status,omitempty"`
}

// BGPPolicyapplySpec defines the desired state of LBService
type BGPPolicyApplySpec struct {
	Model BGPPolicyApplyModel `json:"bgppolicyapply"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPolicyApplyServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []*BGPPolicyApplyService `json:"items"`
}

func (bgpPolicyapplyModel *BGPPolicyApplyModel) GetKeyStruct() api.LoxiModel {
	return bgpPolicyapplyModel
}
