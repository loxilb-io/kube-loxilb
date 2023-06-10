package cmd

import (
	"context"
	"fmt"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
	"github.com/spf13/cobra"
)

func GetCmd(client *api.LoxiClient) *cobra.Command {
	var getCmd = &cobra.Command{
		Use:   "get",
		Short: "A brief description of your command",
		Long: `A longer description that spans multiple lines and likely contains examples
	and usage of using your command. For example:
	
	Cobra is a CLI library for Go that empowers applications.
	This application is a tool to generate the needed files
	to quickly Get a Cobra application.`,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd
			_ = args
			fmt.Println("Get called")

			lbModel, err := client.LoadBalancer().List(context.TODO())
			if err != nil {
				fmt.Printf("error: %s\n", err.Error())
			}

			fmt.Printf("get LB: %v\n", lbModel)
		},
	}

	return getCmd
}

func CreateCmd(client *api.LoxiClient) *cobra.Command {
	var createCmd = &cobra.Command{
		Use:   "create",
		Short: "A brief description of your command",
		Long: `A longer description that spans multiple lines and likely contains examples
	and usage of using your command. For example:
	
	Cobra is a CLI library for Go that empowers applications.
	This application is a tool to generate the needed files
	to quickly Get a Cobra application.`,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd
			_ = args
			fmt.Println("Create called")

			body := &api.LoadBalancerModel{
				Service: api.LoadBalancerService{
					ExternalIP: "123.123.123.1",
					Port:       11111,
					Protocol:   "tcp",
				},
			}
			endpoint := api.LoadBalancerEndpoint{
				EndpointIP: "1.1.1.1",
				TargetPort: 12345,
				Weight:     10,
			}
			body.Endpoints = append(body.Endpoints, endpoint)

			err := client.LoadBalancer().Create(context.TODO(), body)
			if err != nil {
				fmt.Printf("error: %s\n", err.Error())
			}
		},
	}

	return createCmd
}

func DeleteCmd(client *api.LoxiClient) *cobra.Command {
	var deleteCmd = &cobra.Command{
		Use:   "delete",
		Short: "A brief description of your command",
		Long: `A longer description that spans multiple lines and likely contains examples
	and usage of using your command. For example:
	
	Cobra is a CLI library for Go that empowers applications.
	This application is a tool to generate the needed files
	to quickly Get a Cobra application.`,
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd
			_ = args
			fmt.Println("Delete called")

			body := &api.LoadBalancerModel{
				Service: api.LoadBalancerService{
					ExternalIP: "123.123.123.1",
					Port:       11111,
					Protocol:   "tcp",
					BGP:        true,
				},
			}

			err := client.LoadBalancer().Delete(context.TODO(), body)
			if err != nil {
				fmt.Printf("error: %s\n", err.Error())
			}
		},
	}

	return deleteCmd
}
