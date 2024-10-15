package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	DeadSigTs   time.Time
	DoBGPCfg    bool
	Purge       bool
	Stop        chan struct{}
	NoRole      bool
	Name        string
}

// apiServer is string. what format? http://10.0.0.1 or 10.0.0.1
func NewLoxiClient(apiServer string, aliveCh chan *LoxiClient, deadCh chan struct{}, peerOnly bool, noRole bool, name string) (*LoxiClient, error) {

	base, err := url.Parse(apiServer)
	if err != nil {
		fmt.Printf("failed to parse url %s. err: %s", apiServer, err.Error())
		return nil, err
	}

	client, err := CreateHTTPClient(base)
	if err != nil {
		fmt.Printf("failed to create HTTP client: %v", err.Error())
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

	if name == "" {
		name = host
	}

	stop := make(chan struct{})

	lc := &LoxiClient{
		RestClient:  restClient,
		Url:         apiServer,
		Host:        host,
		Port:        port,
		Stop:        stop,
		PeeringOnly: peerOnly,
		DeadSigTs:   time.Now(),
		NoRole:      noRole,
		Name:        name,
	}

	lc.StartLoxiHealthCheckChan(aliveCh, deadCh)

	klog.Infof("NewLoxiClient Created: %s", apiServer)

	return lc, nil
}

func CreateHTTPClient(baseURL *url.URL) (*http.Client, error) {

	client := &http.Client{}

	if baseURL.Scheme == "https" {

		rootCAs, err := x509.SystemCertPool()
		if err != nil || rootCAs == nil {
			baseURL.Scheme = "http"
			klog.Infof("HTTPS not supported: %s", baseURL)
			return client, nil
		}

		tlsConfig := &tls.Config{
			RootCAs:            rootCAs,
			InsecureSkipVerify: false,
		}

		transport := &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		client.Transport = transport
	}

	return client, nil
}

func (l *LoxiClient) StartLoxiHealthCheckChan(aliveCh chan *LoxiClient, deadCh chan struct{}) {
	l.IsAlive = false

	go wait.Until(func() {
		if _, err := l.HealthCheck().Get(context.Background(), ""); err != nil {
			if l.IsAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb-lb(%s) is down", l.Host)
				l.IsAlive = false
				if time.Duration(time.Since(l.DeadSigTs).Seconds()) >= 3 && l.MasterLB {
					klog.Infof("LoxiHealthCheckChan: loxilb-lb(%s) master down", l.Host)
					l.DeadSigTs = time.Now()
					deadCh <- struct{}{}
				} else {
					l.DeadSigTs = time.Now()
				}
			}
		} else {
			if !l.IsAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb-lb(%s) is alive", l.Host)
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

func (l *LoxiClient) BGPPolicyDefinedSetsAPI() *BGPPolicyDefinedSetsAPI {
	return newBGPPolicyDefinedSetsAPI(l.GetRESTClient())
}

func (l *LoxiClient) BGPPolicyDefinition() *BGPPolicyDefinitionAPI {
	return newBGPPolicyDefinition(l.GetRESTClient())
}

func (l *LoxiClient) BGPPolicyApply() *BGPPolicyApplyAPI {
	return newBGPPolicyApplyAPI(l.GetRESTClient())
}

func (l *LoxiClient) GetRESTClient() *RESTClient {
	if l == nil {
		return nil
	}

	return l.RestClient
}
