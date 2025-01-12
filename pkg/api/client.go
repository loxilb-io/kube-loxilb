package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	tk "github.com/loxilb-io/loxilib"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

type LoxiZoneInst struct {
	MasterLB bool
}

type LoxiClientPool struct {
	Clients []*LoxiClient
}

func (l *LoxiClientPool) AddLoxiClient(newLoxiClient *LoxiClient) {
	l.Clients = append(l.Clients, newLoxiClient)
}

func NewLoxiClientPool() *LoxiClientPool {
	return &LoxiClientPool{
		Clients: make([]*LoxiClient, 0),
	}
}

const (
	CIDefault = "llb-inst0"
)

type LoxiClient struct {
	RestClient  *RESTClient
	InstRoles   map[string]*LoxiZoneInst
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

// GenZoneInstName generate zone instance name
func GenZoneInstName(zone string, id int) string {
	return fmt.Sprintf("%s-%s%d", zone, "inst", id)
}

// apiServer is string. what format? http://10.0.0.1 or 10.0.0.1
func NewLoxiClient(apiServer string, aliveCh chan *LoxiClient, deadCh chan struct{}, peerOnly bool, noRole bool, name string, zone string, numZoneInst int) (*LoxiClient, error) {

	base, err := url.Parse(apiServer)
	if err != nil {
		klog.Errorf("failed to parse url %s. err: %s", apiServer, err.Error())
		return nil, err
	}

	client, err := CreateHTTPClient(base)
	if err != nil {
		klog.Errorf("failed to create HTTP client: %v", err.Error())
		return nil, err
	}

	restClient, err := NewRESTClient(base, "netlox", "v1", client)
	if err != nil {
		klog.Errorf("failed to call NewRESTClient. err: %s", err.Error())
		return nil, err
	}

	hostPort := strings.Split(base.Host, ":")
	if len(hostPort) > 1 {
		shostPort := hostPort[:len(hostPort)-1]
		host := strings.Join(shostPort[:], ":")
		if tk.IsNetIPv6(host) {
			host = fmt.Sprintf("[%s]", host)
		}
		base.Host = fmt.Sprintf("%s:%s", host, hostPort[len(hostPort)-1])
	}

	host, port, err := net.SplitHostPort(base.Host)
	if err != nil {
		klog.Errorf("failed to parse host,port %s. err: %s", base.Host, err.Error())
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

	lc.InstRoles = make(map[string]*LoxiZoneInst)

	for i := 0; i < numZoneInst; i++ {
		instName := GenZoneInstName(zone, i)
		lc.InstRoles[instName] = &LoxiZoneInst{}
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

func (l *LoxiClient) LoxiClientHasMaterInst() bool {
	for _, val := range l.InstRoles {
		if val.MasterLB {
			return true
		}
	}
	return false
}

func (l *LoxiClient) StartLoxiHealthCheckChan(aliveCh chan *LoxiClient, deadCh chan struct{}) {
	l.IsAlive = false

	go wait.Until(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		if _, err := l.HealthCheck().Get(ctx, ""); err != nil {
			if l.IsAlive {
				klog.Infof("LoxiHealthCheckChan: loxilb-lb(%s) is down", l.Host)
				l.IsAlive = false
				if time.Duration(time.Since(l.DeadSigTs).Seconds()) >= 3 && l.LoxiClientHasMaterInst() {
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
		cancel()
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

func (l *LoxiClient) Firewall() *FirewallAPI {
	return newFirewallAPI(l.GetRESTClient())
}

func (l *LoxiClient) GetRESTClient() *RESTClient {
	if l == nil {
		return nil
	}

	return l.RestClient
}
