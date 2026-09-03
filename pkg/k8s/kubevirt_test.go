package k8s

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// vmiJSON mirrors what KubeVirt v1.9 reports for a VM with a masquerade default interface and a
// bridge-bound secondary interface on the multus network "default/secnet".
const vmiJSON = `{
  "spec": {"networks": [
    {"name": "default", "pod": {}},
    {"name": "secnet", "multus": {"networkName": "secnet"}}
  ]},
  "status": {"phase": "Running", "interfaces": [
    {"name": "default", "ipAddress": "10.244.1.7", "ipAddresses": ["10.244.1.7"], "infoSource": "domain, guest-agent"},
    {"name": "secnet", "ipAddress": "123.123.123.204", "ipAddresses": ["123.123.123.204", "fe80::78af:b0ff:fe07:3eb7"], "infoSource": "domain, guest-agent, multus-status"}
  ]}
}`

func stubVMI(t *testing.T, body string, err error) {
	t.Helper()
	orig := vmiGetRaw
	vmiGetRaw = func(context.Context, clientset.Interface, string, string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(body), nil
	}
	t.Cleanup(func() { vmiGetRaw = orig })
}

func TestVirtLauncherVMIName(t *testing.T) {
	launcher := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels:          map[string]string{"kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-web"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "VirtualMachineInstance", Name: "vm-web"}},
	}}
	if name, ok := VirtLauncherVMIName(launcher); !ok || name != "vm-web" {
		t.Fatalf("launcher pod: got (%q,%v), want (vm-web,true)", name, ok)
	}
	labelOnly := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-x"}}}
	if name, ok := VirtLauncherVMIName(labelOnly); !ok || name != "vm-x" {
		t.Fatalf("label-only pod: got (%q,%v), want (vm-x,true)", name, ok)
	}
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nginx"}}}
	if _, ok := VirtLauncherVMIName(plain); ok {
		t.Fatalf("plain pod must not be treated as a virt-launcher")
	}
}

func TestVMIEndpointIPs(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		err       error
		netName   string
		addrType  string
		wantIPs   []string
		wantFound bool
		wantErr   bool
	}{
		{name: "ipv4 on attached network", body: vmiJSON, netName: "default/secnet", addrType: "ipv4", wantIPs: []string{"123.123.123.204"}, wantFound: true},
		{name: "ipv6 filter", body: vmiJSON, netName: "default/secnet", addrType: "ipv6", wantIPs: []string{"fe80::78af:b0ff:fe07:3eb7"}, wantFound: true},
		{name: "network not attached to the vmi", body: vmiJSON, netName: "default/other", addrType: "ipv4", wantFound: false},
		{name: "vmi not running yet", body: `{"spec":{"networks":[{"name":"secnet","multus":{"networkName":"secnet"}}]},"status":{"phase":"Scheduling"}}`, netName: "default/secnet", addrType: "ipv4", wantFound: true},
		{name: "attached but no address yet", body: `{"spec":{"networks":[{"name":"secnet","multus":{"networkName":"secnet"}}]},"status":{"phase":"Running","interfaces":[{"name":"secnet","infoSource":"domain"}]}}`, netName: "default/secnet", addrType: "ipv4", wantFound: true},
		{name: "namespaced networkName in the vmi", body: `{"spec":{"networks":[{"name":"n1","multus":{"networkName":"default/secnet"}}]},"status":{"phase":"Running","interfaces":[{"name":"n1","ipAddress":"123.123.123.10"}]}}`, netName: "default/secnet", addrType: "ipv4", wantIPs: []string{"123.123.123.10"}, wantFound: true},
		{name: "api error (e.g. missing rbac)", err: errors.New("forbidden"), netName: "default/secnet", addrType: "ipv4", wantErr: true},
		{name: "malformed body", body: "{", netName: "default/secnet", addrType: "ipv4", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubVMI(t, tt.body, tt.err)
			ips, found, err := VMIEndpointIPs(context.Background(), fake.NewSimpleClientset(), "default", "vm-web", tt.netName, tt.addrType)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !reflect.DeepEqual(ips, tt.wantIPs) {
				t.Fatalf("ips = %v, want %v", ips, tt.wantIPs)
			}
		})
	}
}

