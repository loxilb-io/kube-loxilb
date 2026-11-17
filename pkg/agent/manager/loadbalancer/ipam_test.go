package loadbalancer

import (
	"strings"
	"testing"

	tk "github.com/loxilb-io/loxilib"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/ippool"
)

func portService(name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: port, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// One /32 shared by every Service, as with --cidrPools=defaultPool=<ip>/32.
// An ingress service holds 8084/tcp for a moment while it gives the listener
// up to an InferencePool. The pool's Service has to wait for it, not take the
// controller down - which used to leave the ingress service's rule orphaned
// in loxilb, since the process died before handling the delete.
func TestGetIngressSvcPairsWaitsForAHeldPort(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	heldIP, heldIdent := pool.GetNewIPAddr("llm/llm-gw-ingress-service", 8084, "tcp")
	if heldIP == nil {
		t.Fatal("could not set up the ingress service's allocation")
	}

	m := &Manager{networkConfig: &config.NetworkConfig{ExternalCIDRPoolDefs: []string{"defaultPool=192.168.0.13/32"}}, lbCache: LbCacheTable{
		"llm/llm-gw-ingress-service": {LbServicePairs: map[string]*LbServicePairEntry{
			"pair": {ExternalIP: heldIP.String(), Port: 8084, Protocol: "tcp"},
		}},
	}}
	svc := portService("agg-lab-inference", 8084)
	entry := &LbCacheEntry{IPPool: pool, LbServicePairs: map[string]*LbServicePairEntry{}}

	_, err, _ = m.getIngressSvcPairs(svc, entry)
	if !errors.Is(errors.Wrap(err, "getIngressSvcPairs return error"), ErrExternalIPUnavailable) {
		t.Fatalf("err = %v, want ErrExternalIPUnavailable through the caller's wrap", err)
	}
	if !strings.Contains(err.Error(), "llm/llm-gw-ingress-service") {
		t.Errorf("the error does not name the Service holding the port: %v", err)
	}

	// Another port on the same address is free all along.
	if _, err, _ := m.getIngressSvcPairs(portService("other", 8080), &LbCacheEntry{IPPool: pool, LbServicePairs: map[string]*LbServicePairEntry{}}); err != nil {
		t.Errorf("a free port on the shared address failed: %v", err)
	}

	// The ingress service lets go; the retry gets the port.
	pool.ReturnIPAddr(heldIP.String(), heldIdent)
	delete(m.lbCache, "llm/llm-gw-ingress-service")

	pairs, err, _ := m.getIngressSvcPairs(svc, entry)
	if err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	if len(pairs) != 1 || pairs[0].IPString != "192.168.0.13" || pairs[0].Port != 8084 || !pairs[0].IPAllocd {
		t.Errorf("pairs = %+v, want 192.168.0.13:8084 allocated by this call", pairs)
	}
}

// With nobody else on the port, the message says the pool is out of
// addresses rather than naming a holder.
func TestExternalIPUnavailableWithoutHolder(t *testing.T) {
	pool, err := ippool.NewIPPool(tk.IpAllocatorNew(), "192.168.0.13/32", true)
	if err != nil {
		t.Fatalf("NewIPPool: %v", err)
	}
	err = (&Manager{lbCache: LbCacheTable{}}).externalIPUnavailable(pool, "llm/svc", 8084, "tcp")
	if !errors.Is(err, ErrExternalIPUnavailable) || !strings.Contains(err.Error(), "no address left") {
		t.Errorf("err = %v", err)
	}
}
