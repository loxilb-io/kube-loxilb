package gatewayapi

import (
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	sigsfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	sigs_v1_lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// grantSpec - routes of kind fromKind in fromNamespace may refer to the
// Service toName ("" for any).
func grantSpec(fromKind v1.Kind, fromNamespace, toName string) v1.ReferenceGrantSpec {
	spec := v1.ReferenceGrantSpec{
		From: []v1.ReferenceGrantFrom{{Group: v1.GroupName, Kind: fromKind, Namespace: v1.Namespace(fromNamespace)}},
		To:   []v1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
	}
	if toName != "" {
		name := v1.ObjectName(toName)
		spec.To[0].Name = &name
	}
	return spec
}

func referenceGrant(namespace, name string, spec v1.ReferenceGrantSpec) *v1.ReferenceGrant {
	return &v1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: spec}
}

// staticGrants - ReferenceGrants that hold grants, as if watched and synced.
func staticGrants(grants ...*v1.ReferenceGrant) *ReferenceGrants {
	objs := make([]interface{}, 0, len(grants))
	for _, rg := range grants {
		objs = append(objs, rg)
	}
	lister := sigs_v1_lister.NewReferenceGrantLister(newIndexer(objs...))

	g := NewReferenceGrants()
	g.specs = func(namespace string) ([]v1.ReferenceGrantSpec, error) {
		list, err := lister.ReferenceGrants(namespace).List(labels.Everything())
		if err != nil {
			return nil, err
		}
		specs := make([]v1.ReferenceGrantSpec, 0, len(list))
		for _, rg := range list {
			specs = append(specs, rg.Spec)
		}
		return specs, nil
	}
	close(g.synced)
	return g
}

func routeRef(namespace string) grantReference {
	return grantReference{Group: v1.GroupName, Kind: kindHTTPRoute, Namespace: namespace}
}

func serviceRef(namespace, name string) grantReference {
	return grantReference{Group: "", Kind: "Service", Namespace: namespace, Name: name}
}

func TestReferenceGrantsPermits(t *testing.T) {
	both := grantSpec(kindHTTPRoute, "apps", "web")
	both.From = append(both.From, v1.ReferenceGrantFrom{Group: v1.GroupName, Kind: kindTCPRoute, Namespace: "tcp"})

	split := referenceGrant("backends", "split", v1.ReferenceGrantSpec{
		From: []v1.ReferenceGrantFrom{{Group: v1.GroupName, Kind: kindHTTPRoute, Namespace: "split"}},
		To:   []v1.ReferenceGrantTo{{Group: "", Kind: "Secret"}},
	})
	g := staticGrants(
		referenceGrant("backends", "web", both),
		referenceGrant("backends", "any", grantSpec(kindHTTPRoute, "shop", "")),
		// In the namespace the reference comes from, where it counts for
		// nothing.
		referenceGrant("misplaced", "wrong-side", grantSpec(kindHTTPRoute, "misplaced", "web")),
		split,
		referenceGrant("backends", "split-service", grantSpec(kindTCPRoute, "split", "")),
	)

	cases := []struct {
		name     string
		grants   *ReferenceGrants
		from, to grantReference
		want     bool
	}{
		{"within a namespace, without a grant", nil, routeRef("apps"), serviceRef("apps", "web"), true},
		{"across, without the grants", nil, routeRef("apps"), serviceRef("backends", "web"), false},
		{"across, before the grants are watched", NewReferenceGrants(), routeRef("apps"), serviceRef("backends", "web"), false},
		{"a grant for the Service", g, routeRef("apps"), serviceRef("backends", "web"), true},
		{"a grant for another Service", g, routeRef("apps"), serviceRef("backends", "db"), false},
		{"a grant for every Service", g, routeRef("shop"), serviceRef("backends", "db"), true},
		{"a grant for another namespace", g, routeRef("other"), serviceRef("backends", "web"), false},
		{"a grant for another kind", g, grantReference{Group: v1.GroupName, Kind: kindTCPRoute, Namespace: "apps"}, serviceRef("backends", "web"), false},
		{"another entry of the same grant", g, grantReference{Group: v1.GroupName, Kind: kindTCPRoute, Namespace: "tcp"}, serviceRef("backends", "web"), true},
		{"a grant in the referring namespace", g, routeRef("misplaced"), serviceRef("backends", "web"), false},
		// From in one grant and To in another do not make one.
		{"from and to in different grants", g, routeRef("split"), serviceRef("backends", "web"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.grants.Permits(tc.from, tc.to); got != tc.want {
				t.Errorf("Permits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReferenceGrantsNotifyBothVersions(t *testing.T) {
	g := NewReferenceGrants()
	var got []v1.Namespace
	g.AddHandler(func(spec v1.ReferenceGrantSpec) { got = append(got, spec.From[0].Namespace) })

	g.notifyObject(referenceGrant("backends", "v1", grantSpec(kindHTTPRoute, "a", "")))
	g.notifyObject(&v1beta1.ReferenceGrant{Spec: grantSpec(kindHTTPRoute, "b", "")})
	g.notifyObject(tombstone(referenceGrant("backends", "gone", grantSpec(kindHTTPRoute, "c", ""))))
	g.notifyObject("not a grant")

	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("handlers saw %v, want [a b c]", got)
	}
}

func grantCRD(versions ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: ReferenceGrantCRDName}}
	for _, v := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{Name: v, Served: true})
	}
	return crd
}

// The grants are watched at v1 where the CRD serves it, and at v1beta1 on a
// cluster with Gateway API older than v1.5, which serves nothing else.
func TestReferenceGrantsWatchTheServedVersion(t *testing.T) {
	cases := []struct {
		name     string
		versions []string
		grant    runtime.Object
	}{
		{"v1.5 and later", []string{"v1", "v1beta1"},
			referenceGrant("backends", "web", grantSpec(kindHTTPRoute, "apps", "web"))},
		{"before v1.5", []string{"v1beta1"},
			&v1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "backends", Name: "web"},
				Spec: grantSpec(kindHTTPRoute, "apps", "web")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)

			g := NewReferenceGrants()
			factory := externalversions.NewSharedInformerFactory(sigsfake.NewSimpleClientset(tc.grant), 0)
			go g.Run(stop, extfake.NewSimpleClientset(grantCRD(tc.versions...)), factory)

			select {
			case <-g.synced:
			case <-time.After(5 * time.Second):
				t.Fatal("the grants did not sync")
			}
			if !g.Permits(routeRef("apps"), serviceRef("backends", "web")) {
				t.Error("the grant was not read at the version the CRD serves")
			}
		})
	}
}

// A route manager starts even when the grants never sync - no CRD, or no
// permission to list them - and then serves no cross-namespace backend.
func TestReferenceGrantsWaitForSync(t *testing.T) {
	defer func(old time.Duration) { referenceGrantSyncTimeout = old }(referenceGrantSyncTimeout)
	referenceGrantSyncTimeout = 10 * time.Millisecond

	if !NewReferenceGrants().WaitForSync(make(chan struct{})) {
		t.Error("WaitForSync = false after the timeout, want the manager to start")
	}
	if !staticGrants().WaitForSync(make(chan struct{})) {
		t.Error("WaitForSync = false for synced grants")
	}

	stop := make(chan struct{})
	close(stop)
	referenceGrantSyncTimeout = time.Minute
	if NewReferenceGrants().WaitForSync(stop) {
		t.Error("WaitForSync = true after the stop channel closed")
	}
}
