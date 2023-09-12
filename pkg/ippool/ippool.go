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
package ippool

import (
	"errors"
	"net"
	"sync"

	"k8s.io/klog/v2"

	tk "github.com/loxilb-io/loxilib"
)

type IPPool struct {
	CIDR    string
	NetCIDR *net.IPNet
	IPAlloc *tk.IPAllocator
	mutex   sync.Mutex
	Shared  bool
}

// Initailize IP Pool
func NewIPPool(ipa *tk.IPAllocator, CIDR string, Shared bool) (*IPPool, error) {
	ipa.AddIPRange(tk.IPClusterDefault, CIDR)

	_, ipn, err := net.ParseCIDR(CIDR)
	if err != nil {
		return nil, errors.New("CIDR parse failed")
	}

	return &IPPool{
		CIDR:    CIDR,
		NetCIDR: ipn,
		IPAlloc: ipa,
		mutex:   sync.Mutex{},
		Shared:  Shared,
	}, nil
}

// GetNewIPAddr generate new IP and add key(IP) in IP Pool.
// If IP is already in pool, try to generate next IP.
// Returns nil If all IPs in the subnet are already in the pool.
func (i *IPPool) GetNewIPAddr(name string, sIdent uint32, proto string) (net.IP, string) {

	i.mutex.Lock()
	defer i.mutex.Unlock()

	ipamIdent := tk.IPAMNoIdent
	if i.Shared {
		ipamIdent = tk.MakeIPAMIdent("", sIdent, proto)
	}

	newIP, err := i.IPAlloc.AllocateNewIP(tk.IPClusterDefault, i.CIDR, ipamIdent)
	if err != nil {
		if ipamIdent != tk.IPAMNoIdent {
			ipamIdent = tk.IPAMNoIdent
			newIP, err := i.IPAlloc.AllocateNewIP(tk.IPClusterDefault, i.CIDR, ipamIdent)
			if err != nil {
				return nil, ipamIdent
			}
			return newIP, ipamIdent
		}
		klog.Error(err.Error())
		return nil, ipamIdent
	}

	klog.Infof("Allocate ServiceIP %s:%v (%s)", newIP.String(), sIdent, proto)

	return newIP, ipamIdent
}

// ReturnIPAddr return IPaddress in IP Pool
func (i *IPPool) ReturnIPAddr(ip string, identStr string) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	IP := net.ParseIP(ip)
	if IP == nil || !i.NetCIDR.Contains(IP) {
		return
	}

	klog.Infof("Release ServiceIP %s:%v", ip, identStr)

	i.IPAlloc.DeAllocateIP(tk.IPClusterDefault, i.CIDR, identStr, ip)
}

// ReserveIPAddr reserve this IPaddress in IP Pool
func (i *IPPool) ReserveIPAddr(ip string, name string, sIdent uint32, proto string) (error, string) {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	ipamIdent := tk.IPAMNoIdent
	if i.Shared {
		ipamIdent = tk.MakeIPAMIdent("", sIdent, proto)
	}

	klog.V(2).Infof("Reserve ServiceIP %s:%v", ip, sIdent)

	if err := i.IPAlloc.ReserveIP(tk.IPClusterDefault, i.CIDR, ipamIdent, ip); err != nil {
		if ipamIdent != tk.IPAMNoIdent {
			ipamIdent = tk.IPAMNoIdent
			err := i.IPAlloc.ReserveIP(tk.IPClusterDefault, i.CIDR, ipamIdent, ip)
			return err, ipamIdent
		}
	}
	return nil, ipamIdent

}

// CheckAndReserveIP check and reserve this IPaddress in IP Pool
func (i *IPPool) CheckAndReserveIP(ip string, name string, sIdent uint32, proto string) (bool, bool, string) {
	IP := net.ParseIP(ip)
	if IP != nil && i.NetCIDR.Contains(IP) {
		err, idStr := i.ReserveIPAddr(ip, name, sIdent, proto)
		if err != nil {
			return true, false, idStr
		}
		return true, true, idStr
	}

	return false, false, ""
}
