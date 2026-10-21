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

// Package jwtauthprofile registers JWTAuthProfiles on every
// loxilb-inference-gateway peer that enforces JWT auth profiles.
//
// The gateway keeps profiles per instance and refuses a rule naming one it
// does not have. Owning both the profiles and the rules that name them lets
// kube-loxilb put a profile on a peer before any rule that needs it - on a
// peer added later, and on one that restarted without its snapshot.
package jwtauthprofile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
	jwtv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/jwtauthprofile/v1"
	jwtclientset "github.com/loxilb-io/kube-loxilb/pkg/jwtauthprofile-client/clientset/versioned"
	jwtinformers "github.com/loxilb-io/kube-loxilb/pkg/jwtauthprofile-client/informers/externalversions"
	jwtlisters "github.com/loxilb-io/kube-loxilb/pkg/jwtauthprofile-client/listers/jwtauthprofile/v1"
)

const (
	mgrName = "JWTAuthProfileManager"
	// CRDName - the JWTAuthProfile CRD. The manager starts once it exists.
	CRDName = "jwtauthprofiles.jwtauthprofile.loxilb.io"

	resyncPeriod   = 60 * time.Second
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
	requestTimeout = 5 * time.Second

	// ReasonProfileRefused - a peer refused the profile and said why.
	ReasonProfileRefused = "ProfileRefused"
	// ReasonProfileInUse - the profile is being deleted and a rule on a peer
	// still names it; the delete waits for the rule to go.
	ReasonProfileInUse = "ProfileInUse"
)

// errRetry - something is still pending; the queue comes back with backoff.
var errRetry = errors.New("not settled on every peer yet")

// Manager - registers JWTAuthProfiles on the gateway peers.
type Manager struct {
	extClient   apiextensionclientset.Interface
	client      jwtclientset.Interface
	lister      jwtlisters.JWTAuthProfileLister
	synced      cache.InformerSynced
	loxiClients *api.LoxiClientPool
	recorder    record.EventRecorder
	queue       workqueue.TypedRateLimitingInterface[string]

	mu sync.RWMutex
	// present - the peers known to hold each profile, by name and peer host:
	// a registration succeeded there since the peer last came up. A spec
	// change that a peer refuses leaves the previous version there, so the
	// profile stays present for the rules that name it.
	present map[string]map[string]bool
	// refused - the refusal last reported for each profile and peer, so one
	// is reported once rather than on every pass.
	refused map[string]map[string]string

	// onChange - called with a profile's name when the peers holding it
	// change; set by whoever holds rules back for it.
	onChange atomic.Value
}

// NewManager - a Manager over the JWTAuthProfile informer of factory. Nothing
// runs until Run.
func NewManager(kubeClient clientset.Interface, extClient apiextensionclientset.Interface,
	client jwtclientset.Interface, factory jwtinformers.SharedInformerFactory,
	loxiClients *api.LoxiClientPool) *Manager {

	informer := factory.Jwtauthprofile().V1().JWTAuthProfiles()

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	m := &Manager{
		extClient:   extClient,
		client:      client,
		lister:      informer.Lister(),
		synced:      informer.Informer().HasSynced,
		loxiClients: loxiClients,
		recorder:    broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kube-loxilb"}),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](minRetryDelay, maxRetryDelay),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "jwtAuthProfile"}),
		present: map[string]map[string]bool{},
		refused: map[string]map[string]string{},
	}

	enqueue := func(obj interface{}) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			m.queue.Add(key)
		}
	}
	_, _ = informer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, cur interface{}) { enqueue(cur) },
		DeleteFunc: enqueue,
	}, resyncPeriod)

	return m
}

// SetOnChange - fn is called with a profile's name whenever the peers holding
// it change, so the rules held back for it can be sent.
func (m *Manager) SetOnChange(fn func(name string)) {
	m.onChange.Store(fn)
}

// HasSynced - whether the informer has listed the profiles.
func (m *Manager) HasSynced() bool {
	return m.synced()
}

// Programmed - whether a JWTAuthProfile of that name exists, and whether the
// peer at host holds it.
func (m *Manager) Programmed(name, host string) (exists, programmed bool) {
	profile, err := m.lister.Get(name)
	if err != nil || profile.DeletionTimestamp != nil {
		return false, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	return true, m.present[name][host]
}

// PeerBack - a peer came (back) up and may hold none of the profiles: it is
// new, or restarted without its snapshot. It is not counted as holding any
// until registered again, and every profile is queued.
func (m *Manager) PeerBack(c *api.LoxiClient) {
	m.mu.Lock()
	for _, hosts := range m.present {
		delete(hosts, c.Host)
	}
	m.mu.Unlock()

	profiles, err := m.lister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, profile := range profiles {
		m.queue.Add(profile.Name)
	}
}

// WaitForCRD - wait until the JWTAuthProfile CRD exists. False when stopped
// first.
func (m *Manager) WaitForCRD(stopCh <-chan struct{}) bool {
	logged := false
	err := wait.PollUntilContextCancel(wait.ContextForChannel(stopCh), 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if _, err := m.extClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, CRDName, metav1.GetOptions{}); err != nil {
				if !logged {
					klog.Infof("%s: waiting for the %s CRD; rules naming a JWT auth profile are sent as they are until it exists",
						mgrName, CRDName)
					logged = true
				}
				return false, nil
			}
			klog.Infof("%s: %s CRD found", mgrName, CRDName)
			return true, nil
		})
	return err == nil
}

