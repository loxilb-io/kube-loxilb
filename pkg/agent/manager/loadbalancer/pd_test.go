package loadbalancer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/loxilb-io/kube-loxilb/pkg/agent/config"
	"github.com/loxilb-io/kube-loxilb/pkg/api"
)

func pdPod(name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Status: corev1.PodStatus{
			PodIP:  ip,
			PodIPs: []corev1.PodIP{{IP: ip}},
		},
	}
}

// pdManager builds a Manager backed by a fake API server holding pods, with the
// pod watcher wired to it and torn down with the test.
func pdManager(t *testing.T, pods ...*corev1.Pod) *Manager {
	t.Helper()

	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	client := k8sfake.NewSimpleClientset(objs...)

	m := &Manager{
		kubeClient:    client,
		networkConfig: &config.NetworkConfig{SetLBMode: uint16(api.LBModeFullProxy)},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(time.Millisecond, time.Second), "test"),
	}
	m.pdPods = newPDPodWatcher(client, 0, m.enqueueServicesForPod)

	stop := make(chan struct{})
	m.pdPods.SetStopCh(stop)
	t.Cleanup(func() { close(stop) })

	return m
}

func pdAnnotations() map[string]string {
	return map[string]string{
		lbModeAnnotation:            "fullproxy",
		usePodNetworkAnnotation:     "yes",
		pdDisaggAnnotation:          "true",
		pdPrefillSelectorAnnotation: "llm-role=prefill",
		pdDecodeSelectorAnnotation:  "llm-role=decode",
		pdPrefillNixlPortAnnotation: "9001",
		pdDecodeNixlPortAnnotation:  "9002",
		sseModeAnnotation:           "true",
	}
}

func TestResolvePDRoles(t *testing.T) {
	m := pdManager(t,
		pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-prefill-1", "10.244.1.11", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-decode-0", "10.244.2.10", map[string]string{"llm-role": "decode"}),
		pdPod("unrelated", "10.244.3.10", map[string]string{"app": "nginx"}),
	)
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if !aiArgs.PDDisaggMode {
		t.Fatal("PDDisaggMode was not parsed")
	}

	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	want := map[string]pdEndpointRole{
		"10.244.1.10": {Role: api.EpRolePrefill, NixlPort: 9001},
		"10.244.1.11": {Role: api.EpRolePrefill, NixlPort: 9001},
		"10.244.2.10": {Role: api.EpRoleDecode, NixlPort: 9002},
	}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for ip, w := range want {
		if roles[ip] != w {
			t.Errorf("roles[%s] = %+v, want %+v", ip, roles[ip], w)
		}
	}
	if _, matched := roles["10.244.3.10"]; matched {
		t.Error("a pod matching neither selector was given a role")
	}
}

// A pod in both pools would make the rule depend on map iteration order.
func TestResolvePDRolesRejectsOverlappingSelectors(t *testing.T) {
	m := pdManager(t,
		pdPod("both", "10.244.1.10", map[string]string{"llm-role": "prefill", "tier": "gpu"}),
		pdPod("decode-0", "10.244.2.10", map[string]string{"llm-role": "decode", "tier": "gpu"}),
	)
	annotations := pdAnnotations()
	annotations[pdDecodeSelectorAnnotation] = "tier=gpu"
	svc := aiSvc(annotations)

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}

	_, err = m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err == nil {
		t.Fatal("overlapping selectors were accepted")
	}
	if !strings.Contains(err.Error(), "matches both") {
		t.Errorf("error = %q, want it to name the overlap", err)
	}
}

// Without disaggregation nothing is resolved, no endpoint gains a role, and -
// the point of starting the watch lazily - no pod watch is registered at all.
func TestResolvePDRolesInertWithoutDisagg(t *testing.T) {
	m := pdManager(t, pdPod("p", "10.244.1.10", map[string]string{"llm-role": "prefill"}))

	roles, err := m.resolvePDRoles(context.Background(), aiSvc(nil), api.AIArgs{SseMode: true})
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}
	if roles != nil {
		t.Errorf("roles = %v, want nil", roles)
	}

	m.pdPods.mu.Lock()
	defer m.pdPods.mu.Unlock()
	if len(m.pdPods.watches) != 0 {
		t.Errorf("a pod watch was started for a service that does not use P/D: %v", m.pdPods.watches)
	}
}

