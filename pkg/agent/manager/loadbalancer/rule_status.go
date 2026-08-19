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
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

// PortStatus.Error values.
//
// A fixed vocabulary, never loxilb's own wording. The field is validated as a
// qualified name and capped at 316 characters, and the call that writes it is
// the same call that publishes the ingress IP - so one malformed string would
// take the VIP announcement down with it. Provider-specific values must read
// as <dns1123 subdomain>/CamelCase.
const (
	portStatusDegraded = "loxilb.io/RuleDegraded"
	portStatusOffline  = "loxilb.io/RuleOffline"
	portStatusUnknown  = "loxilb.io/RuleUnknown"
)

// operatingStatusSeverity - how much attention a status deserves.
//
// NO_MONITOR ranks below ONLINE rather than beside it. It is not a healthier
// verdict; it is the absence of one - the gateway checks for a monitor before
// it looks at any endpoint, so a service without liveness enabled always
// reports it and never reports anything else. Ranking it lowest makes an
// actual verdict win the tie, which keeps the reduction independent of the
// order peers happen to answer in. Neither produces an Error either way.
func operatingStatusSeverity(status string) int {
	switch status {
	case api.OperatingStatusNoMonitor:
		return -1
	case api.OperatingStatusOnline:
		return 0
	case api.OperatingStatusDegraded:
		return 1
	case api.OperatingStatusOffline:
		return 2
	default:
		// Not reachable from deriveOperatingStatus; ranked above healthy so an
		// unexpected value is noticed rather than absorbed.
		return 1
	}
}

// worstOperatingStatus - the status to report for a rule fanned out to several
// peers.
//
// Worst-of. The field answers "does this need attention", and one peer being
// OFFLINE answers yes however well the others are doing. Which peer it was
// goes in the event, where there is no length limit.
func worstOperatingStatus(statuses []string) string {
	worst := ""
	for _, status := range statuses {
		if worst == "" || operatingStatusSeverity(status) > operatingStatusSeverity(worst) {
			worst = status
		}
	}

	return worst
}

// portStatusErrorFor - the Error value for a status, or nil when there is
// nothing wrong to record.
//
// ONLINE and NO_MONITOR both yield nil: no problem, and no information, are
// both "no error". An empty string is not a substitute - it would fail
// validation.
func portStatusErrorFor(status string) *string {
	var value string

	switch status {
	case api.OperatingStatusDegraded:
		value = portStatusDegraded
	case api.OperatingStatusOffline:
		value = portStatusOffline
	case api.OperatingStatusOnline, api.OperatingStatusNoMonitor, "":
		return nil
	default:
		value = portStatusUnknown
	}

	return &value
}

// ruleStatus - the aggregated status of one programmed rule.
type ruleStatus struct {
	externalIP string
	port       uint16
	protocol   string
	status     string
	// peers - which peers reported the worst status, for the event text.
	peers []string
}

// collectRuleStatus - read the operating status of every rule of this service
// from every gateway peer, and reduce each rule to its worst report.
//
// Plain upstream peers are skipped rather than queried: the sub-resource does
// not exist there and a 404 must not be read as ill health.
func (m *Manager) collectRuleStatus(ctx context.Context, svc *corev1.Service, entry *LbCacheEntry) []ruleStatus {
	var gateways []*api.LoxiClient
	for _, c := range m.LoxiClients.Clients {
		if c.IsInferenceGateway() {
			gateways = append(gateways, c)
		}
	}
	if len(gateways) == 0 {
		return nil
	}

	var collected []ruleStatus
	for _, sp := range entry.LbServicePairs {
		var statuses []string
		var peers []string

		for _, c := range gateways {
			status, err := c.LoadBalancer().Status(ctx, sp.ExternalIP, sp.Port, sp.Protocol)
			if err != nil {
				if api.StatusCodeOf(err) == http.StatusNotFound {
					// The rule is gone from that peer. Not degraded service -
					// a missing rule, which reconciliation has to put back.
					message := fmt.Sprintf("loxilb-lb(%s) has no rule for %s:%d/%s any more",
						c.Host, sp.ExternalIP, sp.Port, sp.Protocol)
					klog.Errorf("service %s/%s: %s", svc.Namespace, svc.Name, message)
					m.recordServiceWarning(svc, ReasonRuleMissing, message)
					continue
				}
				klog.V(4).Infof("loxilb-lb(%s): rule status unreadable for %s:%d/%s: %v",
					c.Host, sp.ExternalIP, sp.Port, sp.Protocol, err)
				continue
			}

			statuses = append(statuses, status.OperatingStatus)
			peers = append(peers, c.Host)
		}

		if len(statuses) == 0 {
			continue
		}

		worst := worstOperatingStatus(statuses)
		var worstPeers []string
		for i, status := range statuses {
			if status == worst {
				worstPeers = append(worstPeers, peers[i])
			}
		}

		collected = append(collected, ruleStatus{
			externalIP: sp.ExternalIP,
			port:       sp.Port,
			protocol:   sp.Protocol,
			status:     worst,
			peers:      worstPeers,
		})
	}

	return collected
}