// Run - process the queue until stopCh closes.
func (m *Manager) Run(stopCh <-chan struct{}) {
	defer m.queue.ShutDown()

	klog.Infof("Starting %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(mgrName, stopCh, m.synced) {
		return
	}

	go wait.Until(m.worker, time.Second, stopCh)
	<-stopCh
}

func (m *Manager) worker() {
	for m.processNext() {
	}
}

func (m *Manager) processNext() bool {
	name, quit := m.queue.Get()
	if quit {
		return false
	}
	defer m.queue.Done(name)

	if err := m.reconcile(name); err != nil {
		m.queue.AddRateLimited(name)
		if !errors.Is(err, errRetry) {
			klog.Errorf("%s: %s: %v", mgrName, name, err)
		}
		return true
	}
	m.queue.Forget(name)
	return true
}

// reconcile - make the peers agree with the JWTAuthProfile of that name.
func (m *Manager) reconcile(name string) error {
	profile, err := m.lister.Get(name)
	if apierrors.IsNotFound(err) {
		m.forget(name)
		return nil
	}
	if err != nil {
		return err
	}
	if profile.DeletionTimestamp != nil {
		return m.remove(profile)
	}

	if !slices.Contains(profile.Finalizers, jwtv1.FinalizerName) {
		updated := profile.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, jwtv1.FinalizerName)
		if profile, err = m.client.JwtauthprofileV1().JWTAuthProfiles().Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}

	model := modelOf(profile)
	var peers []jwtv1.JWTAuthProfilePeer
	pending, gained := false, false
	for _, c := range m.targets() {
		peer := jwtv1.JWTAuthProfilePeer{Host: c.Host}
		if !c.IsAlive {
			// Registered when it comes back up (PeerBack); retrying now
			// would only keep the profile queued while the peer is down.
			peer.LastError = "not reachable"
			peers = append(peers, peer)
			continue
		}

		if err := register(c, model); err != nil {
			peer.LastError = err.Error()
			pending = true
			m.reportRefusal(profile, c, err)
		} else {
			peer.Programmed = true
			if m.markPresent(name, c.Host) {
				gained = true
			}
		}
		peers = append(peers, peer)
	}

	if err := m.updateStatus(profile, peers); err != nil {
		return err
	}
	if gained {
		m.notify(name)
	}
	if pending {
		return errRetry
	}
	return nil
}

