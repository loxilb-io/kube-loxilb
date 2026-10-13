/*
* Copyright (c) 2026 NetLOX Inc
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

package gatewayapi

import (
	"context"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	// TCPRouteCRDName, UDPRouteCRDName - the route CRDs watched at
	// RouteWatchVersion.
	TCPRouteCRDName = "tcproutes.gateway.networking.k8s.io"
	UDPRouteCRDName = "udproutes.gateway.networking.k8s.io"

	// RouteWatchVersion - the only version the gateway-api module of this
	// build has TCPRoute and UDPRoute types for. Gateway API v1.6 CRDs serve
	// both at v1 only.
	RouteWatchVersion = "v1alpha2"
)

// crdPollInterval - how often a missing or unreadable CRD is asked again.
var crdPollInterval = 5 * time.Second

// WaitForServedVersion - wait for the CRD to exist, then report whether it
// serves version.
//
// Watching a version the CRD does not serve fails on every retry, forever,
// and achieves nothing; asking first lets the caller skip the watch with one
// log line.
//
// A CRD this process is not allowed to read reports served: the check cannot
// run, and the caller watches as it always did. Any other error - the API
// server not ready yet, a timeout - is asked again: falling back on it would
// bring back the endlessly failing watch on a cluster that only serves v1,
// because of a blip at startup. ok is false only when stopCh closes first.
func WaitForServedVersion(stopCh <-chan struct{}, extClient apiextensionclientset.Interface, crdName, version string) (served, ok bool) {
	waiting, failing := false, false

	err := wait.PollUntilContextCancel(wait.ContextForChannel(stopCh), crdPollInterval, true,
		func(ctx context.Context) (bool, error) {
			crd, err := extClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
			switch {
			case errors.IsNotFound(err):
				if !waiting {
					klog.Infof("waiting for the %s CRD", crdName)
					waiting = true
				}
				return false, nil
			case errors.IsForbidden(err) || errors.IsUnauthorized(err):
				klog.Infof("not allowed to read the %s CRD (%v); watching it at %s regardless", crdName, err, version)
				served = true
				return true, nil
			case err != nil:
				if !failing {
					klog.Infof("cannot read the %s CRD yet (%v); asking again", crdName, err)
					failing = true
				}
				return false, nil
			}

			served = crdServes(crd, version)
			return true, nil
		})

	return served, err == nil
}

// crdServes - whether the CRD serves version.
func crdServes(crd *apiextensionsv1.CustomResourceDefinition, version string) bool {
	for _, v := range crd.Spec.Versions {
		if v.Name == version && v.Served {
			return true
		}
	}
	return false
}
