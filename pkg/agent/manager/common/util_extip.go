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

package common

import (
	"net"
	"strings"

	tk "github.com/loxilb-io/loxilib"
)

// GenExtIPName converts an IP address to a Kubernetes-safe hostname format.
// - For IPv6, replaces ':' with '-' to make it label-safe
// - Adds zone prefix if zone is not empty
// - For unspecified IPs (0.0.0.0), returns loxiInstAddrMap hosts or "llbanyextip"
func GenExtIPName(zone string, loxiInstAddrMap map[string]net.IP, ipStr string) []string {
	var hosts []string

	prefix := ""
	if zone != "" {
		prefix = zone + "-"
	}

	IP := net.ParseIP(ipStr)
	if IP != nil {
		if IP.IsUnspecified() {
			if len(loxiInstAddrMap) <= 0 {
				return []string{"llbanyextip"}
			} else {
				for _, host := range loxiInstAddrMap {
					hosts = append(hosts, host.String())
				}
			}
			return hosts
		}
	}

	if tk.IsNetIPv6(ipStr) {
		ipStrSlice := strings.Split(ipStr, ":")
		ipStr = strings.Join(ipStrSlice, "-")
	}

	return []string{prefix + ipStr}
}

// ParseIngressHostname parses a Kubernetes LoadBalancer Ingress hostname back to an IP address.
// - Handles zone-prefixed hostnames
// - Converts '-' back to ':' for IPv6 addresses
// - Returns "0.0.0.0" for special hostnames like "llbanyextip"
func ParseIngressHostname(hostname, zone string) string {
	if hostname == "llbanyextip" {
		return "0.0.0.0"
	}

	llbHost := strings.Split(hostname, "-")

	if len(llbHost) < 2 {
		// No zone prefix, try to parse as IP
		if net.ParseIP(llbHost[0]) != nil {
			return llbHost[0]
		}
		return ""
	}

	// Check if it has zone prefix
	if zone != "" && llbHost[0] == zone {
		// Remove zone prefix
		llbHost = llbHost[1:]
	}

	// Try to parse as IPv4 first
	if net.ParseIP(llbHost[0]) != nil {
		return llbHost[0]
	}

	// Try to reconstruct IPv6 by joining with ':'
	if len(llbHost) > 1 {
		ipStr := strings.Join(llbHost, ":")
		if net.ParseIP(ipStr) != nil {
			return ipStr
		}
	}

	return ""
}