// The cache is narrowed to pods that could match either pool, but only when the
// selectors guarantee the key is present on every such pod.
func TestPodCacheScope(t *testing.T) {
	tests := []struct {
		name    string
		prefill string
		decode  string
		want    string
	}{
		{
			name:    "shared key narrows the cache",
			prefill: "llm-role=prefill",
			decode:  "llm-role=decode",
			want:    "llm-role",
		},
		{
			name:    "set-based membership still implies the key",
			prefill: "llm-role in (prefill)",
			decode:  "llm-role in (decode,decode2)",
			want:    "llm-role",
		},
		{
			name:    "multiple shared keys are ANDed",
			prefill: "app=vllm,llm-role=prefill",
			decode:  "app=vllm,llm-role=decode",
			want:    "app,llm-role",
		},
		{
			name:    "pools keyed differently cannot be ANDed",
			prefill: "llm-role=prefill",
			decode:  "tier=decode",
			want:    "",
		},
		{
			// Kubernetes matches != against objects carrying no such label,
			// so requiring the key would hide them.
			name:    "inequality does not imply the key",
			prefill: "llm-role!=decode",
			decode:  "llm-role=decode",
			want:    "",
		},
		{
			name:    "empty selector matches everything",
			prefill: "",
			decode:  "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := pdAnnotations()
			annotations[pdPrefillSelectorAnnotation] = tt.prefill
			annotations[pdDecodeSelectorAnnotation] = tt.decode

			pools, err := parsePDPools(aiSvc(annotations))
			if err != nil {
				t.Fatalf("parsePDPools: %v", err)
			}
			if got := podCacheScope(pools); got != tt.want {
				t.Errorf("podCacheScope = %q, want %q", got, tt.want)
			}
		})
	}
}

func pdService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm", Namespace: "default", Annotations: pdAnnotations()},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
}

