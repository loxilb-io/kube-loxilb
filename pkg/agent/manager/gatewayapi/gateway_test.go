package gatewayapi

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
)

func portsOf(ports []corev1.ServicePort) []string {
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s:%d->%s/%s", p.Name, p.Port, p.TargetPort.String(), p.Protocol))
	}
	return out
}

// Listeners that share a port share one port of the ingress service. A port
// for each listener was refused by the API server as a duplicate, and with it
// the whole Service: no listener of the Gateway was served.
func TestIngressServicePorts(t *testing.T) {
	gatewayWith := func(listeners ...v1.Listener) *v1.Gateway {
		return &v1.Gateway{Spec: v1.GatewaySpec{Listeners: listeners}}
	}

	cases := []struct {
		name    string
		gateway *v1.Gateway
		claimed map[v1.SectionName]bool
		want    []string
	}{
		{
			name:    "the Run:ai Gateway, three HTTPS listeners on 443",
			gateway: runaiGateway(),
			want:    []string{"https-443:443->443/TCP"},
		},
		{
			name: "hosts on HTTP and HTTPS",
			gateway: gatewayWith(
				listener("a-http", v1.HTTPProtocolType, 80, "a.example.com"),
				listener("b-http", v1.HTTPProtocolType, 80, "b.example.com"),
				listener("a-https", v1.HTTPSProtocolType, 443, "a.example.com"),
				listener("b-https", v1.HTTPSProtocolType, 443, "b.example.com")),
			want: []string{"http-80:80->80/TCP", "https-443:443->443/TCP"},
		},
		{
			name: "a port other than the standard one",
			gateway: gatewayWith(
				listener("alt", v1.HTTPSProtocolType, 8443, "")),
			want: []string{"https-8443:8443->443/TCP"},
		},
		{
			name: "HTTPS on a port HTTP has first",
			gateway: gatewayWith(
				listener("http", v1.HTTPProtocolType, 8080, ""),
				listener("https", v1.HTTPSProtocolType, 8080, "")),
			want: []string{"http-8080:8080->80/TCP"},
		},
		{
			name: "L4 listeners are not the ingress service's",
			gateway: gatewayWith(
				listener("tcp", v1.TCPProtocolType, 5432, ""),
				listener("udp", v1.UDPProtocolType, 53, ""),
				listener("http", v1.HTTPProtocolType, 80, "")),
			want: []string{"http-80:80->80/TCP"},
		},
		{
			name: "a port an InferencePool serves for one of its listeners",
			gateway: gatewayWith(
				listener("models", v1.HTTPProtocolType, 80, "models.example.com"),
				listener("web", v1.HTTPProtocolType, 80, "web.example.com"),
				listener("secure", v1.HTTPSProtocolType, 443, "web.example.com")),
			claimed: map[v1.SectionName]bool{"models": true},
			want:    []string{"https-443:443->443/TCP"},
		},
		{
			name: "a listener name no port name could hold",
			gateway: gatewayWith(
				listener("http.a-name-of-more-than-sixty-three-characters-with-dots-in-it.example", v1.HTTPProtocolType, 80, "")),
			want: []string{"http-80:80->80/TCP"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := portsOf(ingressServicePorts(tc.gateway, tc.claimed))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("ports = %v, want %v", got, tc.want)
			}
		})
	}
}
