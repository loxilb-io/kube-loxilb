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

package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// pdPodWatcher - pod caches backing prefill/decode role resolution.
//
// Roles come from pod labels, so kube-loxilb has to both read them and notice
// when they change. A read on every reconcile answers the first and can never
// answer the second: without a watch there is no event, and a pod relabelled in
// place keeps its address, so nothing else wakes the service up. Repeated
// listing is also the more expensive shape - a query and a full serialised
// response per reconcile, against one long-lived connection and a local cache.
//
// Two properties keep the cost off clusters that do not use disaggregation:
//
//   - Watches start lazily, on the first service that turns disaggregation on.
//     A cluster without one registers no watch and caches nothing.
//   - Each watch is scoped to a namespace, and further to a label selector when
//     the service's own selectors imply one (see podCacheScope), so the cache
//     holds candidate pods rather than every pod in the cluster.
type pdPodWatcher struct {
	kubeClient clientset.Interface
	resync     time.Duration
	// onPodUpdate - called when a cached pod's labels or address change, with
	// the pod as it was and as it now is.
	onPodUpdate func(oldPod, newPod *corev1.Pod)

	mu      sync.Mutex
	stopCh  <-chan struct{}
	watches map[string]*pdPodWatch
}

type pdPodWatch struct {
	lister corelisters.PodLister
	synced cache.InformerSynced
	// gaveUp - a wait for this cache has timed out. Later callers do not wait
	// again until it has synced: each wait holds the cache lock, and a cache
	// that missed one deadline is likely to miss the next.
	gaveUp atomic.Bool
}

// pdCacheSyncTimeout - how long a reconcile waits for a new pod cache to sync.
// The worker holds the cache lock meanwhile, so every other Service waits as
// well. A var so tests can shorten it.
var pdCacheSyncTimeout = 5 * time.Second

// errPodCacheNotSynced - the pod cache behind prefill/decode roles has not
// synced: the API server cannot be reached, or pods cannot be listed in the
// namespace. Not the Service's fault; the informer keeps trying and the sync
// is retried.
var errPodCacheNotSynced = errors.New("the pod cache has not synced")

func newPDPodWatcher(kubeClient clientset.Interface, resync time.Duration, onPodUpdate func(oldPod, newPod *corev1.Pod)) *pdPodWatcher {
	return &pdPodWatcher{
		kubeClient:  kubeClient,
		resync:      resync,
		onPodUpdate: onPodUpdate,
		watches:     make(map[string]*pdPodWatch),
	}
}

// SetStopCh - hand the watcher the manager's lifetime. Called from Run before
// any worker starts, so the informers outlive the reconcile that created them.
func (w *pdPodWatcher) SetStopCh(stopCh <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.stopCh = stopCh
}

// neverClosed - stand-in lifetime for a watcher used outside Run.
var neverClosed = make(chan struct{})

// Lister - the pod cache for this namespace and scope, started on first use.
//
// Waits for the cache to sync. That gate matters: a disaggregated reconcile
// served from an unsynced cache sees no pods, derives no prefill and no decode
// endpoints, and loxilb rejects the rule - which would happen on every
// controller restart.
//
// The wait is bounded by pdCacheSyncTimeout, and once one has timed out the
// next callers do not wait at all until the cache has synced: the caller holds
// the cache lock, and a cache that cannot sync - no pod list rights in the
// namespace, an API server that is down - stopped every Service sync and the
// peer re-sync behind it. Either way the answer is errPodCacheNotSynced.
func (w *pdPodWatcher) Lister(ctx context.Context, namespace, scope string) (corelisters.PodLister, error) {
	key := namespace + "|" + scope

	w.mu.Lock()
	watch, started := w.watches[key]
	if !started {
		watch = w.startWatch(namespace, scope)
		w.watches[key] = watch
	}
	w.mu.Unlock()

	if watch.gaveUp.Load() {
		if !watch.synced() {
			return nil, fmt.Errorf("namespace %s: %w", namespace, errPodCacheNotSynced)
		}
		watch.gaveUp.Store(false)
		return watch.lister, nil
	}

	ctx, cancel := context.WithTimeout(ctx, pdCacheSyncTimeout)
	defer cancel()

	if !cache.WaitForCacheSync(ctx.Done(), watch.synced) {
		watch.gaveUp.Store(true)
		return nil, fmt.Errorf("namespace %s, waited %v: %w", namespace, pdCacheSyncTimeout, errPodCacheNotSynced)
	}

	return watch.lister, nil
}

