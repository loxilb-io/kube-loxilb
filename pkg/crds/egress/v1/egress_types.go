package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type EgressStatus struct {
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Egress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressSpec   `json:"spec,omitempty"`
	Status EgressStatus `json:"status,omitempty"`
}

type EgressSpec struct {
	Addresses []string `json:"addresses,omitempty"`
	Vip       string   `json:"vip,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type EgressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Egress `json:"items"`
}
