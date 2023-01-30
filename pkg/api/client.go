package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type LoxiClient struct {
	restClient *RESTClient
}

// apiServer is string. what format? http://10.0.0.1 or 10.0.0.1
func NewLoxiClient(apiServer string) (*LoxiClient, error) {
	fmt.Println("NewLoxiClient:")
	client := &http.Client{}

	base, err := url.Parse(apiServer)
	if err != nil {
		fmt.Printf("failed to parse url %s. err: %s", apiServer, err.Error())
		return nil, err
	}

	restClient, err := NewRESTClient(base, "netlox", "v1", client)
	if err != nil {
		fmt.Printf("failed to call NewRESTClient. err: %s", err.Error())
		return nil, err
	}

	return &LoxiClient{
		restClient: restClient,
	}, nil
}

func (l *LoxiClient) SetLoxiHealthCheckChan(stop <-chan struct{}, aliveCh chan *LoxiClient) {
	isLoxiAlive := true

	go wait.Until(func() {
		if _, err := l.HealthCheck().Get(context.Background(), ""); err != nil {
			if isLoxiAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb(%s) is down. isLoxiAlive is changed to 'false'", l.restClient.baseURL.String())
				isLoxiAlive = false
			}
		} else {
			if !isLoxiAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb(%s) is alive again. isLoxiAlive is set 'true'", l.restClient.baseURL.String())
				isLoxiAlive = true
				aliveCh <- l
			}
		}
	}, time.Second*2, stop)
}

func (l *LoxiClient) LoadBalancer() *LoadBalancerAPI {
	return newLoadBalancerAPI(l.GetRESTClient())
}

func (l *LoxiClient) HealthCheck() *HealthCheckAPI {
	return newHealthCheckAPI(l.GetRESTClient())
}

func (l *LoxiClient) GetRESTClient() *RESTClient {
	if l == nil {
		return nil
	}

	return l.restClient
}
