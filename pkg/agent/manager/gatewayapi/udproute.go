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

package gatewayapi

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	sigsclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigs_v1alpha2_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1alpha2"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
)

const udpRouteMgrName = "UDPRouteManager"

// UDPRouteManager - serves UDPRoutes; see l4route.go.
type UDPRouteManager struct {
	*l4RouteManager
}

func udpRouteView(r *v1alpha2.UDPRoute) *l4Route {
	view := &l4Route{
		Namespace: r.Namespace, Name: r.Name, Labels: r.Labels, Annotations: r.Annotations,
		ParentRefs: r.Spec.ParentRefs,
	}
	for _, rule := range r.Spec.Rules {
		view.BackendRefs = append(view.BackendRefs, rule.BackendRefs...)
	}
	return view
}

func udpRouteKind(lister sigs_v1alpha2_lister.UDPRouteLister) l4RouteKind {
	return l4RouteKind{
		kind:             kindUDPRoute,
		protocol:         corev1.ProtocolUDP,
		parentAnnotation: "parent-udp-route",
		get: func(namespace, name string) (*l4Route, error) {
			r, err := lister.UDPRoutes(namespace).Get(name)
			if err != nil {
				return nil, err
			}
			return udpRouteView(r), nil
		},
		isRoute: func(obj interface{}) bool {
			_, ok := obj.(*v1alpha2.UDPRoute)
			return ok
		},
		list: func(namespace string) ([]*l4Route, error) {
			routes, err := lister.UDPRoutes(namespace).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			views := make([]*l4Route, 0, len(routes))
			for _, r := range routes {
				views = append(views, udpRouteView(r))
			}
			return views, nil
		},
	}
}

func NewUDPRouteManager(
	kubeClient clientset.Interface,
	sigsClient sigsclientset.Interface,
	networkConfig *config.NetworkConfig,
	kubeInformerFactory informers.SharedInformerFactory,
	sigsInformerFactory externalversions.SharedInformerFactory,
	referenceGrants *ReferenceGrants) *UDPRouteManager {

	informer := sigsInformerFactory.Gateway().V1alpha2().UDPRoutes()
	return &UDPRouteManager{newL4RouteManager(udpRouteMgrName, "udpRoute", udpRouteKind(informer.Lister()), informer.Informer(),
		kubeClient, sigsClient, networkConfig, kubeInformerFactory, sigsInformerFactory, referenceGrants)}
}