func multusPod(name string, phase corev1.PodPhase, labels map[string]string, networks, status string, owners ...metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: labels, OwnerReferences: owners,
			Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": networks, "k8s.v1.cni.cncf.io/network-status": status},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestGetMultusEndpointsKubeVirt(t *testing.T) {
	stubVMI(t, vmiJSON, nil)
	sel := map[string]string{"web": "yes"}
	vmiOwner := metav1.OwnerReference{Kind: "VirtualMachineInstance", Name: "vm-web"}
	launcherLabels := map[string]string{"web": "yes", "kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-web"}
	launcherNets := `[{"name":"secnet","namespace":"default","interface":"pod0dccfe60da3"}]`
	// The launcher pod's annotation carries the IPAM-assigned address; the guest really uses .204 (VMI status).
	launcherStatus := `[{"name":"kindnet","interface":"eth0","ips":["10.244.1.7"],"default":true},{"name":"default/secnet","interface":"pod0dccfe60da3","ips":["123.123.123.195"]}]`
	plainStatus := `[{"name":"kindnet","interface":"eth0","ips":["10.244.2.9"],"default":true},{"name":"default/secnet","interface":"net1","ips":["123.123.123.193"]}]`

	client := fake.NewSimpleClientset(
		multusPod("virt-launcher-vm-web-new", corev1.PodRunning, launcherLabels, launcherNets, launcherStatus, vmiOwner),
		// source pod of a finished live migration: still matches the selector, must be ignored
		multusPod("virt-launcher-vm-web-old", corev1.PodSucceeded, launcherLabels, launcherNets, `[{"name":"default/secnet","interface":"pod0dccfe60da3","ips":["123.123.123.194"]}]`, vmiOwner),
		// ordinary pod on the same network keeps using its annotation
		multusPod("pod-web", corev1.PodRunning, sel, "secnet", plainStatus),
		// pod on an unrelated network
		multusPod("pod-other", corev1.PodRunning, sel, "othernet", `[{"name":"default/othernet","interface":"net1","ips":["10.10.10.10"]}]`),
	)

	got, err := GetMultusEndpoints(client, "default", "web=yes", []string{"secnet"}, "ipv4")
	if err != nil {
		t.Fatalf("GetMultusEndpoints: %v", err)
	}
	sort.Strings(got)
	want := []string{"123.123.123.193", "123.123.123.204"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
}

func TestGetMultusEndpointsKubeVirtFallback(t *testing.T) {
	// Without permission to read VMIs the launcher pod's annotation is used, as before.
	stubVMI(t, "", errors.New("virtualmachineinstances.kubevirt.io is forbidden"))
	launcherLabels := map[string]string{"web": "yes", "kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-web"}
	client := fake.NewSimpleClientset(
		multusPod("virt-launcher-vm-web-x", corev1.PodRunning, launcherLabels, "secnet", `[{"name":"default/secnet","interface":"net1","ips":["123.123.123.195"]}]`),
	)
	got, err := GetMultusEndpoints(client, "default", "web=yes", []string{"secnet"}, "ipv4")
	if err != nil {
		t.Fatalf("GetMultusEndpoints: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"123.123.123.195"}) {
		t.Fatalf("endpoints = %v, want [123.123.123.195]", got)
	}
}

func TestGetMultusEndpointsDedupe(t *testing.T) {
	stubVMI(t, vmiJSON, nil)
	launcherLabels := map[string]string{"web": "yes", "kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-web"}
	// During a live migration both launcher pods are Running and resolve to the same VMI address.
	client := fake.NewSimpleClientset(
		multusPod("virt-launcher-vm-web-src", corev1.PodRunning, launcherLabels, "secnet", `[{"name":"default/secnet","interface":"net1","ips":["123.123.123.194"]}]`),
		multusPod("virt-launcher-vm-web-dst", corev1.PodRunning, launcherLabels, "secnet", `[{"name":"default/secnet","interface":"net1","ips":["123.123.123.195"]}]`),
	)
	got, err := GetMultusEndpoints(client, "default", "web=yes", []string{"secnet"}, "ipv4")
	if err != nil {
		t.Fatalf("GetMultusEndpoints: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"123.123.123.204"}) {
		t.Fatalf("endpoints = %v, want [123.123.123.204]", got)
	}
}
