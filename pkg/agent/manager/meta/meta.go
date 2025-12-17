/*
 * Copyright (c) 2025 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"context"
	"strings"
	"time"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	mgrName        = "LoxilbK8sMetadataManager"
	defaultWorkers = 1
	minRetryDelay  = 2 * time.Second
	maxRetryDelay  = 120 * time.Second
	resyncPeriod   = 60 * time.Second
)

type Manager struct {
	kubeClient          kubernetes.Interface
	loxiClients         *api.LoxiClientPool
	networkConfig       *config.NetworkConfig
	podLister           corelisters.PodLister
	podListerSynced     cache.InformerSynced
	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced
	nodeLister          corelisters.NodeLister
	nodeListerSynced    cache.InformerSynced
}

// Create and Init Manager.
func NewMetaManager(
	kubeClient kubernetes.Interface,
	networkConfig *config.NetworkConfig,
	informerFactory informers.SharedInformerFactory,
	LoxiClients *api.LoxiClientPool,
) *Manager {

	podInformer := informerFactory.Core().V1().Pods()
	serviceInformer := informerFactory.Core().V1().Services()
	nodeInformer := informerFactory.Core().V1().Nodes()

	manager := &Manager{
		kubeClient:          kubeClient,
		loxiClients:         LoxiClients,
		networkConfig:       networkConfig,
		podLister:           podInformer.Lister(),
		podListerSynced:     podInformer.Informer().HasSynced,
		serviceLister:       serviceInformer.Lister(),
		serviceListerSynced: serviceInformer.Informer().HasSynced,
		nodeLister:          nodeInformer.Lister(),
		nodeListerSynced:    nodeInformer.Informer().HasSynced,
	}
	return manager
}

func (m *Manager) Run(stopCh <-chan struct{}) {
	klog.Infof("Starting %s", mgrName)
	defer klog.Infof("Shutting down %s", mgrName)

	if !cache.WaitForNamedCacheSync(
		mgrName,
		stopCh,
		m.podListerSynced,
		m.serviceListerSynced,
		m.nodeListerSynced) {
		return
	}

	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(m.worker, resyncPeriod, stopCh)
	}
	<-stopCh
}

func (m *Manager) worker() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body := &api.K8sMetaModel{}
	svcList, err := m.serviceLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list services: %v", err)
		return
	}

	for _, svc := range svcList {
		// add service metadata
		if svc.Spec.Type != "LoadBalancer" || svc.Spec.LoadBalancerClass == nil {
			continue
		}
		if strings.Compare(*svc.Spec.LoadBalancerClass, m.networkConfig.LoxilbLoadBalancerClass) != 0 {
			continue
		}

		body.Services = append(body.Services, m.createServiceMetaData(svc))

		// add pod metadata
		podSelector := labels.Set(svc.Spec.Selector)
		podList, err := m.podLister.List(podSelector.AsSelector())
		if err != nil {
			klog.Errorf("Failed to list pods: %v", err)
			return
		}

		for _, pod := range podList {
			body.Pods = append(body.Pods, m.createPodMetaData(pod))
		}
	}

	// add node metadata
	nodeList, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes: %v", err)
		return
	}
	for _, node := range nodeList {
		body.Nodes = append(body.Nodes, api.NodeMetadata{
			Name:       node.Name,
			InternalIP: node.Status.Addresses[0].Address,
			Labels:     node.Labels,
			Status:     string(node.Status.Conditions[len(node.Status.Conditions)-1].Status),
		})
	}

	// send metadata to Loxi
	for _, loxiClient := range m.loxiClients.Clients {
		if loxiClient.IsAlive {
			err := loxiClient.K8sMeta().Create(ctx, body)
			if err != nil {
				klog.Errorf("Failed to send metadata to Loxi(%s): %v", loxiClient.Host, err)
			}
		}
	}
}

func (m *Manager) createServiceMetaData(svc *corev1.Service) api.ServiceMetadata {

	externalIP := ""
	if svc.Status.LoadBalancer.Ingress != nil && len(svc.Status.LoadBalancer.Ingress) != 0 {
		externalIP = svc.Status.LoadBalancer.Ingress[0].Hostname
		if externalIP == "" {
			externalIP = svc.Status.LoadBalancer.Ingress[0].IP
		}
	}

	svcMeta := api.ServiceMetadata{
		Name:       svc.Name,
		Namespace:  svc.Namespace,
		ClusterIP:  svc.Spec.ClusterIP,
		ExternalIP: externalIP,
		Ports:      make([]api.ServicePort, 0),
	}

	for _, port := range svc.Spec.Ports {
		svcMeta.Ports = append(svcMeta.Ports, api.ServicePort{
			Port:       int(port.Port),
			Protocol:   string(port.Protocol),
			NodePort:   int(port.NodePort),
			TargetPort: int(port.TargetPort.IntVal),
		})
	}

	return svcMeta
}

func (m *Manager) createPodMetaData(pod *corev1.Pod) api.PodMetadata {
	podMeta := api.PodMetadata{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		NodeName:  pod.Spec.NodeName,
		IP:        pod.Status.PodIP,
		Labels:    pod.Labels,
	}

	return podMeta
}
