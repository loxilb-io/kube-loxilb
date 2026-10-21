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
	"fmt"
	"net"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// readBackTimeout - how long each request of a read-back may take.
const readBackTimeout = 5 * time.Second

// readBack - read a rule back from a gateway peer by its id, and settle what
// a successful answer to the POST did not say.
//
// 200 and 409 both read as success, but 409 also answers a replace the gateway
// refused because it would change a field it does not change in place, and a
// replace that leaves out api_key_auth or jwt_auth_profile keeps the old one -
// so a peer that was down while they changed comes back still holding them.
// The rule as stored settles it:
//
//   - stored as sent: installed.
//   - not stored under its id, or stored with other values: deleted and added
//     again, once, and read back again.
//   - then still without the credential policy it was sent: the peer accepts
//     the fields and drops them, which serves the rule unauthenticated. It is
//     removed and the peer is held (ErrAuthUnsupported).
//
// Anything else that differs after a fresh add is how this peer stores the
// rule rather than something left over, and is let be: re-adding it on every
// sync would only drop connections. A rule that cannot be read back at all is
// left as the POST answered.
func (m *Manager) readBack(c *api.LoxiClient, sent *api.LoadBalancerModel) error {
	got, err := readRule(c, sent.Service.ID)
	if err != nil {
		klog.V(2).Infof("loxilb-lb(%s): rule %s not read back: %v", c.Host, sent.Service.Name, err)
		return nil
	}
	diff := ruleDiff(&sent.Service, got)
	if len(diff) == 0 {
		return nil
	}

	klog.Infof("loxilb-lb(%s): rule %s %s; deleting it and adding it again",
		c.Host, sent.Service.Name, strings.Join(diff, ", "))
	if err := deleteRule(c, sent); err != nil {
		return err
	}
	if err := createRule(c, sent); err != nil {
		return err
	}

	got, err = readRule(c, sent.Service.ID)
	if err != nil {
		klog.V(2).Infof("loxilb-lb(%s): rule %s not read back after adding it again: %v", c.Host, sent.Service.Name, err)
		return nil
	}
	if dropsCredentialPolicy(&sent.Service, got) {
		if err := deleteRule(c, sent); err != nil {
			return err
		}
		return fmt.Errorf("loxilb-lb(%s) stored the rule without the credential policy it was sent, "+
			"so it would serve it unauthenticated; removed: %w", c.Host, ErrAuthUnsupported)
	}
	if diff := ruleDiff(&sent.Service, got); len(diff) != 0 {
		klog.Warningf("loxilb-lb(%s): rule %s still reads back with %s after it was added again; leaving it",
			c.Host, sent.Service.Name, strings.Join(diff, ", "))
	}

	return nil
}

// readRule - the rule stored under id, or nil when there is none.
func readRule(c *api.LoxiClient, id string) (*api.RuleReadBack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readBackTimeout)
	defer cancel()

	got, err := c.LoadBalancer().GetByID(ctx, id)
	if api.IsNotFound(err) {
		return nil, nil
	}

	return got, err
}

func deleteRule(c *api.LoxiClient, lb *api.LoadBalancerModel) error {
	ctx, cancel := context.WithTimeout(context.Background(), readBackTimeout)
	defer cancel()

	if err := c.LoadBalancer().Delete(ctx, lb); err != nil && !api.IsNotFound(err) {
		return err
	}
	return nil
}

func createRule(c *api.LoxiClient, lb *api.LoadBalancerModel) error {
	ctx, cancel := context.WithTimeout(context.Background(), readBackTimeout)
	defer cancel()

	if err := c.LoadBalancer().Create(ctx, lb); err != nil && !api.IsConflict(err) {
		return err
	}
	return nil
}

// ruleDiff - the fields in which the stored rule differs from the one sent,
// or nil when it is stored as sent.
//
// The path match mode is left out: the gateway stores it canonicalized, so
// the value sent and the value read can differ for the same rule.
func ruleDiff(sent *api.LoadBalancerService, got *api.RuleReadBack) []string {
	if got == nil {
		return []string{"is not stored under its id"}
	}

	var diff []string
	differ := func(field string, same bool) {
		if !same {
			diff = append(diff, field+" differs")
		}
	}

	sentIP, gotIP := net.ParseIP(sent.ExternalIP), net.ParseIP(got.ExternalIP)
	differ("externalIP", sentIP != nil && sentIP.Equal(gotIP) || sentIP == nil && sent.ExternalIP == got.ExternalIP)
	differ("port", sent.Port == got.Port)
	differ("protocol", strings.EqualFold(sent.Protocol, got.Protocol))
	differ("host", sent.Host == got.Host)
	differ("path_prefix", sent.PathPrefix == got.PathPrefix)
	differ("model_name", sent.ModelName == got.ModelName)
	differ("mode", sent.Mode == got.Mode)
	differ("security", sent.Security == got.Security)
	differ("egress", sent.Egress == got.Egress)
	differ("kvEngineType", kvEngineOf(sent.KvEngineType) == kvEngineOf(got.KvEngineType))
	differ("api_key_auth", sent.APIKeyAuth == got.APIKeyAuth)
	differ("jwt_auth_profile", sent.JwtAuthProfile == got.JwtAuthProfile)

	return diff
}

// kvEngineOf - "" is the default engine, as the gateway reads it.
func kvEngineOf(engine string) string {
	if engine == "" {
		return "vllm"
	}
	return engine
}

// dropsCredentialPolicy - the rule was sent a credential policy and is stored
// without it.
func dropsCredentialPolicy(sent *api.LoadBalancerService, got *api.RuleReadBack) bool {
	return got != nil && sent.APIKeyAuth != "" &&
		(got.APIKeyAuth != sent.APIKeyAuth || got.JwtAuthProfile != sent.JwtAuthProfile)
}
