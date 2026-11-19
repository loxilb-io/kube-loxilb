package gatewayapi

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

func testQueue() workqueue.RateLimitingInterface {
	return workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
}

func tombstone(obj interface{}) cache.DeletedFinalStateUnknown {
	return cache.DeletedFinalStateUnknown{Key: "ns/name", Obj: obj}
}

// queued - the one item q holds, or nil when it holds none. More than one
// fails the test.
func queued(t *testing.T, q workqueue.RateLimitingInterface) interface{} {
	t.Helper()
	switch q.Len() {
	case 0:
		return nil
	case 1:
		item, _ := q.Get()
		q.Done(item)
		return item
	default:
		t.Fatalf("queue holds %d items, want at most 1", q.Len())
		return nil
	}
}

// A delete the watch missed arrives as a tombstone. Each manager has to queue
// the object inside it - reading the tombstone as the object left a nil that
// the next line dereferenced, and kube-loxilb went down with it.
func TestEnqueueObjectUnwrapsTombstones(t *testing.T) {
	meta := metav1.ObjectMeta{Namespace: "gw-ns", Name: "obj"}

	gateway := &v1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gw-ns", Name: "obj",
			Annotations: map[string]string{"ident-ipam": "ident"}},
		Status: v1.GatewayStatus{Addresses: []v1.GatewayStatusAddress{{Value: "10.0.0.1"}}},
	}

	cases := []struct {
		name    string
		obj     interface{}
		enqueue func(q workqueue.RateLimitingInterface, obj interface{})
		check   func(t *testing.T, item interface{})
	}{
		{
			name: "GatewayClass",
			obj:  &v1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "obj"}},
			enqueue: func(q workqueue.RateLimitingInterface, obj interface{}) {
				(&GatewayClassManager{queue: q}).enqueueObject(obj)
			},
			check: func(t *testing.T, item interface{}) {
				if item != "obj" {
					t.Errorf("queued %#v, want %q", item, "obj")
				}
			},
		},
		{
			name: "Gateway",
			obj:  gateway,
			enqueue: func(q workqueue.RateLimitingInterface, obj interface{}) {
				(&GatewayManager{queue: q}).enqueueObject(obj)
			},
			check: func(t *testing.T, item interface{}) {
				// The delete returns these addresses to the pool, so the
				// queued Gateway has to be the one the tombstone carries.
				if item != gateway {
					t.Errorf("queued %#v, want the Gateway the tombstone carries", item)
				}
			},
		},
		{
			name: "HTTPRoute",
			obj:  &v1.HTTPRoute{ObjectMeta: meta},
			enqueue: func(q workqueue.RateLimitingInterface, obj interface{}) {
				(&HTTPRouteManager{queue: q}).enqueueObject(obj)
			},
			check: func(t *testing.T, item interface{}) {
				if want := (HTTPRouteQueueEntry{"gw-ns", "obj"}); item != want {
					t.Errorf("queued %#v, want %#v", item, want)
				}
			},
		},
		{
			name: "TCPRoute",
			obj:  &v1alpha2.TCPRoute{ObjectMeta: meta},
			enqueue: func(q workqueue.RateLimitingInterface, obj interface{}) {
				(&TCPRouteManager{&l4RouteManager{queue: q, l4RouteKind: tcpRouteKind(nil)}}).enqueueObject(obj)
			},
			check: func(t *testing.T, item interface{}) {
				if want := (l4RouteKey{"gw-ns", "obj"}); item != want {
					t.Errorf("queued %#v, want %#v", item, want)
				}
			},
		},
		{
			name: "UDPRoute",
			obj:  &v1alpha2.UDPRoute{ObjectMeta: meta},
			enqueue: func(q workqueue.RateLimitingInterface, obj interface{}) {
				(&UDPRouteManager{&l4RouteManager{queue: q, l4RouteKind: udpRouteKind(nil)}}).enqueueObject(obj)
			},
			check: func(t *testing.T, item interface{}) {
				if want := (l4RouteKey{"gw-ns", "obj"}); item != want {
					t.Errorf("queued %#v, want %#v", item, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("object", func(t *testing.T) {
				q := testQueue()
				tc.enqueue(q, tc.obj)
				tc.check(t, queued(t, q))
			})

			t.Run("tombstone", func(t *testing.T) {
				q := testQueue()
				tc.enqueue(q, tombstone(tc.obj))
				tc.check(t, queued(t, q))
			})

			// Not expected from a typed informer, but it must not be
			// queued as a nil object either.
			t.Run("tombstone of another kind", func(t *testing.T) {
				q := testQueue()
				tc.enqueue(q, tombstone(&v1.GRPCRoute{ObjectMeta: meta}))
				if item := queued(t, q); item != nil {
					t.Errorf("queued %#v, want nothing", item)
				}
			})

			t.Run("unexpected object", func(t *testing.T) {
				q := testQueue()
				tc.enqueue(q, "not an object")
				if item := queued(t, q); item != nil {
					t.Errorf("queued %#v, want nothing", item)
				}
			})
		})
	}
}
