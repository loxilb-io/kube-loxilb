package loadbalancer

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	infv1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	infv1lister "sigs.k8s.io/gateway-api-inference-extension/client-go/listers/api/v1"
)

// objectRecorder - an EventRecorder that keeps the object of each event too,
// which FakeRecorder does not.
type objectRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	object  runtime.Object
	reason  string
	message string
}

func (r *objectRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{object: object, reason: reason, message: message})
}

func (r *objectRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *objectRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Eventf(object, eventtype, reason, messageFmt, args...)
}

func poolLister(t *testing.T, pools ...*infv1.InferencePool) infv1lister.InferencePoolLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pool := range pools {
		if err := indexer.Add(pool); err != nil {
			t.Fatal(err)
		}
	}
	return infv1lister.NewInferencePoolLister(indexer)
}

func generatedService(pool string) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: pool + "-inference", Namespace: "llm"}}
	if pool != "" {
		svc.Labels = map[string]string{inferencePoolLabel: pool}
	}
	return svc
}

func qwenPool() *infv1.InferencePool {
	return &infv1.InferencePool{ObjectMeta: metav1.ObjectMeta{Name: "qwen3", Namespace: "llm", UID: types.UID("pool-uid")}}
}

// A warning about what the pool asks for lands on the pool as well, naming the
// generated Service.
func TestServiceWarningReachesItsPool(t *testing.T) {
	recorder := &objectRecorder{}
	m := &Manager{eventRecorder: recorder}
	m.SetInferencePoolLister(poolLister(t, qwenPool()))

	m.recordServiceWarning(generatedService("qwen3"), ReasonAuthNotSupported, "not enforced on 10.0.0.11")

	if len(recorder.events) != 2 {
		t.Fatalf("%d events, want one on the Service and one on the pool", len(recorder.events))
	}
	ref, ok := recorder.events[1].object.(*corev1.ObjectReference)
	if !ok {
		t.Fatalf("pool event recorded against %T, want an ObjectReference", recorder.events[1].object)
	}
	want := corev1.ObjectReference{APIVersion: "inference.networking.k8s.io/v1", Kind: "InferencePool",
		Namespace: "llm", Name: "qwen3", UID: "pool-uid"}
	if *ref != want {
		t.Errorf("reference = %+v, want %+v", *ref, want)
	}
	if e := recorder.events[1]; e.reason != ReasonAuthNotSupported || !strings.Contains(e.message, "qwen3-inference") ||
		!strings.Contains(e.message, "not enforced on 10.0.0.11") {
		t.Errorf("pool event = %+v", e)
	}
}

// Only warnings about the pool's own configuration are repeated there; how a
// rule is doing on a peer stays on the Service.
func TestOnlyPoolWarningsReachThePool(t *testing.T) {
	recorder := &objectRecorder{}
	m := &Manager{eventRecorder: recorder}
	m.SetInferencePoolLister(poolLister(t, qwenPool()))

	m.recordServiceWarning(generatedService("qwen3"), ReasonRuleMissing, "gone from 10.0.0.11")

	if len(recorder.events) != 1 {
		t.Errorf("%d events, want the Service's only", len(recorder.events))
	}
}

// Nothing to repeat it on: a plain Service, the extension off, or a pool the
// cache does not have yet.
func TestServiceWarningWithoutAPool(t *testing.T) {
	cases := map[string]func(m *Manager) *corev1.Service{
		"plain Service": func(m *Manager) *corev1.Service {
			m.SetInferencePoolLister(poolLister(t, qwenPool()))
			return generatedService("")
		},
		"extension off": func(m *Manager) *corev1.Service {
			return generatedService("qwen3")
		},
		"pool not cached": func(m *Manager) *corev1.Service {
			m.SetInferencePoolLister(poolLister(t))
			return generatedService("qwen3")
		},
	}
	for name, setup := range cases {
		recorder := &objectRecorder{}
		m := &Manager{eventRecorder: recorder}
		m.recordServiceWarning(setup(m), ReasonLoxiLBRejected, "refused")
		if len(recorder.events) != 1 {
			t.Errorf("%s: %d events, want the Service's only", name, len(recorder.events))
		}
	}
}
