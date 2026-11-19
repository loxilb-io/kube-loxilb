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
	"sync"
	"time"

	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

// ReferenceGrantCRDName - the CRD the grants are watched through.
const ReferenceGrantCRDName = "referencegrants.gateway.networking.k8s.io"

// referenceGrantSyncTimeout - how long a route manager waits for the grants
// to be in the cache before it starts without them.
var referenceGrantSyncTimeout = 30 * time.Second

// ReferenceGrants - the cluster's ReferenceGrants, which say what a route
// may refer to in another namespace. A cross-namespace reference no grant
// permits is not served, and one whose grant goes is withdrawn: the route
// managers queue the routes a grant names when it changes.
type ReferenceGrants struct {
	mu sync.RWMutex
	// specs - the grants in a namespace; nil until they are watched.
	specs    func(namespace string) ([]v1.ReferenceGrantSpec, error)
	handlers []func(v1.ReferenceGrantSpec)

	synced chan struct{}
}

func NewReferenceGrants() *ReferenceGrants {
	return &ReferenceGrants{synced: make(chan struct{})}
}

// AddHandler - call fn with a grant whenever one is added, changed or
// deleted; with both versions of a changed one. Handlers added after Run
// starts the watch are still called, without the grants that were there.
func (g *ReferenceGrants) AddHandler(fn func(v1.ReferenceGrantSpec)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.handlers = append(g.handlers, fn)
}

func (g *ReferenceGrants) notify(spec v1.ReferenceGrantSpec) {
	g.mu.RLock()
	handlers := g.handlers
	g.mu.RUnlock()
	for _, fn := range handlers {
		fn(spec)
	}
}

// Run - watch the grants at the version their CRD serves, once it exists.
//
// v1 is watched where the CRD serves it, and v1beta1 where it does not, as
// with every Gateway API release before v1.5. A CRD this process is not
// allowed to read is watched at v1beta1, which every release so far serves.
func (g *ReferenceGrants) Run(stopCh <-chan struct{}, extClient apiextensionclientset.Interface, factory externalversions.SharedInformerFactory) {
	crd, ok := waitForCRD(stopCh, extClient, ReferenceGrantCRDName)
	if !ok {
		return
	}

	var informer cache.SharedIndexInformer
	var specs func(namespace string) ([]v1.ReferenceGrantSpec, error)
	if crd != nil && crdServes(crd, "v1") {
		grants := factory.Gateway().V1().ReferenceGrants()
		informer = grants.Informer()
		specs = func(namespace string) ([]v1.ReferenceGrantSpec, error) {
			list, err := grants.Lister().ReferenceGrants(namespace).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			out := make([]v1.ReferenceGrantSpec, 0, len(list))
			for _, rg := range list {
				out = append(out, rg.Spec)
			}
			return out, nil
		}
	} else {
		grants := factory.Gateway().V1beta1().ReferenceGrants()
		informer = grants.Informer()
		specs = func(namespace string) ([]v1.ReferenceGrantSpec, error) {
			list, err := grants.Lister().ReferenceGrants(namespace).List(labels.Everything())
			if err != nil {
				return nil, err
			}
			out := make([]v1.ReferenceGrantSpec, 0, len(list))
			for _, rg := range list {
				out = append(out, rg.Spec)
			}
			return out, nil
		}
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(cur interface{}) { g.notifyObject(cur) },
		UpdateFunc: func(old, cur interface{}) {
			g.notifyObject(old)
			g.notifyObject(cur)
		},
		DeleteFunc: func(old interface{}) { g.notifyObject(old) },
	})

	// Read from before the cache is filled, so that a route queued by a
	// grant's arrival finds it there.
	g.mu.Lock()
	g.specs = specs
	g.mu.Unlock()

	factory.Start(stopCh)
	if cache.WaitForNamedCacheSync("ReferenceGrants", stopCh, informer.HasSynced) {
		close(g.synced)
	}
}

// notifyObject - hand the spec of a grant, at either version, to the handlers.
func (g *ReferenceGrants) notifyObject(obj interface{}) {
	if deletedState, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = deletedState.Obj
	}
	switch rg := obj.(type) {
	case *v1.ReferenceGrant:
		g.notify(rg.Spec)
	case *v1beta1.ReferenceGrant:
		g.notify(rg.Spec)
	default:
		klog.Errorf("Received unexpected object: %v", obj)
	}
}

// WaitForSync - wait for the grants to be in the cache, or for
// referenceGrantSyncTimeout. False when stopCh closes first.
//
// A route manager that waits no longer than that keeps serving where the
// grants cannot be read - no CRD, or no permission to list them - with every
// cross-namespace reference refused, as the spec asks of one no grant is
// seen for. A grant read later queues its routes.
func (g *ReferenceGrants) WaitForSync(stopCh <-chan struct{}) bool {
	if g == nil {
		return true
	}
	select {
	case <-g.synced:
		return true
	case <-stopCh:
		return false
	case <-time.After(referenceGrantSyncTimeout):
		klog.Warningf("ReferenceGrants are not readable after %v - is the %s CRD installed, and may kube-loxilb list "+
			"referencegrants? Until they are, no cross-namespace backendRef is served", referenceGrantSyncTimeout, ReferenceGrantCRDName)
		return true
	}
}

// grantReference - one side of a reference a grant may permit.
type grantReference struct {
	Group     v1.Group
	Kind      v1.Kind
	Namespace string
	// Name - the object referred to; only on the to side.
	Name string
}

// Permits - whether a reference from an object of from's kind in its
// namespace to the object to names is allowed: always within a namespace,
// and across one when a grant in to's namespace lists both.
//
// A grant lists where references may come from and what they may refer to,
// each entry on its own; one entry of each has to match. An entry of To
// without a name takes every object of its kind.
func (g *ReferenceGrants) Permits(from, to grantReference) bool {
	if from.Namespace == to.Namespace {
		return true
	}
	if g == nil {
		return false
	}

	g.mu.RLock()
	specs := g.specs
	g.mu.RUnlock()
	if specs == nil {
		return false
	}

	grants, err := specs(to.Namespace)
	if err != nil {
		klog.Errorf("unable to list the ReferenceGrants of namespace %s: %v", to.Namespace, err)
		return false
	}
	for _, spec := range grants {
		if grantListsFrom(spec, from) && grantListsTo(spec, to) {
			return true
		}
	}
	return false
}

func grantListsFrom(spec v1.ReferenceGrantSpec, from grantReference) bool {
	for _, f := range spec.From {
		if f.Group == from.Group && f.Kind == from.Kind && string(f.Namespace) == from.Namespace {
			return true
		}
	}
	return false
}

func grantListsTo(spec v1.ReferenceGrantSpec, to grantReference) bool {
	for _, t := range spec.To {
		if t.Group != to.Group || t.Kind != to.Kind {
			continue
		}
		if t.Name == nil || *t.Name == "" || string(*t.Name) == to.Name {
			return true
		}
	}
	return false
}
