package api

import (
	"context"
)

type K8sMetaModel struct {
	Pods     []PodMetadata     `json:"pods"`
	Services []ServiceMetadata `json:"services"`
	Nodes    []NodeMetadata    `json:"nodes"`
}

func (k8sMetaModel *K8sMetaModel) GetKeyStruct() LoxiModel {
	return k8sMetaModel
}

type PodMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	IP        string            `json:"ip"`
	NodeName  string            `json:"nodeName"`
	Labels    map[string]string `json:"labels"`
}

type ServiceMetadata struct {
	Name       string        `json:"name"`
	Namespace  string        `json:"namespace"`
	ClusterIP  string        `json:"clusterIP"`
	ExternalIP string        `json:"externalIP"`
	Ports      []ServicePort `json:"ports"`
}

type ServicePort struct {
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	NodePort   int    `json:"nodePort"`
	TargetPort int    `json:"targetPort"`
}

type NodeMetadata struct {
	Name       string            `json:"name"`
	InternalIP string            `json:"internalIP"`
	Labels     map[string]string `json:"labels"`
	Status     string            `json:"status"`
}

type K8sMetaAPI struct {
	resource  string
	provider  string
	version   string
	deleteKey []string
	client    *RESTClient
	APICommonFunc
}

func newK8sMetaAPI(r *RESTClient) *K8sMetaAPI {
	return &K8sMetaAPI{
		resource:  "config/k8smeta",
		deleteKey: []string{"name", "namespace"}, // TODO
		provider:  r.provider,
		version:   r.version,
		client:    r,
	}
}

func (l *K8sMetaAPI) GetModel() LoxiModel {
	return &K8sMetaModel{}
}

func (l *K8sMetaAPI) Create(ctx context.Context, lbModel LoxiModel) error {
	resp := l.client.POST(l.resource).Body(lbModel).Do(ctx)
	if resp.err != nil {
		return resp.err
	}
	return nil
}
