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

package api

import (
	"context"
	"net/http"
)

// FwRuleOpts - Information related to Firewall options
type FwOptArg struct {
	// Drop - Drop any matching rule
	Drop bool `json:"drop" yaml:"drop"`
	// Trap - Trap anything matching rule
	Trap bool `json:"trap" yaml:"trap"`
	// Redirect - Redirect any matching rule
	Rdr     bool   `json:"redirect" yaml:"redirect"`
	RdrPort string `json:"redirectPortName" yaml:"redirectPortName"`
	// Allow - Allow any matching rule
	Allow bool   `json:"allow" yaml:"allow"`
	Mark  uint32 `json:"fwMark" yaml:"fwMark"`
	// Record - Record packets matching rule
	Record bool `json:"record" yaml:"record"`
	// DoSNAT - Do snat on matching rule
	DoSnat bool   `json:"doSnat"`
	ToIP   string `json:"toIP"`
	ToPort uint16 `json:"toPort"`
	// OnDefault - Trigger only on default cases
	OnDefault bool `json:"onDefault"`
	// Counter - Traffic counter
	Counter string `json:"counter"`
}

// FwRuleArg - Information related to firewall rule
type FwRuleArg struct {
	// SrcIP - Source IP in CIDR notation
	SrcIP string `json:"sourceIP" yaml:"sourceIP" options:"sourceIP"`
	// DstIP - Destination IP in CIDR notation
	DstIP string `json:"destinationIP" yaml:"destinationIP"`
	// SrcPortMin - Minimum source port range
	SrcPortMin uint16 `json:"minSourcePort" yaml:"minSourcePort"`
	// SrcPortMax - Maximum source port range
	SrcPortMax uint16 `json:"maxSourcePort" yaml:"maxSourcePort"`
	// DstPortMin - Minimum destination port range
	DstPortMin uint16 `json:"minDestinationPort" yaml:"minDestinationPort"`
	// SrcPortMax - Maximum source port range
	DstPortMax uint16 `json:"maxDestinationPort" yaml:"maxDestinationPort"`
	// Proto - the protocol
	Proto uint8 `json:"protocol" yaml:"protocol"`
	// InPort - the incoming port
	InPort string `json:"portName" yaml:"portName"`
	// Pref - User preference for ordering
	Pref uint16 `json:"preference" yaml:"preference"`
}

func (fwRule *FwRuleArg) GetKeyStruct() LoxiModel {
	return fwRule
}

// FwRuleMod - Info related to a firewall entry
type FwRuleMod struct {
	// Serv - service argument of type FwRuleArg
	Rule FwRuleArg `json:"ruleArguments" yaml:"ruleArguments"`
	// Opts - firewall options
	Opts FwOptArg `json:"opts" yaml:"opts"`
}

func (firewallModel *FwRuleMod) GetKeyStruct() LoxiModel {
	return &firewallModel.Rule
}

type FirewallAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newFirewallAPI(r *RESTClient) *FirewallAPI {
	return &FirewallAPI{
		resource: "config/firewall",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (f *FirewallAPI) GetModel() LoxiModel {
	return &FwRuleMod{}
}

func (f *FirewallAPI) Create(ctx context.Context, fwModel LoxiModel) error {
	resp := f.client.POST(f.resource).Body(fwModel).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (f *FirewallAPI) Get(ctx context.Context, name string) error {
	fwModel := f.GetModel()

	resp := f.client.GET(f.resource).SubResource(name).Do(ctx).UnMarshal(fwModel)
	if resp.err != nil {
		return resp.err
	}
	return nil
}

func (f *FirewallAPI) Delete(ctx context.Context, fwModel LoxiModel) error {
	queryParam, err := f.MakeQueryParam(fwModel)
	if err != nil {
		return err
	}

	resp := f.client.DELETE(f.resource).Query(queryParam).Do(ctx)
	if resp.statusCode != http.StatusOK {
		if resp.err != nil {
			return resp.err
		}
	}

	return nil
}
