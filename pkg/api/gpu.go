package api

import (
	"context"
)

// GPURoutingModeGpuAware - the routing_mode the gateway reports when GPU-aware
// selection is actually armed. Anything else means standard CHWBL.
const GPURoutingModeGpuAware = "gpu_aware"

// GPUStatusModel - response of GET /netlox/v1/config/gpu/status.
//
// GPU-aware selection is not a property of the rule. A rule carrying sel=9
// writes its selector unconditionally, while whether that selector runs is
// decided by a process-global routing mode on the loxilb instance, default off.
// With it off the rule is programmed, accepted, and selects as CHWBL - the
// gateway says so itself when disabling: "GPU-aware load balancing disabled,
// falling back to standard CHWBL".
//
// This endpoint is the one signal that makes that state visible.
type GPUStatusModel struct {
	// Enabled - whether GPU monitoring is currently active.
	Enabled bool `json:"enabled"`
	// RoutingMode - "standard_chwbl" or "gpu_aware".
	RoutingMode string `json:"routing_mode,omitempty"`
	// WorkerCount - number of workers whose GPU metrics are being tracked.
	// Zero means the telemetry feed has reported nothing yet, so GPU-aware
	// selection has no data to act on even when Enabled is true.
	WorkerCount int `json:"worker_count,omitempty"`
	// LastMetricsUpdate - timestamp of the last metrics push.
	LastMetricsUpdate string `json:"last_metrics_update,omitempty"`
	// EbpfMapLoaded - whether the eBPF maps are loaded.
	EbpfMapLoaded bool `json:"ebpf_map_loaded,omitempty"`
}

func (g *GPUStatusModel) GetKeyStruct() LoxiModel {
	return nil
}

// GPUArmed - whether GPU-aware selection would actually run on this instance.
func (g *GPUStatusModel) GPUArmed() bool {
	return g.Enabled && (g.RoutingMode == "" || g.RoutingMode == GPURoutingModeGpuAware)
}

type GPUAPI struct {
	resource string
	provider string
	version  string
	client   *RESTClient
	APICommonFunc
}

func newGPUAPI(r *RESTClient) *GPUAPI {
	return &GPUAPI{
		resource: "config/gpu",
		provider: r.provider,
		version:  r.version,
		client:   r,
	}
}

func (g *GPUAPI) GetModel() LoxiModel {
	return &GPUStatusModel{}
}

// Status - read GPU monitoring state.
//
// Read-only on purpose. kube-loxilb does not drive enable and disable: that is
// one instance-wide switch with no reference counting, and arming it is only
// the first of three stages - the second is a per-endpoint GPU telemetry feed
// that comes from the serving engine or DCGM, not from the Kubernetes API.
// Flipping the switch alone would enter GPU-aware mode against an empty stats
// map, which is worse than leaving it off.
func (g *GPUAPI) Status(ctx context.Context) (*GPUStatusModel, error) {
	statusModel := &GPUStatusModel{}

	resp := g.client.GET(g.resource).SubResource("status").Do(ctx).UnMarshal(statusModel)
	if resp.err != nil {
		return nil, resp.err
	}

	return statusModel, nil
}
