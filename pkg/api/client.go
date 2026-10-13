package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
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
	CIDefaultZone = "llb"
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

	// flavor - what the loxilb instance behind this client reported in
	// GET /netlox/v1/version. nil means "not yet detected"; a non-nil value
	// with an empty product means "detected, and it is plain upstream loxilb".
	//
	// Written by the health-check goroutine and read by the load-balancer
	// fan-out goroutines, hence atomic rather than a plain field. Product and
	// features come from one answer, so they are swapped together.
	flavor atomic.Pointer[peerFlavor]
	// flavorRefreshPending - the version query that should follow the peer
	// coming up has not succeeded yet. Only the health-check goroutine
	// touches it.
	flavorRefreshPending bool
}

// flavorQueryTimeout - the version query gets its own budget rather than what
// is left of the health check's.
const flavorQueryTimeout = 2 * time.Second

// peerFlavor - one version answer. Never mutated after it is stored.
type peerFlavor struct {
	product  string
	features []string
}

// Product - detected product identifier, or "" when the peer is plain upstream
// loxilb or has not answered a version query yet.
func (l *LoxiClient) Product() string {
	if f := l.flavor.Load(); f != nil {
		return f.product
	}
	return ""
}

// FlavorDetected - whether a version query has successfully completed.
func (l *LoxiClient) FlavorDetected() bool {
	return l.flavor.Load() != nil
}

// SupportsFeature - whether the peer listed name in the `features` of its
// version answer. An undetected peer, and any peer that predates the field,
// supports nothing.
func (l *LoxiClient) SupportsFeature(name string) bool {
	if f := l.flavor.Load(); f != nil {
		return slices.Contains(f.features, name)
	}
	return false
}

// IsInferenceGateway - whether this peer is a loxilb-inference-gateway.
//
// An undetected peer reports false: absence of the `product` field is the
// documented signal for plain upstream loxilb, so falling back to false keeps
// gateway-only payload fields off the wire when in doubt.
func (l *LoxiClient) IsInferenceGateway() bool {
	return l.Product() == ProductInferenceGateway
}

// DetectFlavor - query GET /netlox/v1/version once and cache the product
// field. Subsequent calls are no-ops, so it is safe to call on every health
// tick. A failed query leaves the flavor undetected so a later call retries.
func (l *LoxiClient) DetectFlavor(ctx context.Context) {
	if l.FlavorDetected() {
		return
	}

	l.RefreshFlavor(ctx)
}

// RefreshFlavor - query GET /netlox/v1/version again even if an answer is
// cached, and replace it on success.
//
// Called when a peer comes back up: the binary behind it may have been
// replaced while it was down, typically by an upgrade that adds a feature. A
// failed query keeps the previous answer rather than forgetting it. The
// fan-out still installs rules on a peer whose health check is failing, and a
// forgotten flavor would make that peer read as plain loxilb, refusing every
// gateway rule with a warning that blames the Service for the peer's outage.
//
// Reports whether the query succeeded.
func (l *LoxiClient) RefreshFlavor(ctx context.Context) bool {
	versionModel, err := l.Version().Get(ctx, "")
	if err != nil {
		klog.V(4).Infof("loxilb-lb(%s): version query failed, keeping the previous answer: %v", l.Host, err)
		return false
	}

	l.flavor.Store(&peerFlavor{
		product:  versionModel.Product,
		features: slices.Clone(versionModel.Features),
	})

	if versionModel.Product == ProductInferenceGateway {
		klog.Infof("loxilb-lb(%s): detected loxilb-inference-gateway (version %s, build %s, features %v)",
			l.Host, versionModel.Version, versionModel.BuildInfo, versionModel.Features)
	} else {
		klog.Infof("loxilb-lb(%s): detected plain loxilb (version %s, build %s)",
			l.Host, versionModel.Version, versionModel.BuildInfo)
	}

	return true
}

// flavorEqual - whether two version answers would make any install decision
// differently.
func flavorEqual(a, b *peerFlavor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.product == b.product && slices.Equal(a.features, b.features)
}

// onHealthy - the flavor bookkeeping of one successful health check. Returns
// whether the peer has to be announced on the alive channel, which re-syncs
// every rule to it.
//
// Flavor is a property of the peer binary, so it only has to be asked again
// when the binary may have changed: when the peer comes back after being seen
// down. It is asked before the announcement, because the re-sync has to see
// the new answer. A query that fails then is retried on every later tick, and
// if the answer it eventually brings differs from the one the re-sync used,
// the peer is announced again so the rules are re-sent under it.
func (l *LoxiClient) onHealthy() bool {
	refresh := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), flavorQueryTimeout)
		defer cancel()
		return l.RefreshFlavor(ctx)
	}

	if !l.IsAlive {
		l.flavorRefreshPending = !refresh()
		klog.Infof("LoxiHealthCheckChan: loxilb-lb(%s) is alive", l.Host)
		l.IsAlive = true
		return true
	}

	if !l.flavorRefreshPending {
		return false
	}

	before := l.flavor.Load()
	if !refresh() {
		return false
	}
	l.flavorRefreshPending = false

	return !flavorEqual(before, l.flavor.Load())
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

	LoadTokenForHost(restClient.baseURL.Host)

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
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*1)
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
		} else if l.onHealthy() {
			aliveCh <- l
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

func (l *LoxiClient) K8sMeta() *K8sMetaAPI {
	return newK8sMetaAPI(l.GetRESTClient())
}

func (l *LoxiClient) GPU() *GPUAPI {
	return newGPUAPI(l.GetRESTClient())
}

func (l *LoxiClient) Version() *VersionAPI {
	return newVersionAPI(l.GetRESTClient())
}

func (l *LoxiClient) SniCert() *SniCertAPI {
	return newSniCertAPI(l.GetRESTClient())
}

func (l *LoxiClient) GetRESTClient() *RESTClient {
	if l == nil {
		return nil
	}

	return l.RestClient
}
