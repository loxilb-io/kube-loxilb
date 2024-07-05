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

// BGPPolicyDefinitionServiceStatus defines the observed state of FireWallService
type BGPPolicyDefinitionServiceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPolicyDefinitionService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPPolicyDefinition              `json:"spec,omitempty"`
	Status BGPPolicyDefinitionServiceStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type BGPPolicyDefinitionServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []*BGPPolicyDefinitionService `json:"items"`
}

func (bgpPolicyDefinitionModel *BGPPolicyDefinition) GetKeyStruct() api.LoxiModel {
	return bgpPolicyDefinitionModel
}

// BGPPolicyDefinition - Info related to a end-point config entry
type BGPPolicyDefinition struct {
	Name      string      `json:"name"`
	Statement []Statement `json:"statements"`
}

type Statement struct {
	Name       string     `json:"name,omitempty"`
	Conditions Conditions `json:"conditions,omitempty"`
	Actions    Actions    `json:"actions,omitempty"`
}

type Actions struct {
	RouteDisposition string     `json:"routeDisposition"`
	BGPActions       BGPActions `json:"bgpActions,omitempty"`
}

type BGPActions struct {
	SetMed            string           `json:"setMed,omitempty"`
	SetCommunity      SetCommunity     `json:"setCommunity,omitempty"`
	SetExtCommunity   SetCommunity     `json:"setExtCommunity,omitempty"`
	SetLargeCommunity SetCommunity     `json:"setLargeCommunity,omitempty"`
	SetNextHop        string           `json:"setNextHop,omitempty"`
	SetLocalPerf      int              `json:"setLocalPerf,omitempty"`
	SetAsPathPrepend  SetAsPathPrepend `json:"setAsPathPrepend,omitempty"`
}

type SetCommunity struct {
	Options            string   `json:"options,omitempty"`
	SetCommunityMethod []string `json:"setCommunityMethod,omitempty"`
}

type SetAsPathPrepend struct {
	ASN     string `json:"as,omitempty"`
	RepeatN int    `json:"repeatN,omitempty"`
}

type Conditions struct {
	PrefixSet     MatchPrefixSet   `json:"matchPrefixSet,omitempty"`
	NeighborSet   MatchNeighborSet `json:"matchNeighborSet,omitempty"`
	BGPConditions BGPConditions    `json:"bgpConditions"`
}

type MatchNeighborSet struct {
	MatchSetOption string `json:"matchSetOption,omitempty"`
	NeighborSet    string `json:"neighborSet,omitempty"`
}

type MatchPrefixSet struct {
	MatchSetOption string `json:"matchSetOption,omitempty"`
	PrefixSet      string `json:"prefixSet,omitempty"`
}

type BGPConditions struct {
	AfiSafiIn         []string        `json:"afiSafiIn,omitempty"`
	AsPathSet         BGPAsPathSet    `json:"matchAsPathSet,omitempty"`
	AsPathLength      BGPAsPathLength `json:"asPathLength,omitempty"`
	CommunitySet      BGPCommunitySet `json:"matchCommunitySet,omitempty"`
	ExtCommunitySet   BGPCommunitySet `json:"matchExtCommunitySet,omitempty"`
	LargeCommunitySet BGPCommunitySet `json:"largeCommunitySet,omitempty"`
	RouteType         string          `json:"routeType,omitempty"`
	NextHopInList     []string        `json:"nextHopInList,omitempty"`
	Rpki              string          `json:"rpki,omitempty"`
}

type BGPAsPathLength struct {
	Operator string `json:"operator,omitempty"`
	Value    int    `json:"value,omitempty"`
}
type BGPAsPathSet struct {
	AsPathSet       string `json:"asPathSet,omitempty"`
	MatchSetOptions string `json:"matchSetOptions,omitempty"`
}
type BGPCommunitySet struct {
	CommunitySet    string `json:"communitySet,omitempty"`
	MatchSetOptions string `json:"matchSetOptions,omitempty"`
}
