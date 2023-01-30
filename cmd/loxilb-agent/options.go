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

package main

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"strings"
	"net"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v2"
)

var (
	loxiURLFlag = ""
)

type Options struct {
	// The path of configuration file.
	configFile string
	// The configuration object
	config *AgentConfig
}

func newOptions() *Options {
	return &Options{
		config: &AgentConfig{},
	}
}

// addFlags adds flags to fs and binds them to options.
func (o *Options) addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.configFile, "config", o.configFile, "The path to the configuration file")
	fs.StringVar(&loxiURLFlag, "loxiURL", loxiURLFlag, "loxilb API server URL(s)")
	fs.StringVar(&o.config.ExternalCIDR, "externalCIDR", o.config.ExternalCIDR, "External CIDR Range")
	fs.StringVar(&o.config.LoxilbLoadBalancerClass, "loxilbLoadBalancerClass", o.config.LoxilbLoadBalancerClass, "Load-Balancer Class Name")
	fs.BoolVar(&o.config.SetBGP, "setBGP", o.config.SetBGP, "Use BGP routing")
	fs.BoolVar(&o.config.ExclIPAM, "setUniqueIP", o.config.ExclIPAM, "Use unique IPAM per service")
	fs.Uint16Var(&o.config.SetLBMode, "setLBMode", o.config.SetLBMode, "LB mode to use")
}

// complete completes all the required options
func (o *Options) complete(args []string) error {
	if len(o.configFile) > 0 {
		if err := o.loadConfigFromFile(); err != nil {
			return err
		}
	}
	o.updateConfigFromCommandLine()
	o.setDefaults()
	return nil
}

// validate validates all the required options. It must be called after complete.
func (o *Options) validate(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("no positional arguments are supported")
	}

	if len(o.config.LoxiURLs) > 0 {
		for _, loxiURL := range o.config.LoxiURLs {
			if _, err := url.Parse(loxiURL); err != nil {
				return fmt.Errorf("loxiURL %s is invalid. err: %v", loxiURL, err)
			}
		}
	}

	if o.config.ExternalCIDR != "" {
		if _, _, err := net.ParseCIDR(o.config.ExternalCIDR); err != nil {
			return fmt.Errorf("externalCIDR %s config is invalid", o.config.ExternalCIDR)
		}
	}

	if o.config.LoxilbLoadBalancerClass != "" {
		if ok := strings.Contains(o.config.LoxilbLoadBalancerClass, "/"); !ok {
			return fmt.Errorf("loxilbLoadBalancerClass must be a label-style identifier")
		}
	}

	return nil
}

func (o *Options) loadConfigFromFile() error {
	data, err := ioutil.ReadFile(o.configFile)
	if err != nil {
		return err
	}

	if err := yaml.UnmarshalStrict(data, &o.config); err != nil {
		return err
	}

	return nil
}

func (o *Options) updateConfigFromCommandLine() {
	if loxiURLFlag != "" {
		o.config.LoxiURLs = strings.Split(loxiURLFlag, ",")
	}
}

const (
	defaultHostProcPathPrefix    = "/host"
	defaultLoxiURL               = "http://127.0.0.1:11111"
	defaultnodePortServiceVirtIP = "11.187.0.1/32"
)

func (o *Options) setDefaults() {
	o.config.WithNamespace = true
	o.config.ExclIPAM = false

	if o.config.HostProcPathPrefix == "" {
		o.config.HostProcPathPrefix = defaultHostProcPathPrefix
	}
	if o.config.LoxiURLs == nil {
		o.config.LoxiURLs = []string{defaultLoxiURL}
	}
	if o.config.NodePortServiceVirtIP == "" {
		o.config.NodePortServiceVirtIP = defaultnodePortServiceVirtIP
	}
	if o.config.LoxilbLoadBalancerClass == "" {
		o.config.LoxilbLoadBalancerClass = "loxilb.io/loxilb"
	}
	if o.config.ExternalCIDR == "" {
		o.config.ExternalCIDR = "123.123.123.1/24"
	}
}