// startWatch - build and start one namespace-scoped pod informer. Caller holds
// w.mu.
func (w *pdPodWatcher) startWatch(namespace, scope string) *pdPodWatch {
	stopCh := w.stopCh
	if stopCh == nil {
		stopCh = neverClosed
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		w.kubeClient,
		w.resync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = scope
		}),
	)

	podInformer := factory.Core().V1().Pods()
	informer := podInformer.Informer()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			oldPod, ok := old.(*corev1.Pod)
			if !ok {
				return
			}
			newPod, ok := cur.(*corev1.Pod)
			if !ok {
				return
			}
			// Resync replays every pod unchanged; only drift is interesting.
			if maps.Equal(oldPod.Labels, newPod.Labels) && oldPod.Status.PodIP == newPod.Status.PodIP {
				return
			}
			w.onPodUpdate(oldPod, newPod)
		},
	}); err != nil {
		klog.Errorf("failed to watch pods in namespace %s: %v", namespace, err)
	}

	klog.Infof("P/D: watching pods in namespace %s (scope %q)", namespace, scope)
	factory.Start(stopCh)

	return &pdPodWatch{lister: podInformer.Lister(), synced: informer.HasSynced}
}

// podCacheScope - a label selector that every pod either pool could match must
// satisfy, so the cache can be narrowed to candidates.
//
// Only equality-style requirements imply the key is present. Kubernetes matches
// `key!=value` and `notin` against objects carrying no such label at all, and
// DoesNotExist obviously so, so a selector using any of those cannot narrow the
// cache and the scope opens back up to the whole namespace.
//
// Requirements are ANDed here, which is why every pool has to agree on the same
// key set: with two pools keyed differently, requiring both keys would hide the
// pods of each from the other.
func podCacheScope(pools []pdPool) string {
	var shared []string

	for i, pool := range pools {
		requirements, selectable := pool.selector.Requirements()
		if !selectable || len(requirements) == 0 {
			return ""
		}

		keys := make([]string, 0, len(requirements))
		for _, req := range requirements {
			switch req.Operator() {
			case selection.Equals, selection.DoubleEquals, selection.In, selection.Exists:
				keys = append(keys, req.Key())
			default:
				return ""
			}
		}
		sort.Strings(keys)

		if i == 0 {
			shared = keys
			continue
		}
		if !slicesEqual(shared, keys) {
			return ""
		}
	}

	return strings.Join(shared, ",")
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// enqueueServicesForPod - requeue every disaggregated service whose pools the
// pod belonged to, or now belongs to. Both sides matter: a pod leaving a pool
// has to be noticed as well as one joining.
func (m *Manager) enqueueServicesForPod(oldPod, newPod *corev1.Pod) {
	svcs, err := m.serviceLister.Services(newPod.Namespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("P/D: failed to list services in namespace %s: %v", newPod.Namespace, err)
		return
	}

	oldLabels := labels.Set(oldPod.Labels)
	newLabels := labels.Set(newPod.Labels)

	for _, svc := range svcs {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		if disagg, err := annoBool(svc, pdDisaggAnnotation); err != nil || !disagg {
			continue
		}

		pools, err := parsePDPools(svc)
		if err != nil {
			continue
		}

		for _, pool := range pools {
			if !pool.selector.Matches(oldLabels) && !pool.selector.Matches(newLabels) {
				continue
			}

			klog.Infof("P/D: pod %s/%s role labels changed, resyncing service %s/%s",
				newPod.Namespace, newPod.Name, svc.Namespace, svc.Name)
			m.queue.Add(LbCacheKey{Namespace: svc.Namespace, Name: svc.Name})
			break
		}
	}
}
