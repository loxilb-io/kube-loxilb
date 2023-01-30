## What is kube-loxilb ?

kube-loxilb is loxilb's implementation of kubernetes service load-balancer spec which includes support for load-balancer class, IPAM (shared or exclusive) etc.

## How is kube-loxilb different from [loxi-ccm](https://github.com/loxilb-io/loxi-ccm) ?

loxi-ccm also provides implementation of kubernetes load-balancer spec but it runs as a part of cloud-provider and provides load-balancer life-cycle management as part of it. If one does not have a cloud-provider or wants to integrate loxilb with their existing cloud-provider, one can use or include loxi-ccm as a part of it

kube-loxilb is a standalone implementation of kubernetes load-balancer spec which does not depend on cloud-provider. It runs as a kube-system deployment and provisions load-balancer based on load-balancer class. It only acts on load-balancers for the LB classes that is provided by itself. This also allows us to have different load-balancers working together in the same K8s environment.

## How to use kube-loxilb ?

Download the loxilb config yaml :
```
wget https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/kube-loxilb.yaml
```

Modify arguments as per user's needs :
```
args:
        - --loxiURL=http://12.12.12.1:11111
        - --externalCIDR=123.123.123.1/24

```

Apply the following :
```
kubectl apply -f kube-loxilb.yaml
```

The above should make sure kube-loxilb is successfully running. Finally to create service LB, we can use and apply the following template yaml (with loadBalancerClass) :
```
apiVersion: v1
kind: Service
metadata:
  name: iperf-service
spec:
  externalTrafficPolicy: Local
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




