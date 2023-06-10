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

package k8s

import (
	"os"

	tk "github.com/loxilb-io/loxilib"
	"k8s.io/klog/v2"
)

// This const are environment variable of k8s pod
const (
	NodeNameEnvKey     = "NODE_NAME"
	podNameEnvKey      = "POD_NAME"
	podNamespaceEnvKey = "POD_NAMESPACE"
	svcAcctNameEnvKey  = "SERVICEACCOUNT_NAME"

	defaultloxilbNamespace = "kube-system"
)

func GetNodeName() (string, error) {
	nodeName := os.Getenv(NodeNameEnvKey)
	if nodeName != "" {
		return nodeName, nil
	}

	klog.Infof("Environment variable %s not found, using hostname instead", NodeNameEnvKey)
	var err error
	nodeName, err = os.Hostname()
	if err != nil {
		klog.Errorf("Failed to get local hostname: %v", err)
		return "", err
	}
	return nodeName, nil
}

// Check address belongs to desired family
func AddrInFamily(addrType string, addr string) bool {
	if ((addrType == "ipv4" || addrType == "ipv6to4") && tk.IsNetIPv4(addr)) ||
		(addrType == "ipv6" && tk.IsNetIPv6(addr)) {
		return true
	}
	return false
}
