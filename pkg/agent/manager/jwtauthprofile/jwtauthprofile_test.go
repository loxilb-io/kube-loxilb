package jwtauthprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/loxilb-io/kube-loxilb/pkg/api"
	jwtv1 "github.com/loxilb-io/kube-loxilb/pkg/crds/jwtauthprofile/v1"
	jwtfake "github.com/loxilb-io/kube-loxilb/pkg/jwtauthprofile-client/clientset/versioned/fake"
	jwtinformers "github.com/loxilb-io/kube-loxilb/pkg/jwtauthprofile-client/informers/externalversions"
)

// fakeGateway - a loxilb-inference-gateway's profile endpoints.
type fakeGateway struct {
	srv *httptest.Server

	mu           sync.Mutex
	features     []string
	createStatus int
	deleteStatus int
	posted       []map[string]any
	deletes      int
}

func newFakeGateway(t *testing.T, features ...string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{features: features}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/netlox/v1/version":
			_ = json.NewEncoder(w).Encode(&api.VersionModel{Version: "test", Product: api.ProductInferenceGateway, Features: g.features})
		case r.Method == http.MethodPost && r.URL.Path == "/netlox/v1/config/ai/jwtauthprofile":
			body, _ := io.ReadAll(r.Body)
			var profile map[string]any
			_ = json.Unmarshal(body, &profile)
			g.posted = append(g.posted, profile)
			if g.createStatus != 0 {
				w.WriteHeader(g.createStatus)
				_, _ = w.Write([]byte(`{"result":"issuer must be an http(s) URL"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/netlox/v1/config/ai/jwtauthprofile/"):
			g.deletes++
			if g.deleteStatus != 0 {
				w.WriteHeader(g.deleteStatus)
				_, _ = w.Write([]byte(`{"result":"jwt auth profile kc is referenced by rule(s): llm_vllm:llb-inst0"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":"Success"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) client(t *testing.T) *api.LoxiClient {
	t.Helper()
	base, err := url.Parse(g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := api.NewRESTClient(base, "netlox", "v1", g.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c := &api.LoxiClient{RestClient: rc, Url: g.srv.URL, Host: base.Host, IsAlive: true}
	c.DetectFlavor(context.Background())
	return c
}

func (g *fakeGateway) counts() (posts, deletes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.posted), g.deletes
}

// eventLog - an EventRecorder that keeps what it is given.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) Event(_ runtime.Object, eventtype, reason, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, eventtype+" "+reason+" "+message)
}

func (e *eventLog) Eventf(o runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	e.Event(o, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (e *eventLog) AnnotatedEventf(o runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	e.Eventf(o, eventtype, reason, messageFmt, args...)
}

func kc() *jwtv1.JWTAuthProfile {
	return &jwtv1.JWTAuthProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", UID: "kc-uid", Generation: 1},
		Spec: jwtv1.JWTAuthProfileSpec{
			Issuer: "https://id.example/realms/inference", Audiences: []string{"inference-api"},
			RolesClaim: "realm_access.roles", LeewaySec: 10,
		},
	}
}

// harness - a Manager over the given profiles and peers, with its informer
// synced, its events kept and the names it reports changed collected.
type harness struct {
	m       *Manager
	client  *jwtfake.Clientset
	events  *eventLog
	changed []string
}

func newHarness(t *testing.T, profiles []*jwtv1.JWTAuthProfile, peers ...*api.LoxiClient) *harness {
	t.Helper()
	objects := make([]runtime.Object, 0, len(profiles))
	for _, p := range profiles {
		objects = append(objects, p)
	}
	client := jwtfake.NewSimpleClientset(objects...)
	factory := jwtinformers.NewSharedInformerFactory(client, 0)

	pool := api.NewLoxiClientPool()
	for _, peer := range peers {
		pool.AddLoxiClient(peer)
	}

	h := &harness{client: client, events: &eventLog{}}
	h.m = NewManager(k8sfake.NewSimpleClientset(), nil, client, factory, pool)
	h.m.recorder = h.events
	h.m.SetOnChange(func(name string) { h.changed = append(h.changed, name) })

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	if !cache.WaitForCacheSync(stop, h.m.synced) {
		t.Fatal("informer did not sync")
	}
	return h
}

func (h *harness) get(t *testing.T) *jwtv1.JWTAuthProfile {
	t.Helper()
	p, err := h.client.JwtauthprofileV1().JWTAuthProfiles().Get(context.Background(), "kc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get kc: %v", err)
	}
	return p
}

func programmedCondition(p *jwtv1.JWTAuthProfile) metav1.ConditionStatus {
	for _, c := range p.Status.Conditions {
		if c.Type == jwtv1.ConditionProgrammed {
			return c.Status
		}
	}
	return ""
}

// The profile goes to every gateway that enforces profiles, and the object
// says so: each peer, Programmed, and a finalizer for the delete.
func TestRegistersOnEveryPeer(t *testing.T) {
	a, b := newFakeGateway(t, api.FeatureJWTAuthProfile), newFakeGateway(t, api.FeatureJWTAuthProfile)
	peerA, peerB := a.client(t), b.client(t)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, peerA, peerB)

	if err := h.m.reconcile("kc"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, g := range []*fakeGateway{a, b} {
		if posts, _ := g.counts(); posts != 1 {
			t.Fatalf("a peer got %d posts, want 1", posts)
		}
		body := g.posted[0]
		if body["name"] != "kc" || body["issuer"] != "https://id.example/realms/inference" ||
			body["roles_claim"] != "realm_access.roles" || body["leeway_sec"] != float64(10) {
			t.Errorf("body = %v", body)
		}
	}
	p := h.get(t)
	if programmedCondition(p) != metav1.ConditionTrue || len(p.Status.Peers) != 2 ||
		!p.Status.Peers[0].Programmed || !p.Status.Peers[1].Programmed {
		t.Errorf("status = %+v", p.Status)
	}
	if len(p.Finalizers) != 1 || p.Finalizers[0] != jwtv1.FinalizerName {
		t.Errorf("finalizers = %v", p.Finalizers)
	}
	if exists, programmed := h.m.Programmed("kc", peerA.Host); !exists || !programmed {
		t.Errorf("Programmed(kc, a) = %v, %v", exists, programmed)
	}
	if strings.Join(h.changed, ",") != "kc" {
		t.Errorf("changes reported: %v, want kc once", h.changed)
	}
}

// A gateway that does not list the feature is never sent a rule naming a
// profile, so it gets no profile either; with only such peers, nothing is
// Programmed.
func TestPeerWithoutTheFeatureIsNotATarget(t *testing.T) {
	old := newFakeGateway(t, api.FeatureAPIKeyAuth)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, old.client(t))

	if err := h.m.reconcile("kc"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if posts, _ := old.counts(); posts != 0 {
		t.Errorf("a gateway without the feature got %d posts", posts)
	}
	if p := h.get(t); programmedCondition(p) != metav1.ConditionFalse || len(p.Status.Peers) != 0 {
		t.Errorf("status = %+v", p.Status)
	}
}

// A peer that refuses the profile is reported on it once, keeps being tried,
// and does not count as holding it.
func TestRefusalIsReportedOnceAndRetried(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	g.createStatus = http.StatusBadRequest
	peer := g.client(t)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, peer)

	for i := 0; i < 2; i++ {
		if err := h.m.reconcile("kc"); !errors.Is(err, errRetry) {
			t.Fatalf("reconcile = %v, want errRetry", err)
		}
	}
	if len(h.events.events) != 1 || !strings.Contains(h.events.events[0], ReasonProfileRefused) ||
		!strings.Contains(h.events.events[0], peer.Host) {
		t.Errorf("events = %q, want one %s naming the peer", h.events.events, ReasonProfileRefused)
	}
	if p := h.get(t); programmedCondition(p) != metav1.ConditionFalse || p.Status.Peers[0].LastError == "" {
		t.Errorf("status = %+v", p.Status)
	}
	if exists, programmed := h.m.Programmed("kc", peer.Host); !exists || programmed {
		t.Errorf("Programmed = %v, %v; want true, false", exists, programmed)
	}
}

// A peer that is down is left to its return rather than retried.
func TestPeerThatIsDownIsLeftForItsReturn(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	peer := g.client(t)
	peer.IsAlive = false
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, peer)

	if err := h.m.reconcile("kc"); err != nil {
		t.Fatalf("reconcile = %v, want nil: nothing to retry while the peer is down", err)
	}
	if posts, _ := g.counts(); posts != 0 {
		t.Errorf("a peer that is down got %d posts", posts)
	}
	if p := h.get(t); programmedCondition(p) != metav1.ConditionFalse {
		t.Errorf("status = %+v", p.Status)
	}
}

// A peer that comes back may have lost every profile: it no longer counts as
// holding them, and they are queued.
func TestPeerBackForgetsAndQueues(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	peer := g.client(t)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, peer)
	if err := h.m.reconcile("kc"); err != nil {
		t.Fatal(err)
	}

	h.m.PeerBack(peer)

	if _, programmed := h.m.Programmed("kc", peer.Host); programmed {
		t.Error("the returning peer still counts as holding the profile")
	}
	if h.m.queue.Len() != 1 {
		t.Errorf("%d queued, want kc", h.m.queue.Len())
	}
}

func TestMissingProfileDoesNotExist(t *testing.T) {
	h := newHarness(t, nil)
	if exists, programmed := h.m.Programmed("kc", "10.0.0.11"); exists || programmed {
		t.Errorf("Programmed = %v, %v for a profile with no object", exists, programmed)
	}
}

// Deleting a profile a rule still names waits for the rule; once the peers
// let it go, so does the finalizer.
func TestDeleteWaitsWhileARuleNamesIt(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	g.deleteStatus = http.StatusConflict
	peer := g.client(t)
	deleting := kc()
	now := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{jwtv1.FinalizerName}
	h := newHarness(t, []*jwtv1.JWTAuthProfile{deleting}, peer)

	if err := h.m.reconcile("kc"); !errors.Is(err, errRetry) {
		t.Fatalf("reconcile = %v, want errRetry while in use", err)
	}
	if len(h.events.events) != 1 || !strings.Contains(h.events.events[0], ReasonProfileInUse) {
		t.Errorf("events = %q, want one %s", h.events.events, ReasonProfileInUse)
	}
	if p := h.get(t); len(p.Finalizers) != 1 {
		t.Fatal("the finalizer went while a rule still names the profile")
	}

	g.mu.Lock()
	g.deleteStatus = 0
	g.mu.Unlock()
	if err := h.m.reconcile("kc"); err != nil {
		t.Fatalf("reconcile = %v once the rule went", err)
	}
	if p := h.get(t); len(p.Finalizers) != 0 {
		t.Errorf("finalizers = %v, want none", p.Finalizers)
	}
	if strings.Join(h.changed, ",") != "kc" {
		t.Errorf("changes reported: %v", h.changed)
	}
}

// Nothing changed, nothing written: the resync runs every minute.
func TestStatusIsNotRewrittenUnchanged(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, g.client(t))
	if err := h.m.reconcile("kc"); err != nil {
		t.Fatal(err)
	}

	// Run again from what was written, as the informer would hand it over.
	written := h.get(t)
	if err := h.m.client.JwtauthprofileV1().JWTAuthProfiles().Delete(context.Background(), "kc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, []*jwtv1.JWTAuthProfile{written}, g.client(t))
	h2.client.ClearActions()
	if err := h2.m.reconcile("kc"); err != nil {
		t.Fatal(err)
	}
	for _, action := range h2.client.Actions() {
		if action.GetVerb() == "update" {
			t.Errorf("unchanged profile written: %s %s", action.GetVerb(), action.(k8stesting.UpdateAction).GetSubresource())
		}
	}
}

// The finalizer, each status write and a peer coming up all wake a profile up
// again within moments; the generation just registered is not posted again.
func TestFreshRegistrationIsNotRepeated(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, g.client(t))

	for i := 0; i < 3; i++ {
		if err := h.m.reconcile("kc"); err != nil && !errors.Is(err, errRetry) {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if posts, _ := g.counts(); posts != 1 {
		t.Errorf("posted %d times, want once", posts)
	}
}

// A changed spec is a new generation, and goes out at once.
func TestNewGenerationIsRegisteredAgain(t *testing.T) {
	g := newFakeGateway(t, api.FeatureJWTAuthProfile)
	h := newHarness(t, []*jwtv1.JWTAuthProfile{kc()}, g.client(t))
	if err := h.m.reconcile("kc"); err != nil {
		t.Fatal(err)
	}

	changed := h.get(t).DeepCopy()
	changed.Spec.Audiences = []string{"other-api"}
	changed.Generation = 2
	if _, err := h.client.JwtauthprofileV1().JWTAuthProfiles().Update(context.Background(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := h.m.lister.Get("kc"); err == nil && p.Generation == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := h.m.reconcile("kc"); err != nil {
		t.Fatal(err)
	}
	if posts, _ := g.counts(); posts != 2 {
		t.Fatalf("posted %d times, want the new generation posted too", posts)
	}
	if aud := g.posted[1]["audiences"]; fmt.Sprint(aud) != "[other-api]" {
		t.Errorf("second post carried audiences %v", aud)
	}
}
