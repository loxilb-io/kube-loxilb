package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type LoxiClient struct {
	RestClient  *RESTClient
	MasterLB    bool
	PeeringOnly bool
	Url         string
	Host        string
	Port        string
	IsAlive     bool
	Stop        chan struct{}
}

// apiServer is string. what format? http://10.0.0.1 or 10.0.0.1
func NewLoxiClient(apiServer string, aliveCh chan *LoxiClient, peerOnly bool) (*LoxiClient, error) {
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

	host, port, err := net.SplitHostPort(base.Host)
	if err != nil {
		fmt.Printf("failed to parse host,port %s. err: %s", base.Host, err.Error())
		return nil, err
	}

	stop := make(chan struct{})

	lc := &LoxiClient{
		RestClient:  restClient,
		Url:         apiServer,
		Host:        host,
		Port:        port,
		Stop:        stop,
		PeeringOnly: peerOnly,
	}

	lc.StartLoxiHealthCheckChan(aliveCh)

	return lc, nil
}

func (l *LoxiClient) StartLoxiHealthCheckChan(aliveCh chan *LoxiClient) {
	l.IsAlive = false

	go wait.Until(func() {
		if _, err := l.HealthCheck().Get(context.Background(), ""); err != nil {
			if l.IsAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb(%s) is down", l.RestClient.baseURL.String())
				l.IsAlive = false
			}
		} else {
			if !l.IsAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb(%s) is alive", l.RestClient.baseURL.String())
				l.IsAlive = true
				aliveCh <- l
			}
		}
	}, time.Second*2, l.Stop)
}

func (l *LoxiClient) StopLoxiHealthCheckChan() {
	l.Stop <- struct{}{}
}

func (l *LoxiClient) LoadBalancer() *LoadBalancerAPI {
	return newLoadBalancerAPI(l.GetRESTClient())
}

func (l *LoxiClient) CIStatus() *CiStatusAPI {
	return newCiStatusAPI(l.GetRESTClient())
}

func (l *LoxiClient) BGP() *BGPAPI {
	return newBGPAPI(l.GetRESTClient())
}

func (l *LoxiClient) HealthCheck() *HealthCheckAPI {
	return newHealthCheckAPI(l.GetRESTClient())
}

func (l *LoxiClient) GetRESTClient() *RESTClient {
	if l == nil {
		return nil
	}

	return l.RestClient
}
