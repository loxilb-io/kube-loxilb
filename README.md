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

## How to use kube-loxilb ?

1.Make sure loxilb docker is downloaded and installed properly in a node external to your cluster. One can follow guides [here](https://loxilb-io.github.io/loxilbdocs/run/) or refer to various other [documentation](https://loxilb-io.github.io/loxilbdocs/#how-to-guides) . It is important to have network connectivity from this node to the master nodes of k8s cluster (where kube-loxilb will eventually run) as seen in the above figure.

2.Download the loxilb config yaml :

```
wget https://raw.githubusercontent.com/loxilb-io/kube-loxilb/main/manifest/ext-cluster/kube-loxilb.yaml
```

3.Modify arguments as per user's needs :
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
- loxiURL : API server address of loxilb. This is the docker IP address loxilb docker of Step 1. If unspecified, kube-loxilb assumes loxilb is running in-cluster mode and autoconfigures this.
- externalCIDR : CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode)    
- externalCIDR6 : Ipv6 CIDR or IPAddress range to allocate addresses from. By default address allocated are shared for different services(shared Mode)   
- monitor : Enable liveness probe for the LB end-points (default : unset)    
- setBGP : Use specified BGP AS-ID to advertise this service. If not specified BGP will be disabled. Please check [here](https://github.com/loxilb-io/loxilbdocs/blob/main/docs/integrate_bgp_eng.md) how it works.    
- extBGPPeers : Specifies external BGP peers with appropriate remote AS    
- setRoles : If present, kube-loxilb arbitrates loxilb role(s) in cluster-mode. Further, it sets a special VIP (selected as sourceIP) to communicate with end-points in full-nat mode.   
- setLBMode : 0, 1, 2   
  0 - default (only DNAT, preserves source-IP)       
  1 - onearm (source IP is changed to load balancer’s interface IP)     
  2 - fullNAT (sourceIP is changed to virtual IP)    
- setUniqueIP : Allocate unique service-IP per LB service (default : false)   
- externalSecondaryCIDRs: Secondary CIDR or IPAddress ranges to allocate addresses from in case of multi-homing support      

Many of the above flags and arguments can be overriden on a per-service basis based on loxilb specific annotation as mentioned in section 6 below.      

4. Apply the following :
```
kubectl apply -f kube-loxilb.yaml
```

5. The above should make sure kube-loxilb is successfully running. Check kube-loxilb is running :

```
kubectl get pods -A | grep kube-loxilb
```


6. Finally to create service LB, we can use and apply the following template yaml    
(<b>Note</b> -  Check <b>*loadBalancerClass*</b> and other <b>*loxilb*</b> specific annotation) :
```yaml
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

7. Verify LB service is created
```
kubectl get svc
```

For more example yaml templates, kindly refer to kube-loxilb's manifest [directory](https://github.com/loxilb-io/kube-loxilb/tree/main/manifest)    
