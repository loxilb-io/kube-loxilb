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
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// ErrProfileNotFound - the rule names a JWT auth profile that has no
// JWTAuthProfile, so no peer is given it.
var ErrProfileNotFound = errors.New("no JWTAuthProfile of that name")

// ErrProfileNotProgrammed - the JWTAuthProfile is not on that peer yet.
var ErrProfileNotProgrammed = errors.New("the JWT auth profile is not on the loxilb peer yet")

// JWTProfiles - what kube-loxilb knows of the JWT auth profiles it registers
// on the gateway peers.
type JWTProfiles interface {
	// Programmed - whether a JWTAuthProfile of that name exists, and whether
	// the peer at host holds it.
	Programmed(name, host string) (exists, programmed bool)
}

type jwtProfilesHolder struct{ profiles JWTProfiles }

// SetJWTProfiles - from now on, a rule naming a JWT auth profile goes to a
// peer only once kube-loxilb has registered the profile there. Set once the
// JWTAuthProfile CRD exists and its informer has synced; until then rules go
// as they are and the gateway answers for the profile.
func (m *Manager) SetJWTProfiles(profiles JWTProfiles) {
	m.jwtProfiles.Store(jwtProfilesHolder{profiles: profiles})
}

// checkProfile - hold back a rule naming a JWT auth profile the peer does not
// hold yet.
//
// The gateway would refuse it anyway. Holding it here instead says why on the
// Service, and does not depend on that refusal: the rule is sent once the
// profile is there, when the profile manager queues the Services naming it.
func (m *Manager) checkProfile(c *api.LoxiClient, model *api.LoadBalancerModel) error {
	if !model.Service.UsesJWTAuth() {
		return nil
	}
	holder, ok := m.jwtProfiles.Load().(jwtProfilesHolder)
	if !ok || holder.profiles == nil {
		return nil
	}

	name := model.Service.JwtAuthProfile
	exists, programmed := holder.profiles.Programmed(name, c.Host)
	switch {
	case !exists:
		return fmt.Errorf("%s names JWT auth profile %q, which has no JWTAuthProfile: %w",
			jwtAuthProfileAnnotation, name, ErrProfileNotFound)
	case !programmed:
		return fmt.Errorf("JWT auth profile %q is not on loxilb-lb(%s) yet: %w", name, c.Host, ErrProfileNotProgrammed)
	}
	return nil
}

// QueueServicesUsingProfile - queue every Service naming the JWT auth profile,
// so what was held back for it is looked at again. Safe to call from any
// goroutine.
func (m *Manager) QueueServicesUsingProfile(name string) {
	if m.serviceLister == nil || m.queue == nil {
		return
	}
	services, err := m.serviceLister.List(labels.Everything())
	if err != nil {
		return
	}
	queued := 0
	for _, svc := range services {
		if svc.Annotations[jwtAuthProfileAnnotation] == name {
			m.queue.Add(LbCacheKey{Namespace: svc.Namespace, Name: svc.Name})
			queued++
		}
	}
	klog.V(2).Infof("JWT auth profile %s changed: %d services queued", name, queued)
}

type peerBackHook struct{ fn func(*api.LoxiClient) }

// SetPeerBackHook - fn is called when a peer comes (back) up, before its rules
// are queued to be put back, so what the rules need can go first.
func (m *Manager) SetPeerBackHook(fn func(*api.LoxiClient)) {
	m.peerBackHook.Store(peerBackHook{fn: fn})
}

func (m *Manager) callPeerBackHook(c *api.LoxiClient) {
	if hook, ok := m.peerBackHook.Load().(peerBackHook); ok && hook.fn != nil {
		hook.fn(c)
	}
}
