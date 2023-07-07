/*
 * Copyright (c) 2023 NetLOX Inc
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
	"flag"
	"os"

	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"github.com/spf13/cobra"

	"github.com/loxilb-io/kube-loxilb/cmd"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/loxilb-io/kube-loxilb/pkg/log"
)

func Execute() {
	var rootCmd = &cobra.Command{
		Use:   "kube-loxilb",
		Short: "loxilb-k8s",
		Long:  "loxilb-k8s",
	}

	client, err := api.NewLoxiClient("http://127.0.0.1:11111")
	if err != nil {
		return
	}

	rootCmd.AddCommand(cmd.GetCmd(client))
	rootCmd.AddCommand(cmd.CreateCmd(client))
	rootCmd.AddCommand(cmd.DeleteCmd(client))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func main() {

	logs.InitLogs()
	defer logs.FlushLogs()

	command := newAgentCommand()
	if err := command.Execute(); err != nil {
		logs.FlushLogs()
		os.Exit(1)
	}

	//Execute()
}

func newAgentCommand() *cobra.Command {
	opts := newOptions()

	cmd := &cobra.Command{
		Use:  "loxilb-agent",
		Long: "The loxilb agent runs on each node.",
		Run: func(cmd *cobra.Command, args []string) {
			log.InitLogFileLimits(cmd.Flags())
			log.InitLogLevel()
			if err := opts.complete(args); err != nil {
				klog.Errorf("Failed to options complete. err: %v", err)
				os.Exit(255)
			}
			if err := opts.validate(args); err != nil {
				klog.Errorf("Failed to options validate. err: %v", err)
				os.Exit(255)

			}
			if err := run(opts); err != nil {
				klog.Errorf("Error running agent. err: %v", err)
				os.Exit(255)
			}
		},
		Version: "beta",
	}

	flags := cmd.Flags()
	opts.addFlags(flags)
	log.AddFlags(flags)
	flags.AddGoFlagSet(flag.CommandLine)

	return cmd
}
