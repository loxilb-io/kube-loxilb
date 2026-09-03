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

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// KubeVirt runs every virtual machine inside a "virt-launcher" pod. The pod carries the multus
// annotations of the VM's secondary networks, but the address KubeVirt hands to the guest is only
// reliably known from the VirtualMachineInstance status (guest agent / pod cache), for example when
// the guest configures a static address on a network without IPAM. This file resolves VM endpoint
// addresses from the VMI so that loxilb.io/multus-nets services can front virtual machines.
const (
	kubevirtAppLabel      = "kubevirt.io"
	kubevirtLauncherValue = "virt-launcher"
	kubevirtVMNameLabel   = "vm.kubevirt.io/name"
	kubevirtVMIKind       = "VirtualMachineInstance"
	kubevirtVMIPath       = "/apis/kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s"
)

// vmiObject is the subset of kubevirt.io/v1 VirtualMachineInstance this package reads.
type vmiObject struct {
	Spec struct {
		Networks []struct {
			Name   string `json:"name"`
			Multus *struct {
				NetworkName string `json:"networkName"`
			} `json:"multus,omitempty"`
		} `json:"networks"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Interfaces []struct {
			Name        string   `json:"name"`
			IPAddress   string   `json:"ipAddress"`
			IPAddresses []string `json:"ipAddresses"`
			InfoSource  string   `json:"infoSource"`
		} `json:"interfaces"`
	} `json:"status"`
}

// vmiGetRaw fetches a VMI as JSON through the kube REST client. It is a variable so tests can stub it.
var vmiGetRaw = func(ctx context.Context, kubeClient clientset.Interface, ns, name string) ([]byte, error) {
	return kubeClient.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf(kubevirtVMIPath, ns, name)).
		SetHeader("Accept", "application/json").
		DoRaw(ctx)
}

// VirtLauncherVMIName returns the name of the VirtualMachineInstance a pod belongs to, and true when
// the pod is a KubeVirt virt-launcher pod.
func VirtLauncherVMIName(pod *corev1.Pod) (string, bool) {
	if pod == nil || pod.Labels[kubevirtAppLabel] != kubevirtLauncherValue {
		return "", false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == kubevirtVMIKind && owner.Name != "" {
			return owner.Name, true
		}
	}
	if name := pod.Labels[kubevirtVMNameLabel]; name != "" {
		return name, true
	}
	return "", false
}

// VMIEndpointIPs returns the addresses KubeVirt reports for the VMI interface attached to the multus
// network netName (formatted "<namespace>/<net-attach-def>"), filtered by addrType. found is false when
// the VMI does not attach that network at all, in which case callers should fall back to the pod's
// network-status annotation. A VMI that attaches the network but has no address yet (guest still
// booting, guest agent not connected) returns found=true with an empty list.
func VMIEndpointIPs(ctx context.Context, kubeClient clientset.Interface, ns, vmiName, netName, addrType string) ([]string, bool, error) {
	raw, err := vmiGetRaw(ctx, kubeClient, ns, vmiName)
	if err != nil {
		return nil, false, fmt.Errorf("get vmi %s/%s: %w", ns, vmiName, err)
	}
	var vmi vmiObject
	if err := json.Unmarshal(raw, &vmi); err != nil {
		return nil, false, fmt.Errorf("parse vmi %s/%s: %w", ns, vmiName, err)
	}

	ifaceName := ""
	for _, n := range vmi.Spec.Networks {
		if n.Multus == nil {
			continue
		}
		fullName := n.Multus.NetworkName
		if !strings.Contains(fullName, "/") {
			fullName = GetMultusNetworkName(ns, fullName)
		}
		if fullName == netName {
			ifaceName = n.Name
			break
		}
	}
	if ifaceName == "" {
		return nil, false, nil
	}

	if vmi.Status.Phase != "Running" {
		klog.V(4).Infof("vmi %s/%s is %q, no endpoints for %s yet", ns, vmiName, vmi.Status.Phase, netName)
		return nil, true, nil
	}

	var ips []string
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name != ifaceName {
			continue
		}
		candidates := iface.IPAddresses
		if len(candidates) == 0 && iface.IPAddress != "" {
			candidates = []string{iface.IPAddress}
		}
		for _, ip := range candidates {
			if AddrInFamily(addrType, ip) {
				ips = append(ips, ip)
			}
		}
		klog.V(4).Infof("vmi %s/%s interface %s (%s) on %s: %v", ns, vmiName, ifaceName, iface.InfoSource, netName, ips)
	}
	return ips, true, nil
}
