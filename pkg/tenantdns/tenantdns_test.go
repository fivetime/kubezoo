/*
Copyright 2024 The KubeZoo Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenantdns

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

func ns(tenantID string) string { return tenantID + "-kube-system" }

func resolverService(tenantID, clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: ResolverName, Namespace: ns(tenantID),
			Labels: map[string]string{ServiceLabelKey: "true", TenantLabelKey: tenantID},
		},
		Spec: corev1.ServiceSpec{ClusterIP: clusterIP},
	}
}

func readySlice(tenantID string, ready *bool, addresses ...string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: ResolverName + "-abcde", Namespace: ns(tenantID),
			Labels: map[string]string{discoveryv1.LabelServiceName: ResolverName},
		},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  addresses,
			Conditions: discoveryv1.EndpointConditions{Ready: ready},
		}},
	}
}

func resolverWith(svc *corev1.Service, slices ...*discoveryv1.EndpointSlice) *Resolver {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if svc != nil {
		_ = store.Add(svc)
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{ServiceNameIndex: IndexBySliceServiceName})
	for _, s := range slices {
		_ = indexer.Add(s)
	}
	synced := func() bool { return true }
	return New(store, synced, indexer, synced, "cluster.local")
}

// TestDeclaredButNotServingIsNotAnAnswer is the regression guard for a promise
// this package made and did not keep.
//
// ⛔ The lookup used to require only that the resolver Service EXIST. So a
// resolver that could not start -- measured on a live cluster, CoreDNS refusing
// to become ready because its credential was denied -- still had a Service with
// a ClusterIP, and the gateway wrote dnsPolicy: None plus that address into
// every pod the tenant created. dnsPolicy: None has no fallback, so those pods
// had NO name resolution at all: strictly worse than the platform resolver they
// would otherwise have kept.
//
// ⚠️ The fail-open the design promised covered "no resolver exists" and nothing
// else. Every other way a resolver stops working -- OOMKilled, evicted, an
// expired credential -- landed in the same hole.
func TestDeclaredButNotServingIsNotAnAnswer(t *testing.T) {
	cases := map[string]*Resolver{
		"no EndpointSlice at all": resolverWith(resolverService("111111", "10.0.0.5")),
		"endpoint not ready": resolverWith(resolverService("111111", "10.0.0.5"),
			readySlice("111111", ptr.To(false), "10.1.1.1")),
		"ready but no address": resolverWith(resolverService("111111", "10.0.0.5"),
			readySlice("111111", ptr.To(true))),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := r.For("111111"); ok {
				t.Error("a resolver that is not serving was handed to the injector; " +
					"its pods get dnsPolicy None pointed at an address that answers nothing, " +
					"and None has no fallback")
			}
		})
	}
}

// TestServingResolverIsUsed is the other half -- a readiness check that never
// says yes would disable the feature and look like the same green tests.
func TestServingResolverIsUsed(t *testing.T) {
	r := resolverWith(resolverService("111111", "10.0.0.5"),
		readySlice("111111", ptr.To(true), "10.1.1.1"))
	dns, ok := r.For("111111")
	if !ok {
		t.Fatal("a serving resolver was refused, which turns the feature off silently")
	}
	if dns.Nameserver != "10.0.0.5" || dns.ClusterDomain != "cluster.local" {
		t.Errorf("got %+v, want the Service ClusterIP and the configured domain", dns)
	}
}

// TestNilReadyMeansReady -- the API contract says an unset Ready is ready.
// Treating nil as not-ready would disable the feature wherever the endpoint
// controller leaves it unset, and it would look exactly like a broken resolver.
func TestNilReadyMeansReady(t *testing.T) {
	r := resolverWith(resolverService("111111", "10.0.0.5"),
		readySlice("111111", nil, "10.1.1.1"))
	if _, ok := r.For("111111"); !ok {
		t.Error("an endpoint with Ready unset was treated as not ready")
	}
}

// TestAnotherTenantsEndpointsDoNotCount -- the index is keyed by the Service
// name, which is the tenant id, so a mistake here would be one tenant's resolver
// being declared healthy on the strength of another's pods.
func TestAnotherTenantsEndpointsDoNotCount(t *testing.T) {
	r := resolverWith(resolverService("111111", "10.0.0.5"),
		readySlice("222222", ptr.To(true), "10.1.1.1"))
	if _, ok := r.For("111111"); ok {
		t.Error("another tenant's endpoints made this tenant's resolver look healthy")
	}
}

// TestUnsyncedInformersAnswerNothing -- before either cache has filled, an empty
// store is indistinguishable from "nothing is provisioned". Answering from it
// would inject a resolver on the strength of a cache that has not loaded, or
// refuse one that exists; both are decided on no information.
func TestUnsyncedInformersAnswerNothing(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	_ = store.Add(resolverService("111111", "10.0.0.5"))
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{ServiceNameIndex: IndexBySliceServiceName})
	_ = indexer.Add(readySlice("111111", ptr.To(true), "10.1.1.1"))

	notSynced := func() bool { return false }
	synced := func() bool { return true }
	if _, ok := New(store, synced, indexer, notSynced, "cluster.local").For("111111"); ok {
		t.Error("answered before the endpoint cache had synced")
	}
	if _, ok := New(store, notSynced, indexer, synced, "cluster.local").For("111111"); ok {
		t.Error("answered before the Service cache had synced")
	}
}

// TestNoEndpointIndexerDisablesTheFeature -- a Resolver built without the
// endpoint informer cannot tell serving from declared. Answering "serving" would
// restore the whole hole; this pins the safe direction.
func TestNoEndpointIndexerDisablesTheFeature(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	_ = store.Add(resolverService("111111", "10.0.0.5"))
	synced := func() bool { return true }
	if _, ok := New(store, synced, nil, nil, "cluster.local").For("111111"); ok {
		t.Error("a Resolver with no endpoint informer answered anyway")
	}
}

// TestATenantCannotClaimAnothersResolver is new with the move into tenant
// namespaces, and it guards the one thing that move put at risk.
//
// ⛔ The Service now sits somewhere the tenant can write, so the tenant label on
// it is attacker-controlled: a tenant can label its own resolver with a
// neighbour's id. What is NOT attacker-controlled is the namespace the object
// was found in, which kubezoo derives. For keys on the namespace and checks the
// label against it; if it ever trusted the label instead, one tenant's pods
// could be pointed at another tenant's resolver and would read that tenant's
// service names while looking like perfectly healthy DNS.
func TestATenantCannotClaimAnothersResolver(t *testing.T) {
	// 222222 labels its own resolver as if it belonged to 111111.
	impostor := resolverService("222222", "10.0.0.9")
	impostor.Labels[TenantLabelKey] = "111111"
	r := resolverWith(impostor, readySlice("222222", ptr.To(true), "10.1.1.9"))

	if dns, ok := r.For("111111"); ok {
		t.Errorf("111111 was handed %s, which lives in 222222's namespace; "+
			"the tenant label was trusted over the namespace", dns.Nameserver)
	}
	if _, ok := r.For("222222"); ok {
		t.Error("222222's own lookup succeeded on a resolver labelled for someone else; " +
			"the label check has to refuse a disagreement in both directions")
	}
}

// TestEachTenantsReadinessIsItsOwn -- every resolver Service now has the SAME
// name, so the EndpointSlice index returns every tenant's slices for one lookup.
// The namespace is the only thing separating them.
func TestEachTenantsReadinessIsItsOwn(t *testing.T) {
	r := resolverWith(resolverService("111111", "10.0.0.5"),
		readySlice("222222", ptr.To(true), "10.1.1.1"))
	if _, ok := r.For("111111"); ok {
		t.Error("222222's ready endpoints made 111111's resolver look healthy; " +
			"with a shared Service name the index alone cannot tell them apart")
	}
}

// TestPodsAreSentToAnAddressTheyCanReach guards the moment the resolver moves
// onto the tenant's own data plane.
//
// ⛔ This package's informer reads the upstream cluster directly, so it sees
// Services as STORED -- the read-path translation that shows a tenant the data
// plane's address never runs here. Returning spec.clusterIP was correct only
// while the resolver ran on the platform's own nodes.
//
// It stops being correct the moment the resolver becomes a capsule, which is
// the entire point of moving it: only there does its pod get an OVN address and
// become eligible as a load balancer member. Its Service then acquires a VIP on
// the tenant's network, and the upstream service-CIDR address stops being
// reachable from the pods that have to use it.
//
// ⛔ Getting this wrong is not a degradation. The injected spec carries
// dnsPolicy: None, which has NO fallback, so a nameserver the pod cannot reach
// means that pod has no name resolution at all -- strictly worse than never
// having injected anything.
func TestPodsAreSentToAnAddressTheyCanReach(t *testing.T) {
	svc := resolverService("111111", "254.51.215.104") // upstream service CIDR
	svc.Annotations = map[string]string{"kubezoo.io/cluster-ip": "192.168.200.12"}
	r := resolverWith(svc, readySlice("111111", ptr.To(true), "192.168.100.9"))

	dns, ok := r.For("111111")
	if !ok {
		t.Fatal("a serving resolver was refused")
	}
	if dns.Nameserver != "192.168.200.12" {
		t.Errorf("nameserver = %q, want the data plane's address; pods get dnsPolicy None "+
			"pointed at an address their network does not carry, and None has no fallback",
			dns.Nameserver)
	}
}

// TestWithoutATranslationTheStoredAddressStands -- a resolver on the platform's
// own nodes has no data-plane address and must keep working.
func TestWithoutATranslationTheStoredAddressStands(t *testing.T) {
	r := resolverWith(resolverService("111111", "10.0.0.5"),
		readySlice("111111", ptr.To(true), "10.1.1.1"))
	dns, ok := r.For("111111")
	if !ok || dns.Nameserver != "10.0.0.5" {
		t.Errorf("got %q,%v; want the stored address to stand when nothing translated it",
			dns.Nameserver, ok)
	}
}
