package gatewayapi

import (
	"testing"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

func groupPtr(s string) *v1.Group { g := v1.Group(s); return &g }
func kindPtr(s string) *v1.Kind   { k := v1.Kind(s); return &k }

func TestClassifyBackendRef(t *testing.T) {
	tests := []struct {
		name string
		ref  v1.BackendObjectReference
		want backendKind
	}{
		{
			// The API defaults group to "" and kind to "Service" without
			// materialising either, so an unset pair is the common case.
			name: "unset group and kind default to Service",
			ref:  v1.BackendObjectReference{Name: "nginx"},
			want: backendService,
		},
		{
			name: "explicit empty group with Service",
			ref:  v1.BackendObjectReference{Group: groupPtr(""), Kind: kindPtr("Service"), Name: "nginx"},
			want: backendService,
		},
		{
			name: "core spelled out",
			ref:  v1.BackendObjectReference{Group: groupPtr("core"), Kind: kindPtr("Service"), Name: "nginx"},
			want: backendService,
		},
		{
			name: "inference pool",
			ref: v1.BackendObjectReference{
				Group: groupPtr(inferencePoolGroup), Kind: kindPtr(inferencePoolKind), Name: "vllm-pool",
			},
			want: backendInferencePool,
		},
		{
			// The failure this whole helper exists to prevent: without the
			// group check this reads as Service "vllm-pool".
			name: "inference group with an unknown kind",
			ref: v1.BackendObjectReference{
				Group: groupPtr(inferencePoolGroup), Kind: kindPtr("InferenceObjective"), Name: "vllm-pool",
			},
			want: backendUnsupported,
		},
		{
			name: "Service kind in a foreign group",
			ref: v1.BackendObjectReference{
				Group: groupPtr("multicluster.x-k8s.io"), Kind: kindPtr("Service"), Name: "nginx",
			},
			want: backendUnsupported,
		},
		{
			name: "core group with a non-Service kind",
			ref:  v1.BackendObjectReference{Kind: kindPtr("ServiceImport"), Name: "nginx"},
			want: backendUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyBackendRef(tt.ref); got != tt.want {
				t.Errorf("classifyBackendRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBackendRefDescription(t *testing.T) {
	tests := []struct {
		name string
		ref  v1.BackendObjectReference
		want string
	}{
		{
			name: "defaults read back as core/Service",
			ref:  v1.BackendObjectReference{Name: "nginx"},
			want: "core/Service nginx",
		},
		{
			name: "empty group is core",
			ref:  v1.BackendObjectReference{Group: groupPtr(""), Name: "nginx"},
			want: "core/Service nginx",
		},
		{
			name: "inference pool",
			ref: v1.BackendObjectReference{
				Group: groupPtr(inferencePoolGroup), Kind: kindPtr(inferencePoolKind), Name: "vllm-pool",
			},
			want: "inference.networking.k8s.io/InferencePool vllm-pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backendRefDescription(tt.ref); got != tt.want {
				t.Errorf("backendRefDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}