// remove - take a deleted profile off every peer, then let the object go.
//
// A peer still holding a rule that names the profile refuses with 409; the
// delete waits for the rule to go, which is how the gateway keeps a rule from
// pointing at nothing. A peer that is down is not waited for: it may never
// come back, and if it does, the profile it kept is named by no rule kube-
// loxilb sends, since the object is gone.
func (m *Manager) remove(profile *jwtv1.JWTAuthProfile) error {
	name := profile.Name
	inUse, pending := "", false
	for _, c := range m.targets() {
		if !c.IsAlive {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		err := c.JWTAuthProfile().Delete(ctx, name)
		cancel()

		switch {
		case err == nil || api.IsNotFound(err):
			m.unmarkPresent(name, c.Host)
		case api.StatusCodeOf(err) == http.StatusConflict:
			inUse = fmt.Sprintf("loxilb-lb(%s): %v", c.Host, err)
		default:
			klog.Infof("%s: %s: not deleted on loxilb-lb(%s) yet: %v", mgrName, name, c.Host, err)
			pending = true
		}
	}

	if inUse != "" {
		m.reportOnce(profile, "", ReasonProfileInUse, inUse)
		return errRetry
	}
	if pending {
		return errRetry
	}

	if slices.Contains(profile.Finalizers, jwtv1.FinalizerName) {
		updated := profile.DeepCopy()
		updated.Finalizers = slices.DeleteFunc(updated.Finalizers, func(f string) bool { return f == jwtv1.FinalizerName })
		if _, err := m.client.JwtauthprofileV1().JWTAuthProfiles().Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	m.forget(name)
	m.notify(name)
	return nil
}

// targets - the peers a profile goes to: gateways that list the
// jwt_auth_profile feature. Plain loxilb has no profiles, and a gateway
// without the feature is never sent a rule that names one.
func (m *Manager) targets() []*api.LoxiClient {
	var targets []*api.LoxiClient
	for _, c := range m.loxiClients.Clients {
		if c.IsInferenceGateway() && c.SupportsFeature(api.FeatureJWTAuthProfile) {
			targets = append(targets, c)
		}
	}
	return targets
}

func register(c *api.LoxiClient, model *api.JWTAuthProfileModel) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	if err := c.JWTAuthProfile().Create(ctx, model); err != nil && !api.IsConflict(err) {
		return err
	}
	return nil
}

// modelOf - the gateway's body for a JWTAuthProfile.
func modelOf(p *jwtv1.JWTAuthProfile) *api.JWTAuthProfileModel {
	s := p.Spec
	return &api.JWTAuthProfileModel{
		Name:                     p.Name,
		Issuer:                   s.Issuer,
		JWKSURL:                  s.JWKSURL,
		Audiences:                s.Audiences,
		Algs:                     s.Algs,
		LeewaySec:                int(s.LeewaySec),
		RefreshSec:               int(s.RefreshSec),
		TenantClaim:              s.TenantClaim,
		UserClaim:                s.UserClaim,
		ModelsClaim:              s.ModelsClaim,
		RolesClaim:               s.RolesClaim,
		ModelRolePrefix:          s.ModelRolePrefix,
		UsernameClaim:            s.UsernameClaim,
		ModelAuthz:               s.ModelAuthz,
		DefaultTenant:            s.DefaultTenant,
		ForwardIdentity:          s.ForwardIdentity,
		AuthorizationPassthrough: s.AuthorizationPassthrough,
		PriorityClass:            s.PriorityClass,
	}
}

// updateStatus - write each peer's answer and the Programmed condition, when
// they changed.
func (m *Manager) updateStatus(profile *jwtv1.JWTAuthProfile, peers []jwtv1.JWTAuthProfilePeer) error {
	sort.Slice(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })

	status := profile.Status.DeepCopy()
	status.Peers = peers

	condition := metav1.Condition{
		Type:               jwtv1.ConditionProgrammed,
		Status:             metav1.ConditionTrue,
		Reason:             "Programmed",
		Message:            fmt.Sprintf("on all %d loxilb-inference-gateway peers that enforce JWT auth profiles", len(peers)),
		ObservedGeneration: profile.Generation,
	}
	missing := 0
	for _, peer := range peers {
		if !peer.Programmed {
			missing++
		}
	}
	switch {
	case len(peers) == 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, "NoPeers"
		condition.Message = "no loxilb-inference-gateway peer lists the jwt_auth_profile feature"
	case missing > 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, "NotProgrammed"
		condition.Message = fmt.Sprintf("missing on %d of %d peers; see status.peers", missing, len(peers))
	}
	meta.SetStatusCondition(&status.Conditions, condition)

	if equality.Semantic.DeepEqual(status, &profile.Status) {
		return nil
	}

	updated := profile.DeepCopy()
	updated.Status = *status
	_, err := m.client.JwtauthprofileV1().JWTAuthProfiles().UpdateStatus(context.Background(), updated, metav1.UpdateOptions{})
	return err
}

func (m *Manager) markPresent(name, host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.present[name] == nil {
		m.present[name] = map[string]bool{}
	}
	if m.present[name][host] {
		return false
	}
	m.present[name][host] = true
	if refused := m.refused[name]; refused != nil {
		delete(refused, host)
	}
	return true
}

func (m *Manager) unmarkPresent(name, host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.present[name], host)
}

func (m *Manager) forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.present, name)
	delete(m.refused, name)
}

func (m *Manager) notify(name string) {
	if fn, ok := m.onChange.Load().(func(string)); ok && fn != nil {
		fn(name)
	}
}

// reportRefusal - a peer refused the profile; say so on it, once per answer.
func (m *Manager) reportRefusal(profile *jwtv1.JWTAuthProfile, c *api.LoxiClient, err error) {
	klog.Infof("%s: %s: not registered on loxilb-lb(%s): %v", mgrName, profile.Name, c.Host, err)
	if api.IsClientError(err) {
		m.reportOnce(profile, c.Host, ReasonProfileRefused, fmt.Sprintf("loxilb-lb(%s): %v", c.Host, err))
	}
}

// reportOnce - a Warning on the profile, unless the same one was the last
// reported for that peer.
func (m *Manager) reportOnce(profile *jwtv1.JWTAuthProfile, host, reason, message string) {
	m.mu.Lock()
	if m.refused[profile.Name] == nil {
		m.refused[profile.Name] = map[string]string{}
	}
	last := m.refused[profile.Name][host]
	m.refused[profile.Name][host] = reason + message
	m.mu.Unlock()
	if last == reason+message {
		return
	}

	// By reference: the recorder's scheme does not know the type.
	ref := &corev1.ObjectReference{
		APIVersion: jwtv1.SchemeGroupVersion.String(), Kind: "JWTAuthProfile",
		Name: profile.Name, UID: profile.UID, ResourceVersion: profile.ResourceVersion,
	}
	m.recorder.Event(ref, corev1.EventTypeWarning, reason, message)
}
