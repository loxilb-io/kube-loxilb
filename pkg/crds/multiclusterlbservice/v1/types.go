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
type MultiClusterLBModel api.LoadBalancerModel

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MultiClusterLBService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MultiClusterLBServiceSpec `json:"spec,omitempty"`
}

// MultiClusterLBServiceSpec defines the desired state of MultiClusterLBService
type MultiClusterLBServiceSpec struct {
	Model MultiClusterLBModel `json:"lbModel"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MultiClusterLBServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []*MultiClusterLBService `json:"items"`
}

func (lbModel *MultiClusterLBModel) GetKeyStruct() api.LoxiModel {
	return &lbModel.Service
}
