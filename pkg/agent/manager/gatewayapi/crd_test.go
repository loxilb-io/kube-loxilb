package gatewayapi

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func routeCRD(versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: TCPRouteCRDName},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: versions},
	}
}

func TestWaitForServedVersion(t *testing.T) {
	tests := []struct {
		name   string
		client *extfake.Clientset
		want   bool
	}{
		{
			name: "Gateway API before v1.6: v1alpha2 served",
			client: extfake.NewSimpleClientset(routeCRD(
				apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha2", Served: true, Storage: true})),
			want: true,
		},
		{
			// The cluster the warning loop came from.
			name: "Gateway API v1.6: only v1 served",
			client: extfake.NewSimpleClientset(routeCRD(
				apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha2", Served: false},
				apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true, Storage: true})),
			want: false,
		},
		{
			// No RBAC for CRDs: the check cannot run, so the old behaviour -
			// watch anyway - stays.
			name: "CRD unreadable",
			client: func() *extfake.Clientset {
				c := extfake.NewSimpleClientset()
				c.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, TCPRouteCRDName, nil)
				})
				return c
			}(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served, ok := WaitForServedVersion(make(chan struct{}), tt.client, TCPRouteCRDName, RouteWatchVersion)
			if !ok {
				t.Fatal("ok = false without the stop channel closing")
			}
			if served != tt.want {
				t.Errorf("served = %v, want %v", served, tt.want)
			}
		})
	}
}

func TestWaitForServedVersionStopsWhileWaiting(t *testing.T) {
	stop := make(chan struct{})
	close(stop)

	if _, ok := WaitForServedVersion(stop, extfake.NewSimpleClientset(), TCPRouteCRDName, RouteWatchVersion); ok {
		t.Error("ok = true for a CRD that never appeared before the stop channel closed")
	}
}