// syncRuleStatus - reflect what loxilb says about this service's rules into the
// Service, and report the changes.
//
// Runs on the reconcile path rather than on a loop of its own, the same shape
// as the GPU arming check, and writes only when something moved: this shares
// the call that publishes the ingress IP, and one write per service per
// reconcile would be a lot of writes for nothing.
func (m *Manager) syncRuleStatus(svc *corev1.Service, cacheKey string) {
	entry, ok := m.lbCache[cacheKey]
	if !ok || len(entry.LbServicePairs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	collected := m.collectRuleStatus(ctx, svc, entry)
	if len(collected) == 0 {
		return
	}

	if entry.RuleStatus == nil {
		entry.RuleStatus = make(map[string]string)
	}

	changed := false
	for _, rs := range collected {
		key := GenSPKey(rs.externalIP, rs.port, rs.protocol)
		previous, seen := entry.RuleStatus[key]
		if seen && previous == rs.status {
			continue
		}

		entry.RuleStatus[key] = rs.status
		changed = true

		// Report the edge, as with GPU arming. Recovering is worth saying too,
		// so the operator learns the incident closed.
		message := fmt.Sprintf("%s:%d/%s is %s on %v", rs.externalIP, rs.port, rs.protocol,
			rs.status, rs.peers)
		if operatingStatusSeverity(rs.status) > 0 {
			klog.Errorf("service %s/%s: %s", svc.Namespace, svc.Name, message)
			m.recordServiceWarning(svc, ReasonRuleUnhealthy, message)
		} else if seen {
			klog.Infof("service %s/%s: %s", svc.Namespace, svc.Name, message)
			m.recordServiceEvent(svc, corev1.EventTypeNormal, ReasonRuleHealthy, message)
		}
	}

	if !changed {
		return
	}

	if err := m.updateServicePortStatus(svc, collected); err != nil {
		klog.Errorf("failed to update port status for service %s/%s: %v", svc.Namespace, svc.Name, err)
		// Forget what was written so the next reconcile tries again.
		for _, rs := range collected {
			delete(entry.RuleStatus, GenSPKey(rs.externalIP, rs.port, rs.protocol))
		}
	}
}

// portStatusesFor - the PortStatus list for one ingress address.
//
// Every port the Service declares gets an entry, which is what the field's
// contract asks for; ports with nothing to report carry a nil Error. Worst-of
// applies within a port, never across them.
func portStatusesFor(svc *corev1.Service, externalIP string, collected []ruleStatus) []corev1.PortStatus {
	byPort := make(map[string]string)
	for _, rs := range collected {
		if rs.externalIP != externalIP {
			continue
		}
		key := fmt.Sprintf("%d/%s", rs.port, rs.protocol)
		if existing, ok := byPort[key]; !ok || operatingStatusSeverity(rs.status) > operatingStatusSeverity(existing) {
			byPort[key] = rs.status
		}
	}

	ports := make([]corev1.PortStatus, 0, len(svc.Spec.Ports))
	for _, port := range svc.Spec.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		key := fmt.Sprintf("%d/%s", port.Port, strings.ToLower(string(protocol)))
		ports = append(ports, corev1.PortStatus{
			Port:     port.Port,
			Protocol: protocol,
			Error:    portStatusErrorFor(byPort[key]),
		})
	}

	return ports
}

// updateServicePortStatus - write the port statuses onto the matching ingress
// entries.
//
// The ingress list is built elsewhere and appended to only when the address is
// new, so this updates entries in place rather than adding any: the address is
// already published by the time there is a status to report.
func (m *Manager) updateServicePortStatus(svc *corev1.Service, collected []ruleStatus) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	cur, err := m.kubeClient.CoreV1().Services(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	updated := false
	for i := range cur.Status.LoadBalancer.Ingress {
		ingress := &cur.Status.LoadBalancer.Ingress[i]
		address := ingress.IP
		if address == "" {
			address = ingress.Hostname
		}

		ports := portStatusesFor(cur, address, collected)
		if portStatusesEqual(ingress.Ports, ports) {
			continue
		}

		ingress.Ports = ports
		updated = true
	}

	if !updated {
		return nil
	}

	_, err = m.kubeClient.CoreV1().Services(cur.Namespace).UpdateStatus(ctx, cur, metav1.UpdateOptions{})

	return err
}

func portStatusesEqual(a, b []corev1.PortStatus) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Port != b[i].Port || a[i].Protocol != b[i].Protocol {
			return false
		}
		switch {
		case a[i].Error == nil && b[i].Error == nil:
		case a[i].Error == nil || b[i].Error == nil:
			return false
		case *a[i].Error != *b[i].Error:
			return false
		}
	}

	return true
}
