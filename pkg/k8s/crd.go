package k8s

import (
	"context"

	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func CheckCRD(k8sClient apiextensionclientset.Interface, name string) bool {
	_, err := k8sClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}

	return true
}
