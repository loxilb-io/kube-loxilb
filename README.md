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
            - --externalCIDR=123.123.123.1/24
            #- --externalSecondaryCIDRs=124.124.124.1/24,125.125.125.1/24
            #- --externalCIDR6=3ffe::1/96
            #- --monitor
            #- --setBGP=65100
            #- --extBGPPeers=50.50.50.1:65101,51.51.51.1:65102
            #- --setRoles=0.0.0.0
            #- --setLBMode=1
            #- --setUniqueIP=false
```
        
The arguments have the following meaning :     

| Name | Description |
| ----------- | ----------- |
| loxiURL | API server address of loxilb. This is the docker IP address loxilb docker of Step 1. If unspecified, kube-loxilb assumes loxilb is running in-cluster mode and autoconfigures this. |
| externalCIDR | CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode) |     
|externalCIDR6 | Ipv6 CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode) |
| monitor | Enable liveness probe for the LB end-points (default : unset) | 
| setBGP | Use specified BGP AS-ID to advertise this service. If not specified BGP will be disabled. Please check [here](https://github.com/loxilb-io/loxilbdocs/blob/main/docs/integrate_bgp_eng.md) how it works. | 
| extBGPPeers | Specifies external BGP peers with appropriate remote AS | 
| setRoles | If present, kube-loxilb arbitrates loxilb role(s) in cluster-mode. Further, it sets a special VIP (selected as sourceIP) to communicate with end-points in full-nat mode. | 
| setLBMode | 0, 1, 2 <br> 0 - default (only DNAT, preserves source-IP) <br> 1 - onearm (source IP is changed to load balancer’s interface IP) <br> 2 - fullNAT (sourceIP is changed to virtual IP) | 
| setUniqueIP | Allocate unique service-IP per LB service (default : false) | 
| externalSecondaryCIDRs | Secondary CIDR or IPAddress ranges to allocate addresses from in case of multi-homing support |   

Many of the above flags and arguments can be overriden on a per-service basis based on loxilb specific annotation as mentioned below.   

* kube-loxilb supported annotations:   
  
| Annotations | Description |
| ----------- | ----------- |
| <b>loxilb.io/multus-nets</b> | When using multus, the multus network can also be used as a service endpoint.Register the multus network name to be used.<br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: multus-service<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/multus-nets: macvlan1,macvlan2<br>spec:<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;app: pod-01<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;- port: 55002<br>&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 5002<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/num-secondary-networks</b> | When using the SCTP multi-homing function, you can specify the number of secondary IPs(upto 3) to be assigned to the service. When used with the loxilb.io/secondaryIPs annotation, the value set in loxilb.io/num-secondary-networks is ignored. (loxilb.io/secondaryIPs annotation takes precedence)<br><br><b>Example:</b><br>metadata:<br>&nbsp;&nbsp;name: sctp-lb1<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/num-secondary-networks: “2”<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-test<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 55002<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer |
| <b>loxilb.io/secondaryIPs</b> | When using the SCTP multi-homing function, specify the secondary IP to be assigned to the service. Multiple IPs(upto 3) can be specified at the same time using a comma(,). When used with the loxilb.io/num-secondary-networks annotation, loxilb.io/secondaryIPs takes priority.)<br><br><b>Example:</b><br>metadata:<br>name: sctp-lb-secips<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/lbmode: "fullnat"<br>loxilb.io/secondaryIPs: "1.1.1.1,2.2.2.2"<br>spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb-secips<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;type: LoadBalancer|
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
| <b>loxilb.io/epselect</b> | Specifies the algorithm for end-point slection e.g "rr", "hash", "persist", "lc" etc. The default value is roundrobin. <br><br><b>Example:</b><br>apiVersion: v1<br>kind: Service<br>metadata:<br>&nbsp;&nbsp;name: sctp-lb<br>&nbsp;&nbsp;annotations:<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/liveness : "yes"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/probetimeout : "10"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/proberetries : "3"<br>&nbsp;&nbsp;&nbsp;&nbsp;loxilb.io/epselect : "hash"<br>&nbsp;&nbsp;&nbsp;&nbsp;spec:<br>&nbsp;&nbsp;loadBalancerClass: loxilb.io/loxilb<br>&nbsp;&nbsp;externalTrafficPolicy: Local<br>&nbsp;&nbsp;selector:<br>&nbsp;&nbsp;&nbsp;&nbsp;what: sctp-lb<br>&nbsp;&nbsp;ports:<br>&nbsp;&nbsp;&nbsp;- port: 56004<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;protocol: SCTP<br>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;targetPort: 9999<br>&nbsp;&nbsp;type: LoadBalancer   |

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
           # Specify number of secondary networks for multi-homing
           # Only valid for SCTP currently
           # loxilb.io/num-secondary-networks: "2
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
            - --externalCIDR=123.123.123.1/24
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

Kube-loxilb provides Custom Resource Definition (CRD). Current the following operations are supported (which would be continually updated):   
-  Add a BGP Peer   
-  Delete a BGP Peer   

An example of CRD is stored in  manifest/crds. Setting up a BGP Peer as an example is as follows:   

1. Pre-Processing (Register kube-loxilb CRDs with K8s). Apply lbpeercrd.yaml as first step   
```
kubectl apply -f manifest/crds/lbpeercrd.yaml
```
2. CRD definition

You need to create a yaml file that adds a peer for BGP. The example below is an example of creating a Peer with a RemoteAS number of Peer IP address 65123 at 123.123.123.2. Create a file named bgp-peer.yaml and add the contents below.   
```yaml
apiVersion: "bgppeer.loxilb.io/v1"
kind: BGPPeerService
metadata:
  name: bgp-peer-test
spec:
  ipAddress: 123.123.123.2
  remoteAs: 65123
  remotePort: 179
```

3. Apply CRD to add a new BGP Peer

```
kubectl apply -f bgp-peer.yaml
```

4. Verify the applied CRD

You can check it in two ways. The first one can be checked through loxicmd(in loxilb container), and the second one can be checked through kubectl.    
```
# loxicmd
kubectl exec -it {loxilb} -n kube-system -- loxicmd get bgpneigh 
|      PEER      |  AS   |   UP/DOWN   |    STATE    | 
|----------------|-------|-------------|-------------|
| 123.123.123.2  | 65123 | never       | ACTIVE      |

# kubectl
kubectl get bgppeerservice
NAME            PEER            AS   
bgp-peer-test   123.123.123.2   65123 
```   
