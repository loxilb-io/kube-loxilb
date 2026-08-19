![build workflow](https://github.com/loxilb-io/kube-loxilb/actions/workflows/docker-publish.yml/badge.svg)  [![Build](https://github.com/loxilb-io/kube-loxilb/actions/workflows/build-check.yml/badge.svg)](https://github.com/loxilb-io/kube-loxilb/actions/workflows/build-check.yml)   

## What is kube-loxilb ?

[kube-loxilb](https://github.com/loxilb-io/kube-loxilb) is loxilb's implementation of kubernetes service load-balancer spec which includes support for load-balancer class, advanced IPAM (shared or exclusive) etc. kube-loxilb runs as a deloyment set in kube-system namespace. This component runs inside k8s cluster to gather information about k8s nodes/reachability/LB services etc but in itself does not implement packet/session load-balancing. It is done by [loxilb](https://github.com/loxilb-io/loxilb) which usually runs outside the cluster as an external-LB. 

Many users frequently ask us whether it is possible to run the actual packet/session load-balancing inside the cluster (in worker-nodes or master-nodes). The answer is "yes". loxilb can be run in-cluster or as an external entity. The preferred way is to run <b>kube-loxilb</b> component inside the cluster and provision <b>loxilb</b> docker in any external node/vm as mentioned in this guide. The rationale is to provide users a similar look and feel whether running loxilb in an on-prem or public cloud environment. Public-cloud environments usually run load-balancers/firewalls externally in order to provide a seamless and safe environment for the cloud-native workloads. But users are free to choose any mode (in-cluster mode or external mode) as per convenience and their system architecture. The following blogs give detailed steps for :

1. [Running loxilb in external node with AWS EKS](https://www.loxilb.io/post/loxilb-load-balancer-setup-on-eks)
2. [Running in-cluster LB with K3s for on-prem use-cases](https://www.loxilb.io/post/k8s-nuances-of-in-cluster-external-service-lb-with-loxilb)

This usually leads to another query - Who will be responsible for managing the external node ? On public cloud(s), it is as simple as spawning a new instance in your VPC and launch loxilb docker in it. For on-prem cases, you need to run loxilb docker in a spare node/vm as applicable. loxilb docker is a self-contained entity and easily managed with well-known tools like docker, containerd, podman etc. It can be independently restarted/upgraded anytime and kube-loxilb will make sure all the k8s LB services are properly configured each time. When deploying in-cluster mode, everything is managed by Kubernetes itself with little to no manual intervention.   

## How is kube-loxilb different from loxi-ccm ?

Another loxilb component known as [loxi-ccm](https://github.com/loxilb-io/loxi-ccm) also provides implementation of kubernetes load-balancer spec but it runs as a part of cloud-provider and provides load-balancer life-cycle management as part of it. If one needs to integrate loxilb with their existing cloud-provider implementation, they can use or include loxi-ccm as a part of it. Else, kube-loxilb is the right component to use for all scenarios. It also has the latest loxilb features integrated as development is currently focused on it.   

kube-loxilb is a standalone implementation of kubernetes load-balancer spec which does not depend on cloud-provider. It runs as a kube-system deployment and provisions load-balancer rules in loxilb based on load-balancer class. It only acts on load-balancers services for the LB classes that is provided by itself. This along with loxilb's modularized architecture also allows us to have different load-balancers working together in the same K8s environment. In future, loxi-ccm and kube-loxilb will share the same code base but currently they are maintained separately.   

## Overall topology   

* For external mode, the overall topology including all components should be similar to the following :

![kube-loxilb-Ext-Cluster-LB](https://github.com/loxilb-io/kube-loxilb/assets/75648333/4b760173-c61d-42f2-b72b-95e0ae017db6)

* For in-cluster mode, the overall topology including all components should be similar to the following :

![kube-loxilb-int](https://github.com/loxilb-io/kube-loxilb/assets/75648333/76ffa47f-9c7f-4a44-bc5d-0ba2f41b2fb3)

## How to deploy kube-loxilb ?

* If you have chosen external-mode, please make sure loxilb docker is downloaded and installed properly in a node external to your cluster. One can follow guides [here](https://loxilb-io.github.io/loxilbdocs/run/) or refer to various other [documentation](https://loxilb-io.github.io/loxilbdocs/#how-to-guides) . It is important to have network connectivity from this node to the k8s cluster nodes (where kube-loxilb will eventually run) as seen in the above figure. (PS - This step can be skipped if running in-cluster mode)   

* Download the kube-loxilb config yaml :

```
wget https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/ext-cluster/kube-loxilb.yaml
```

* Modify arguments as per user's needs :

```
        args:
            - --loxiURL=http://12.12.12.1:11111
            - --cidrPools=defaultPool=123.123.123.1/24
            #- --cidrPools=defaultPool=123.123.123.1/24,pool2=124.124.124.1/24
            #- --cidr6Pools=defaultPool=3ffe::1/96
            #- --monitor
            #- --setBGP=65100
            #- --extBGPPeers=50.50.50.1:65101,51.51.51.1:65102
            #- --enableBGPCRDs
            #- --setRoles=0.0.0.0
            #- --setLBMode=1
            #- --setUniqueIP=false
```
        
The arguments have the following meaning :     

| Name | Description |
| ----------- | ----------- |
| loxiURL | API server address of loxilb. This is the docker IP address loxilb docker of Step 1. If unspecified, kube-loxilb assumes loxilb is running in-cluster mode and autoconfigures this. |
| cidrPools | CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode). Multiple pools can be defined and any of them can be selected during service creation using "loxilb.io/poolSelect" annotation. cidrPools can be specified as ip-cidr e.g 123.123.123.0/24 or as ip-ranges e.g 192.168.20.100-192.168.20.105 |     
| cidr6Pools | Ipv6 CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode). Multiple pools can be defined and any of them can be selected during service creation using "loxilb.io/poolSelect" annotation. |
| monitor | Enable liveness probe for the LB end-points (default : unset) | 
| setBGP | Use specified BGP AS-ID to advertise this service. If not specified BGP will be disabled. Please check [here](https://github.com/loxilb-io/loxilbdocs/blob/main/docs/integrate_bgp_eng.md) how it works. | 
| extBGPPeers | Specifies external BGP peers with appropriate remote AS | 
| setRoles | If present, kube-loxilb arbitrates loxilb role(s) in cluster-mode. Further, it sets a special VIP (selected as sourceIP) to communicate with end-points in full-nat mode. | 
| setLBMode | 0, 1, 2 <br> 0 - default (only DNAT, preserves source-IP) <br> 1 - onearm (source IP is changed to load balancer’s interface IP) <br> 2 - fullNAT (sourceIP is changed to virtual IP) | 
| setUniqueIP | Allocate unique service-IP per LB service (default : false) | 
| externalSecondaryCIDRs | Secondary CIDR or IPAddress ranges to allocate addresses from in case of multi-homing support |   
| enableBGPCRDs | Enable BGP Policy and Peer CRDs |   

Many of the above flags and arguments can be overriden on a per-service basis based on loxilb specific annotation as mentioned below.   

* kube-loxilb supported annotations:   
  
| Annotations | Description |
| ----------- | ----------- |
| <b>loxilb.io/multus-nets</b> | When using multus, the multus network can also be used as a service endpoint.Register the multus network name to be used. If you need to specify a Multus network located in a different namespace from the Service, register it in the `<namespace>/<network-name>` format.   <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: multus-service<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/multus-nets: macvlan1,macvlan2<br>spec:<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;app: pod-01<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;- port: 55002<br>&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 5002<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/poolSelect</b> | You can specify the IP Pool name from which external IP address will be allocated and assigned to the service. If this annotation is not present, it defaults to "defaultPool"<br><br><b>Example:</b><br>metadata:<br>&nbsp;&nbsp;name: sctp-lb1<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/poolSelect: “pool1”<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 55002<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/poolSelectSecondary</b> | When using the SCTP multi-homing function, you can specify the number of secondary IP Pools(upto 3) from which secondary IP address can be assigned to the service. When used with the loxilb.io/secondaryIPs annotation, the value set in loxilb.io/poolSelectSecondary is ignored. (loxilb.io/secondaryIPs annotation takes precedence)<br><br><b>Example:</b><br>metadata:<br>&nbsp;&nbsp;name: sctp-lb1<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/poolSelectSecondary: “pool2,pool3”<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 55002<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/secondaryIPs</b> | When using the SCTP multi-homing function, specify the secondary IP to be assigned to the service. Multiple IPs(upto 3) can be specified at the same time using a comma(,). When used with the loxilb.io/poolSelectSecondary annotation, loxilb.io/secondaryIPs takes priority.)<br><br><b>Example:</b><br>metadata:<br>name: sctp-lb-secips<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/lbmode: "fullnat"<br>loxilb.io/secondaryIPs: "1.1.1.1,2.2.2.2"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb-secips<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;type: LoadBalancer|
| <b> loxilb.io/staticIP</b> | Specifies the External IP to assign to the LoadBalancer service. By default, an external IP is assigned within the externalCIDR range set in kube-loxilb, but using the annotation, IPs outside the range can also be statically specified. <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb-fullnat<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/lbmode: "fullnat"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/staticIP: "192.168.255.254"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-fullnat-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer|
| <b>loxilb.io/liveness</b> | Set LoxiLB to perform a health check (probe) based endpoint selection(If flag is set, only active endpoints will be selected). The default value is no, and when the value is set to yes, the probe function of the corresponding service is activated.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb-fullnat<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-fullnat-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer|
| <b>loxilb.io/lbmode</b> | Set LB mode individually for each service. The values ​​that can be specified: “default”, “onearm”, “fullnat” and "dsr". Please refer to [this](https://loxilb-io.github.io/loxilbdocs/nat/) document for more details.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb-fullnat<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/lbmode: "fullnat"<br>&nbsp;&nbsp;&nbsp;&nbsp;spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-fullnat-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/ipam</b> | Specify which IPAM mode the service will use. Select one of three options: “ipv4”, “ipv6”, or “ipv6to4”. <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/ipam : "ipv4"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/timeout</b> | Set the session retention time for the service. <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/timeout : "60"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/probetype</b> | Specifies the protocol type to use for endpoint probe operations. You can select one of “udp”, “tcp”, “https”, “http”, “sctp”, “ping”, or “none”. Probetype is set to protocol type, if you are using lbMode as "fullnat" or "onearm". To set it off, use probetype : "none" <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetype : "ping"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/probeport</b> | Set the port to use for probe operation. It is not applied if the loxilb.io/probetype annotation is not used or if it is of type icmp or none.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetype : "tcp"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probeport : "3000"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer  |
| <b>loxilb.io/probereq</b> | Specifies API for the probe request. It is not applied if the loxilb.io/probetype annotation is not used or if it is of type icmp or none.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetype : "tcp"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probeport : "3000"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probereq : "health"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/proberesp</b> | Specifies the response to the probe request. It is not applied if the loxilb.io/probetype annotation is not used or if it is of type icmp or none.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetype : "tcp"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probeport : "3000"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probereq : "health"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/proberesp : "ok"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/probetimeout</b> | Specifies the timeout for starting a probe request (in seconds). The default value is 60 seconds <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetimeout : "10"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/proberetries</b> | Specifies the number of probe request retries before considering an endpoint as inoperative. The default value is 2 <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetimeout : "10"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/proberetries : "3"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/epselect</b> | Specifies the algorithm for end-point slection e.g "rr", "hash", "persist", "lc" etc. The default value is roundrobin. The values "chwbl", "gpuaware" and "wrr-hash" additionally require <b>loxilb.io/lbmode: "fullproxy"</b> and a loxilb-inference-gateway backend - see [Inference gateway annotations](#inference-gateway-annotations). <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetimeout : "10"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/proberetries : "3"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/epselect : "hash"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/usepodnetwork</b> | Whether to select PodIP and targetPort as EndPoints <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetimeout : "10"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/proberetries : "3"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/usepodnetwork : "yes"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |
| <b>loxilb.io/useproxyprotov2</b> | Whether to enable proxy protocol v2 <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: tcp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/lbmode : "fullnat"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/useproxyprotov2 : "yes"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: tcp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 80<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: TCP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 8080<br>&nbsp;&nbsp;type: LoadBalancer   |

* Endpoint health-monitor annotations:

These configure the gateway's per-endpoint health monitor directly, instead of through the `probereq` / `proberesp` escape hatch. The values apply uniformly to every endpoint of the service, which is the normal case for a health check.

All five exist only in loxilb-inference-gateway. Against a plain upstream loxilb the probe path is <b>carried across to `probereq`</b>, which upstream formats into the probe URL, so the path is still honoured; the other four have no upstream equivalent and are dropped, with a `ProbeFieldsDowngraded` event naming which. `expectedCodes` is deliberately not folded into `proberesp` -- that is a body-substring match rather than a status check, and translating one into the other would change what is being tested.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/probe-method</b> | HTTP method for the probe. Default `GET`. |
| <b>loxilb.io/probe-path</b> | Probe path, e.g. `"/healthz"`. Must start with `/`. <b>Wins over `loxilb.io/probereq`</b>, which stays available as the escape hatch. |
| <b>loxilb.io/probe-expected-codes</b> | Accepted status codes: `"200"`, a list `"200,202"`, or a range `"200-204"`. Default `"200"`. Replaces the `loxilb.io/proberesp` substring match when set. |
| <b>loxilb.io/probe-http-version</b> | `"1.0"` or `"1.1"`. At `"1.1"` a Host header is sent. |
| <b>loxilb.io/probe-domain</b> | TLS SNI for HTTPS monitors, and the Host header at HTTP/1.1. |

<b>These replace the legacy probe, they do not refine it.</b> loxilb picks one probe mode rather than merging the two configurations, and any one of the five annotations switches it over. In the new mode the check is a status-code match against `probe-expected-codes` (default `"200"`) and <b>`loxilb.io/proberesp` is no longer consulted at all</b>. So a service running `proberesp: "OK"` that adds `probe-domain` purely to fix SNI loses the body check. kube-loxilb raises a `ProbeModeChanged` warning event when both are configured, naming what will actually be checked. One asymmetry: `probe-http-version: "1.0"` on its own does not switch the mode -- only `"1.1"` does.

<b>Setting `probe-domain` turns on HTTP/1.1 for you.</b> The domain is two things at once: it is always the TLS SNI, but it only becomes the Host header at HTTP/1.1. Someone configuring virtual-host health checks would otherwise get SNI and no Host header, with nothing to indicate it, so `probe-http-version` defaults to `"1.1"` whenever `probe-domain` is set. Setting it explicitly to `"1.0"` still wins.

Example:

```yaml
metadata:
  annotations:
    loxilb.io/liveness: "yes"
    loxilb.io/probetype: "https"
    loxilb.io/probe-method: "GET"
    loxilb.io/probe-path: "/healthz"
    loxilb.io/probe-expected-codes: "200-204"
    loxilb.io/probe-domain: "api.example.com"
```

* Gateway-only service annotations:

Per-service limits, member timeouts, and TLS/HSTS policy. All twelve exist only in loxilb-inference-gateway. A plain upstream peer in the pool simply does not get them -- the rule is still programmed there without them, with a `GatewayArgsDowngraded` event naming which. They are additive hardening on a rule that works without them, so refusing would be worse than reporting.

kube-loxilb checks that the annotation parses and nothing more. Whether a cipher string is understood, a TLS version supported, or a certificate id present in loxilb's registry is not knowable from the request, so the value goes through and loxilb's own refusal comes back as a `LoxiLBRejected` event on the Service.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/connection-limit</b> | Ceiling on simultaneous connections across all endpoints of the service, enforced in eBPF. 0 or unset means unlimited. |
| <b>loxilb.io/timeout-member-connect</b> | Seconds to wait for a backend connection. |
| <b>loxilb.io/timeout-member-data</b> | Seconds a backend connection may stay idle. |
| <b>loxilb.io/timeout-tcp-inspect</b> | Seconds allowed for TCP inspection. |
| <b>loxilb.io/tls-ciphers</b> | OpenSSL cipher string, colon-separated -- one string, not a list. |
| <b>loxilb.io/tls-versions</b> | Comma-separated, e.g. `"TLSv1.2,TLSv1.3"`. loxilb collapses it to a min/max range. |
| <b>loxilb.io/alpn-protocols</b> | Comma-separated, e.g. `"h2,http/1.1"`. Advertised on both listener and pool. |
| <b>loxilb.io/hsts-max-age</b> | HSTS `max-age` in seconds. |
| <b>loxilb.io/hsts-include-subdomains</b> | Add `includeSubDomains`. `"true"`/`"yes"`. |
| <b>loxilb.io/hsts-preload</b> | Add `preload`. `"true"`/`"yes"`. |
| <b>loxilb.io/backend-ca-cert-id</b> | Names a CA certificate <b>already registered in loxilb</b>. Distinct from the Secret-mounted `loxilb.io/mtls-backend-ca-secret`: separate mechanism, separate dataplane slot, and the two do not collide. |
| <b>loxilb.io/backend-client-cert-id</b> | The same for the client certificate. |

Example:

```yaml
metadata:
  annotations:
    loxilb.io/connection-limit: "5000"
    loxilb.io/timeout-member-data: "50"
    loxilb.io/tls-versions: "TLSv1.2,TLSv1.3"
    loxilb.io/alpn-protocols: "h2,http/1.1"
    loxilb.io/hsts-max-age: "31536000"
    loxilb.io/hsts-include-subdomains: "true"
```

<a name="inference-gateway-annotations"></a>
* Inference gateway annotations:

These are served by [loxilb-inference-gateway](https://github.com/loxilb-io/loxilb-inference-gateway), a superset of upstream loxilb aimed at LLM serving fleets. kube-loxilb detects the flavor at runtime from the `product` field of `GET /netlox/v1/version`, so the same kube-loxilb drives both. A service that asks for any of the annotations below while its loxilb is plain upstream loxilb is rejected with a Warning event on the Service rather than being silently downgraded.

All of these require <b>loxilb.io/lbmode: "fullproxy"</b> in practice; the selection and KV-exact paths run in the userspace proxy, which only fullproxy traffic reaches. Omitting an annotation leaves the field out of the request entirely, so loxilb applies its own default.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/model-name</b> | Route by the `model` field of the request body. Several services can share one VIP:port, each claiming a model name; a service with no model name is the catch-all. |
| <b>loxilb.io/sse-mode</b> | Server-sent-events awareness: suppresses the idle timeout during token streaming. Also arms AI key and rate-limit enforcement. `"true"`/`"yes"`. |
| <b>loxilb.io/max-stream-duration</b> | Ceiling in seconds for a single streamed response. Default 0 (unbounded). |
| <b>loxilb.io/backend-keepalive-interval</b> | Backend keepalive interval in seconds. Default 0. |
| <b>loxilb.io/session-header-name</b> | Pin a session to one endpoint by an HTTP header, e.g. `"mcp-session-id"` for an MCP gateway. |
| <b>loxilb.io/trace-type</b> | Protocol-aware tracing, e.g. `"mcp"`. |
| <b>loxilb.io/cb-enable</b> | Per-endpoint circuit breaker. `"true"`/`"yes"`. |
| <b>loxilb.io/chwbl-prefix-hash-level</b> | Prefix-hash depth for consistent hashing with bounded loads: `1`, `2` or `3`. Default 1. Used by `epselect: chwbl` and `epselect: wrr-hash`. |
| <b>loxilb.io/chwbl-prefix-hash-flags</b> | Prefix-hash flags, 0..255. Default 0. |
| <b>loxilb.io/chwbl-mean-load-factor</b> | Bounded-load factor as a percentage, 100..300. Default 125. Lower values spread load more evenly at the cost of cache locality. |
| <b>loxilb.io/chwbl-replication</b> | Virtual nodes per endpoint on the hash ring, 1..1024. Default 100. |
| <b>loxilb.io/chwbl-enable-cache-salt</b> | Salt the cache key. `"true"`/`"yes"`. Default false. |
| <b>loxilb.io/kv-exact-mode</b> | KV-cache exact routing. Use `"3"` for a single role-less serving pool, or `"1"` alongside prefill/decode disaggregation (see below). |
| <b>loxilb.io/kv-engine-type</b> | `"vllm"` (default) or `"sglang"`. Immutable once the rule exists: changing it needs a delete and recreate. |
| <b>loxilb.io/kv-dp-rank-count</b> | SGLang `--dp-size`, 1..8. Default 1. |
| <b>loxilb.io/kv-block-size</b> | KV block size in tokens. Default 16. Must match the engine. |
| <b>loxilb.io/kv-zmq-port</b> | Engine event socket port. Default 5557. |
| <b>loxilb.io/kv-warmup-sec</b> | Warmup window in seconds. The gateway's swagger documents a default of 30 but no code applies it, so set this explicitly if warmup matters. |
| <b>loxilb.io/kv-hash-algo</b> | `"sha256_cbor"`, `"xxhash_cbor"` or `"sha256_sglang"`. Best omitted: loxilb then derives it from the engine type and the pair can never be incoherent. |

* Prefill/decode disaggregation:

Disaggregation needs a role per endpoint, which a Service cannot state directly. kube-loxilb derives it: two label selectors name the prefill and the decode pods, and each endpoint is stamped with the role of the pod it belongs to.

Because the role is per pod, the endpoints have to <b>be</b> pods. Set <b>loxilb.io/usepodnetwork: "yes"</b> (or use a multus network). In the default mode the endpoints are node addresses, every pod on a node collapses into one entry, and the split cannot be represented - kube-loxilb rejects that combination rather than programming a rule that cannot work.

| Annotation | Description |
| ---------- | ----------- |
| <b>loxilb.io/pd-disagg</b> | Turn on prefill/decode disaggregation. `"true"`/`"yes"`. Requires fullproxy, pod endpoints, and both selectors below. |
| <b>loxilb.io/pd-prefill-selector</b> | Label selector for the prefill pods, e.g. `"llm-role=prefill"`. |
| <b>loxilb.io/pd-decode-selector</b> | Label selector for the decode pods. A pod may not match both. |
| <b>loxilb.io/pd-prefill-nixl-port</b> | NIXL side-channel port the prefill pods listen on, matching their `VLLM_NIXL_SIDE_CHANNEL_PORT`. 0 or unset reuses the target port. |
| <b>loxilb.io/pd-decode-nixl-port</b> | The same for the decode pods. |
| <b>loxilb.io/pd-cache-aware</b> | Cache-aware prefill placement. Requires `pd-disagg`. |
| <b>loxilb.io/pd-cache-threshold</b> | Cache-hit percentage above which the cached prefill endpoint is preferred, 0..100. Default 20. |
| <b>loxilb.io/pd-session-ttl</b> | Session lifetime in seconds. Default 0. |
| <b>loxilb.io/pd-balance-abs-threshold</b> | Absolute load gap before rebalancing. Default 3. |

With disaggregation on, <b>loxilb.io/kv-exact-mode: "1"</b> becomes available; mode `3` is for a single role-less pool and loxilb rejects it here.

The port is uniform per pool, not per pod: all prefill pods are assumed to share one NIXL port and all decode pods another, which is how one Deployment per role deploys. Per-pod ports would need a pod annotation and are not supported.

Example - a prefill pool and a decode pool behind one Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: vllm-pd
  annotations:
    loxilb.io/lbmode: "fullproxy"
    loxilb.io/usepodnetwork: "yes"
    loxilb.io/pd-disagg: "true"
    loxilb.io/pd-prefill-selector: "llm-role=prefill"
    loxilb.io/pd-decode-selector: "llm-role=decode"
    loxilb.io/pd-prefill-nixl-port: "9001"
    loxilb.io/pd-decode-nixl-port: "9002"
    loxilb.io/kv-exact-mode: "1"
    loxilb.io/sse-mode: "true"
spec:
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app: vllm          # selects both pools
  ports:
    - port: 8000
      targetPort: 8000
      protocol: TCP
  type: LoadBalancer
```

The Service selector must cover both pools, since one rule carries both. The two role selectors then partition what it found.

<b>GPU-aware routing has to be armed outside Kubernetes.</b> `loxilb.io/epselect: "gpuaware"` sets the rule's selector, but whether that selector actually runs is decided by a process-global routing mode on each loxilb instance, and it is off by default. A rule created against a loxilb with it off is accepted and then routes as plain CHWBL, indistinguishable from `epselect: "chwbl"`.

Two things have to happen on the loxilb side, and neither is something kube-loxilb can do:

1. `POST /netlox/v1/config/gpu/enable` on each instance, which arms the routing mode.
2. Per-endpoint GPU telemetry pushed to `POST /netlox/v1/config/worker/metrics`. This comes from the serving engine or DCGM; without it the selector is armed but has no data.

kube-loxilb does not drive either: the first is one instance-wide switch with no reference counting, and the second needs metrics the Kubernetes API does not have. What it does do is check. Before programming a `gpuaware` rule it reads `GET /netlox/v1/config/gpu/status`, and if the mode is disarmed it refuses the rule on that instance with a `GPUMonitoringDisabled` warning event rather than letting it silently become CHWBL. The same check repeats on every reconcile of a `gpuaware` service, so disarming the mode after the rule exists is reported too. If the status cannot be read at all, the rule is programmed anyway -- a failed diagnostic should not take down a rule that would have worked.

<b>KV-exact routing needs a staged tokenizer.</b> loxilb reads `/etc/loxilb/tokenizers/<model-slug>/tokenizer.json`, where `<model-slug>` is the model name with each `/` replaced by `__`. kube-loxilb does not manage that file and cannot see it, so whenever a rule enables `kv-exact-mode` it records a Normal `KvExactTokenizerRequired` event on the Service naming the exact path to check -- visible with `kubectl describe svc`.

If the file is missing, loxilb logs `kv-router: tokenizer not available` once and silently falls back to load-based routing: the rule is still created and traffic still flows, just without cache-aware placement. <b>loxilb caches that failure</b>, so staging the tokenizer afterwards does not take effect until loxilb restarts. Stage it before creating the rule.

Example - prefix-cache aware routing across a vLLM pool:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: vllm-llama70b
  annotations:
    loxilb.io/lbmode: "fullproxy"
    loxilb.io/epselect: "chwbl"
    loxilb.io/chwbl-prefix-hash-level: "2"
    loxilb.io/chwbl-mean-load-factor: "125"
    loxilb.io/sse-mode: "true"
    loxilb.io/model-name: "meta-llama/Llama-3.1-70B-Instruct"
spec:
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app: vllm
  ports:
    - port: 8000
      targetPort: 8000
      protocol: TCP
  type: LoadBalancer
```

* Apply the yaml after making necessary changes :

```
kubectl apply -f kube-loxilb.yaml
```
* The above should make sure kube-loxilb is successfully running. Check kube-loxilb is running :   

```
k8s@master:~$ sudo kubectl get pods -A
NAMESPACE         NAME                                        READY   STATUS    RESTARTS   AGE
kube-system       local-path-provisioner-84db5d44d9-pczhz     1/1     Running   0          16h
kube-system       coredns-6799fbcd5-44qpx                     1/1     Running   0          16h
kube-system       metrics-server-67c658944b-t4x5d             1/1     Running   0          16h
kube-system       kube-loxilb-5fb5566999-ll4gs                1/1     Running   0          14h
```

* Finally to create service LB for a workload, we can use and apply the following template yaml   
   
(<b>Note</b> -  Check <b>*loadBalancerClass*</b> and other <b>*loxilb*</b> specific annotation) :

```
        apiVersion: v1
        kind: Service
        metadata:
          name: iperf-service
          annotations:
           # If there is a need to do liveness check from loxilb
           loxilb.io/liveness: "yes"
           # Specify LB mode - one of default, onearm or fullnat 
           loxilb.io/lbmode: "default"
           # Specify loxilb IPAM mode - one of ipv4, ipv6 or ipv6to4 
           loxilb.io/ipam: "ipv4"
           # Specify cidr pool for allocating externalIP
           # If not specified defaults to "defaultPool"
           # loxilb.io/poolSelect: "pool1"
           # Specify cidr pool of secondary networks for multi-homing
           # Only valid for SCTP currently
           # loxilb.io/poolSelectSecondary: "pool2,pool3"
           # Specify a static externalIP for this service
           # loxilb.io/staticIP: "123.123.123.2"
        spec:
          loadBalancerClass: loxilb.io/loxilb
          selector:
            what: perf-test
          ports:
            - port: 55001
              targetPort: 5001
          type: LoadBalancer
        ---
        apiVersion: v1
        kind: Pod
        metadata:
          name: iperf1
          labels:
            what: perf-test
        spec:
          containers:
            - name: iperf
              image: eyes852/ubuntu-iperf-test:0.5
              command:
                - iperf
                - "-s"
              ports:
                - containerPort: 5001
```   

Users can change the above as per their needs.

* Verify LB service is created   

```
k8s@master:~$ sudo kubectl get svc
NAME                TYPE           CLUSTER-IP    EXTERNAL-IP         PORT(S)             AGE
kubernetes          ClusterIP      10.43.0.1     <none>              443/TCP             13h
iperf1              LoadBalancer   10.43.8.156   llb-192.168.80.20   55001:5001/TCP      8m20s
```   

* For more example yaml templates, kindly refer to kube-loxilb's manifest [directory](https://github.com/loxilb-io/kube-loxilb/tree/main/manifest)           

## Additional steps to deploy loxilb (in-cluster) mode

To run loxilb in-cluster mode, the URL argument in [kube-loxilb.yaml](https://github.com/loxilb-io/kube-loxilb/blob/main/manifest/in-cluster/kube-loxilb.yaml) needs to be commented out:   


```
        args:
            #- --loxiURL=http://12.12.12.1:11111
            - --cidrPools=defaultPool=123.123.123.1/24
```   

This enables a self-discovery mode of kube-loxilb where it can find and reach loxilb pods running inside the cluster. Last but not the least we need to create the loxilb pods in cluster :   

```
sudo kubectl apply -f https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/in-cluster/loxilb.yaml
```   

Once all the pods are created, the same can be verified as follows (you can see both kube-loxilb and loxilb components running:   

```
k8s@master:~$ sudo kubectl get pods -A
NAMESPACE         NAME                                        READY   STATUS    RESTARTS   AGE
kube-system       local-path-provisioner-84db5d44d9-pczhz     1/1     Running   0          16h
kube-system       coredns-6799fbcd5-44qpx                     1/1     Running   0          16h
kube-system       metrics-server-67c658944b-t4x5d             1/1     Running   0          16h
kube-system       kube-loxilb-5fb5566999-ll4gs                1/1     Running   0          14h
kube-system       loxilb-lb-mklj2                             1/1     Running   0          13h
kube-system       loxilb-lb-stp5k                             1/1     Running   0          13h
kube-system       loxilb-lb-j8fc6                             1/1     Running   0          13h
kube-system       loxilb-lb-5m85p                             1/1     Running   0          13h
```    

Thereafter, the process of service creation remains the same as explained in previous sections.   

## How to use kube-loxilb CRDs ?   

Kube-loxilb provides various Custom Resource Definition (CRD) to facilicate its operations:

1. Generic config CRDS. More info [here](https://github.com/loxilb-io/loxilbdocs/blob/main/docs/kube-loxilb-url-crds.md)
2. BGP config CRDS. More info [here](https://github.com/loxilb-io/loxilbdocs/blob/main/docs/k8s_bgp_policy_crd.md)
