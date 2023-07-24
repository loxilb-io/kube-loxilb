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

package config

type NetworkConfig struct {
	LoxilbURLs              []string
	LoxilbLoadBalancerClass string
	ExternalCIDR            string
	ExternalSecondaryCIDRs  []string
	ExternalCIDR6           string
	ExternalSecondaryCIDRs6 []string
	SetBGP                  uint16
	ExtBGPPeers             []string
	SetLBMode               uint16
	Monitor                 bool
	SetRoles                bool
}