// Relabelling a pod in place keeps its address, so nothing but a pod watch can
// notice it. The watch has to requeue the services the pod left as well as the
// ones it joined.
func TestPodRelabelRequeuesService(t *testing.T) {
	ctx := context.Background()
	prefill := pdPod("vllm-0", "10.244.1.10", map[string]string{"llm-role": "prefill"})
	m := pdManager(t, prefill, pdPod("vllm-1", "10.244.2.10", map[string]string{"llm-role": "decode"}))

	svc := pdService()
	factory := informers.NewSharedInformerFactory(m.kubeClient, 0)
	serviceInformer := factory.Core().V1().Services()
	if err := serviceInformer.Informer().GetIndexer().Add(svc); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	m.serviceLister = serviceInformer.Lister()

	pools, err := parsePDPools(svc)
	if err != nil {
		t.Fatalf("parsePDPools: %v", err)
	}
	if _, err := m.pdPods.Lister(ctx, "default", podCacheScope(pools)); err != nil {
		t.Fatalf("start pod watch: %v", err)
	}

	if m.queue.Len() != 0 {
		t.Fatalf("queue was not empty before the relabel: %d", m.queue.Len())
	}

	// move the pod from the prefill pool to the decode pool
	moved := prefill.DeepCopy()
	moved.Labels["llm-role"] = "decode"
	if _, err := m.kubeClient.CoreV1().Pods("default").Update(ctx, moved, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for m.queue.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.queue.Len() == 0 {
		t.Fatal("relabelling a pod did not requeue the service")
	}

	key, _ := m.queue.Get()
	if want := (LbCacheKey{Namespace: "default", Name: "vllm"}); key != want {
		t.Errorf("queued %v, want %v", key, want)
	}
}

// A pod whose labels never touch either pool must not wake the service.
func TestUnrelatedPodUpdateDoesNotRequeue(t *testing.T) {
	ctx := context.Background()
	other := pdPod("nginx-0", "10.244.9.9", map[string]string{"app": "nginx"})
	m := pdManager(t, other, pdPod("vllm-1", "10.244.2.10", map[string]string{"llm-role": "decode"}))

	svc := pdService()
	factory := informers.NewSharedInformerFactory(m.kubeClient, 0)
	serviceInformer := factory.Core().V1().Services()
	if err := serviceInformer.Informer().GetIndexer().Add(svc); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	m.serviceLister = serviceInformer.Lister()

	// unscoped, so the unrelated pod is cached and its update is delivered
	if _, err := m.pdPods.Lister(ctx, "default", ""); err != nil {
		t.Fatalf("start pod watch: %v", err)
	}

	changed := other.DeepCopy()
	changed.Labels["app"] = "nginx2"
	if _, err := m.kubeClient.CoreV1().Pods("default").Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if m.queue.Len() != 0 {
		t.Errorf("an unrelated pod update requeued the service")
	}
}

// The change detector has to compare the derived endpoint, not the address:
// a relabelled pod keeps its address, so an address-only diff reports "nothing
// changed" and the stale role stays programmed in loxilb.
func TestCheckUpdateEndpointsDetectsRoleChange(t *testing.T) {
	m := &Manager{lbCache: LbCacheTable{}}
	cacheKey := "default/vllm"
	m.lbCache[cacheKey] = &LbCacheEntry{
		LbServicePairs: map[string]*LbServicePairEntry{
			"sp": {
				LbModelList: []api.LoadBalancerModel{{
					Endpoints: []api.LoadBalancerEndpoint{
						{EndpointIP: "10.244.1.10", EpRole: api.EpRolePrefill, NixlPort: 9001},
						{EndpointIP: "10.244.2.10", EpRole: api.EpRoleDecode, NixlPort: 9002},
					},
				}},
			},
		},
	}

	ips := []string{"10.244.1.10", "10.244.2.10"}
	unchanged := map[string]pdEndpointRole{
		"10.244.1.10": {Role: api.EpRolePrefill, NixlPort: 9001},
		"10.244.2.10": {Role: api.EpRoleDecode, NixlPort: 9002},
	}
	if m.checkUpdateEndpoints(pdService(), cacheKey, ips, unchanged, false) {
		t.Error("an unchanged endpoint set was reported as changed")
	}

	swapped := map[string]pdEndpointRole{
		"10.244.1.10": {Role: api.EpRoleDecode, NixlPort: 9002},
		"10.244.2.10": {Role: api.EpRolePrefill, NixlPort: 9001},
	}
	if !m.checkUpdateEndpoints(pdService(), cacheKey, ips, swapped, false) {
		t.Error("a role swap at identical addresses went undetected")
	}

	portChanged := map[string]pdEndpointRole{
		"10.244.1.10": {Role: api.EpRolePrefill, NixlPort: 9101},
		"10.244.2.10": {Role: api.EpRoleDecode, NixlPort: 9002},
	}
	if !m.checkUpdateEndpoints(pdService(), cacheKey, ips, portChanged, false) {
		t.Error("a nixl_port change at identical addresses went undetected")
	}
}

func TestGetAIArgsPDRejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		lbMode  int
		wantErr string
	}{
		{
			name: "no selectors",
			mutate: func(a map[string]string) {
				delete(a, pdPrefillSelectorAnnotation)
				delete(a, pdDecodeSelectorAnnotation)
			},
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "requires both loxilb.io/pd-prefill-selector and loxilb.io/pd-decode-selector",
		},
		{
			name:    "only the prefill selector",
			mutate:  func(a map[string]string) { delete(a, pdDecodeSelectorAnnotation) },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "requires both",
		},
		{
			name:    "outside fullproxy",
			mutate:  func(a map[string]string) {},
			lbMode:  int(api.LBModeFullNat),
			wantErr: "pd-disagg requires mode=fullproxy",
		},
		{
			name:    "malformed selector",
			mutate:  func(a map[string]string) { a[pdPrefillSelectorAnnotation] = "llm-role==!!" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "is not a label selector",
		},
		{
			name:    "nixl port out of range",
			mutate:  func(a map[string]string) { a[pdPrefillNixlPortAnnotation] = "70000" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "loxilb.io/pd-prefill-nixl-port must be within 0..65535",
		},
		{
			name:    "cache-aware without disagg",
			mutate:  func(a map[string]string) { delete(a, pdDisaggAnnotation); a[pdCacheAwareAnnotation] = "true" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "pd-cache-aware requires pd_disagg_mode=true",
		},
		{
			name:    "kv-exact single-role contradicts disagg",
			mutate:  func(a map[string]string) { a[kvExactModeAnnotation] = "3" },
			lbMode:  int(api.LBModeFullProxy),
			wantErr: "incompatible with pd-disagg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := pdAnnotations()
			tt.mutate(annotations)
			m := pdManager(t)

			_, err := m.getAIArgs(aiSvc(annotations), tt.lbMode, api.LbSelRr)
			if err == nil {
				t.Fatalf("getAIArgs = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("getAIArgs = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// kvExactMode=1 is only legal alongside disaggregation, and now reachable.
func TestGetAIArgsKvExactZmqWithDisagg(t *testing.T) {
	annotations := pdAnnotations()
	annotations[kvExactModeAnnotation] = "1"
	m := pdManager(t)

	got, err := m.getAIArgs(aiSvc(annotations), int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	if got.KvExactMode != api.KvExactModeZmq || !got.PDDisaggMode {
		t.Errorf("got kvExactMode=%d pdDisagg=%v, want 1/true", got.KvExactMode, got.PDDisaggMode)
	}
}

// The assembled payload must match the shape loxilb's own P/D scenario posts:
// pd_disagg_mode on the service, and ep_role plus nixl_port on every endpoint.
func TestPDPayloadMatchesGatewayScenario(t *testing.T) {
	m := pdManager(t,
		pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}),
		pdPod("vllm-decode-0", "10.244.2.10", map[string]string{"llm-role": "decode"}),
	)
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	// mirrors how makeLoxiLoadBalancerModel stamps the endpoints
	var eps []api.LoadBalancerEndpoint
	for _, ip := range []string{"10.244.1.10", "10.244.2.10"} {
		r := roles[ip]
		eps = append(eps, api.LoadBalancerEndpoint{
			EndpointIP: ip, TargetPort: 8000, Weight: 1,
			EpRole: r.Role, NixlPort: r.NixlPort,
		})
	}

	lb := api.LoadBalancerModel{
		Service: api.LoadBalancerService{
			ExternalIP: "20.20.20.1", Port: 2020, Protocol: "tcp",
			Mode: api.LBModeFullProxy, AIArgs: aiArgs,
		},
		Endpoints: eps,
	}

	if err := lb.Service.AIArgs.Validate(lb.Service.Mode, lb.Endpoints); err != nil {
		t.Fatalf("assembled payload fails validation: %v", err)
	}

	body, err := json.Marshal(&lb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(body)

	for _, want := range []string{
		`"pd_disagg_mode":true`, `"sse_mode":true`, `"mode":4`,
		`"ep_role":1`, `"nixl_port":9001`,
		`"ep_role":2`, `"nixl_port":9002`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %s\n%s", want, payload)
		}
	}
}

// A disaggregated rule whose decode pool has no ready pods must be refused
// before it reaches loxilb.
func TestPDPayloadRejectedWhenAPoolIsEmpty(t *testing.T) {
	m := pdManager(t, pdPod("vllm-prefill-0", "10.244.1.10", map[string]string{"llm-role": "prefill"}))
	svc := aiSvc(pdAnnotations())

	aiArgs, err := m.getAIArgs(svc, int(api.LBModeFullProxy), api.LbSelRr)
	if err != nil {
		t.Fatalf("getAIArgs: %v", err)
	}
	roles, err := m.resolvePDRoles(context.Background(), svc, aiArgs)
	if err != nil {
		t.Fatalf("resolvePDRoles: %v", err)
	}

	r := roles["10.244.1.10"]
	eps := []api.LoadBalancerEndpoint{
		{EndpointIP: "10.244.1.10", TargetPort: 8000, Weight: 1, EpRole: r.Role, NixlPort: r.NixlPort},
	}

	err = aiArgs.Validate(api.LBModeFullProxy, eps)
	if err == nil {
		t.Fatal("a prefill-only disaggregated rule was accepted")
	}
	if !strings.Contains(err.Error(), "at least 1 prefill (ep_role=1) and 1 decode (ep_role=2)") {
		t.Errorf("error = %q, want loxilb's own wording", err)
	}
}

// KV-exact routing needs a tokenizer staged inside the loxilb pod. kube-loxilb
// cannot see it, so it names the exact path instead of guessing at readiness.
func TestTokenizerNotice(t *testing.T) {
	tests := []struct {
		name     string
		aiArgs   api.AIArgs
		wantNone bool
		contains []string
	}{
		{
			name:     "not used",
			aiArgs:   api.AIArgs{SseMode: true},
			wantNone: true,
		},
		{
			name:   "named model gives an exact path",
			aiArgs: api.AIArgs{KvExactMode: api.KvExactModeSingleRole, ModelName: "meta-llama/Llama-3.1-70B-Instruct"},
			contains: []string{
				"/etc/loxilb/tokenizers/meta-llama__Llama-3.1-70B-Instruct/tokenizer.json",
				"needs a loxilb restart",
			},
		},
		{
			name:   "nested slashes each become a double underscore",
			aiArgs: api.AIArgs{KvExactMode: api.KvExactModeZmq, ModelName: "a/b/c"},
			contains: []string{
				"/etc/loxilb/tokenizers/a__b__c/tokenizer.json",
			},
		},
		{
			name:   "catch-all rule can only name the directory",
			aiArgs: api.AIArgs{KvExactMode: api.KvExactModeSingleRole},
			contains: []string{
				"/etc/loxilb/tokenizers/<model>/tokenizer.json",
				"loxilb.io/model-name is unset",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenizerNotice(tt.aiArgs)

			if tt.wantNone {
				if got != "" {
					t.Errorf("tokenizerNotice = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatal("tokenizerNotice = empty, want a notice")
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("notice missing %q\n%s", want, got)
				}
			}
		})
	}
}

// The notice reaches the Service, as a Normal event: nothing has been detected
// as wrong, so it must not devalue the Warnings that report real rejections.
func TestRecordTokenizerNoticeEmitsNormalEvent(t *testing.T) {
	recorder := record.NewFakeRecorder(4)
	m := &Manager{eventRecorder: recorder}
	svc := pdService()

	m.recordTokenizerNotice(svc, api.AIArgs{
		KvExactMode: api.KvExactModeSingleRole,
		ModelName:   "meta-llama/Llama-3.1-70B-Instruct",
	})

	select {
	case event := <-recorder.Events:
		if !strings.HasPrefix(event, corev1.EventTypeNormal+" "+ReasonKvExactTokenizerRequired) {
			t.Errorf("event = %q, want a Normal %s event", event, ReasonKvExactTokenizerRequired)
		}
		if !strings.Contains(event, "/etc/loxilb/tokenizers/meta-llama__Llama-3.1-70B-Instruct/tokenizer.json") {
			t.Errorf("event does not name the tokenizer path: %s", event)
		}
	default:
		t.Fatal("no event was recorded")
	}

	// a rule that does not use KV-exact routing stays quiet
	m.recordTokenizerNotice(svc, api.AIArgs{SseMode: true})
	select {
	case event := <-recorder.Events:
		t.Errorf("unexpected event for a non-KV-exact rule: %s", event)
	default:
	}
}
