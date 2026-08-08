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

// Package tenantdns answers where a tenant's own CoreDNS is.
//
// See pkg/convert/dnsconfig.go for why a tenant has one at all.
package tenantdns

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

const (
	// ResolverName is what a tenant's resolver Service is called inside that
	// tenant's own kube-system.
	//
	// ⭐ kube-dns, which is what a stock cluster calls the CoreDNS Service -- the
	// tenant should see in its own kube-system what it would see anywhere else.
	// The workload behind it is called coredns, also as upstream does.
	//
	// ⚠️ Duplicated as TenantDNSServiceName in the controller's pkg/controller,
	// and nothing checks the two against each other -- separate repositories, so
	// no test can see both. A disagreement makes every lookup miss, and a miss
	// fails open, so the only symptom is tenants quietly going back to the
	// platform resolver. Same hazard as the two label keys below.
	ResolverName = "kube-dns"
	// DefaultClusterDomain is the zone tenant resolvers are authoritative for.
	// It has to agree with what the resolver is configured to serve; the two are
	// set from the same place by the provisioning side for that reason.
	DefaultClusterDomain = "cluster.local"

	// ServiceLabelKey marks the Services this package reads. The provisioning
	// side sets it.
	ServiceLabelKey = "kubezoo.io/tenant-dns"
	// TenantLabelKey carries which tenant a resolver serves. Read from the label
	// rather than parsed out of the Service name so that the two sides disagree
	// loudly -- a lookup miss -- instead of a rename quietly pointing a tenant's
	// pods at another tenant's resolver.
	TenantLabelKey = "kubezoo.io/tenant"
)

// Resolver maps a tenant to the address of its CoreDNS.
//
// ⭐ The Services it reads live in each tenant's OWN kube-system, because the
// tenant is billed for the pods behind them and a charge for something the payer
// cannot list is not a charge anyone can check.
//
// ⚠️ That means a tenant can delete or repoint its own resolver. The blast
// radius is that tenant: the pods reading it are its own, the reconciler puts
// the objects back, and this package refuses anything that is not actually
// serving, so a tenant that breaks its resolver falls back to the platform one
// rather than losing DNS. What it cannot do is reach another tenant.
//
// ⛔ The label cross-check in For is what enforces that last sentence, and it is
// no longer a formality: the Service now lives somewhere the tenant can write,
// so the tenant label on it is attacker-controlled. It is checked against the
// namespace the object was found in, which is not.
type Resolver struct {
	store  cache.Store
	synced cache.InformerSynced
	// endpoints answers whether a resolver is actually SERVING, not merely
	// declared. See For.
	endpoints     cache.Indexer
	endpointsSyn  cache.InformerSynced
	clusterDomain string
}

// New builds a Resolver over a label-selected Service informer's store and the
// EndpointSlices behind those Services.
func New(store cache.Store, synced cache.InformerSynced, endpoints cache.Indexer,
	endpointsSyn cache.InformerSynced, clusterDomain string) *Resolver {
	return &Resolver{
		store: store, synced: synced,
		endpoints: endpoints, endpointsSyn: endpointsSyn,
		clusterDomain: clusterDomain,
	}
}

// ResolverNamespace is where tenantID's resolver lives.
func ResolverNamespace(tenantID string) string { return tenantID + "-kube-system" }

// ServiceNameIndex is the index a Resolver needs on the EndpointSlice informer,
// so that finding a tenant's endpoints is a keyed lookup rather than a walk over
// every tenant's on every pod create.
const ServiceNameIndex = "kubezoo-tenant-dns-service-name"

// IndexBySliceServiceName is the index function for ServiceNameIndex.
func IndexBySliceServiceName(obj interface{}) ([]string, error) {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil, nil
	}
	name := slice.Labels[discoveryv1.LabelServiceName]
	if name == "" {
		return nil, nil
	}
	return []string{name}, nil
}

// serving reports whether anything is actually behind the tenant's resolver
// Service.
//
// ⛔ This is the check whose absence made the fail-open promise false. The
// lookup used to require only that the Service EXIST, so a resolver that could
// not start -- measured: CoreDNS refusing to become ready because its credential
// was denied -- still produced a Service with a ClusterIP, and the gateway wrote
// dnsPolicy: None and that address into every pod of the tenant. dnsPolicy: None
// has no fallback, so those pods had NO name resolution at all: strictly worse
// than the platform resolver they would otherwise have kept.
//
// ⚠️ The property this restores is not specific to that bug. A resolver that is
// OOMKilled, evicted, or whose credential lapses now takes its tenant back to
// the platform resolver instead of off a cliff.
func (r *Resolver) serving(tenantID string) bool {
	if r.endpoints == nil {
		// No endpoint informer configured. Answering "serving" would restore the
		// exact hole above; answering "not serving" disables the feature. The
		// latter is the safe direction and the constructor is the only place that
		// can get this wrong.
		return false
	}
	slices, err := r.endpoints.ByIndex(ServiceNameIndex, ResolverName)
	if err != nil {
		return false
	}
	for _, obj := range slices {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		// ⛔ The namespace is the tenant check. Every tenant's resolver Service
		// has the same name now, so the index returns all of them; matching on
		// the namespace is what keeps one tenant's readiness from vouching for
		// another's.
		if !ok || slice.Namespace != ResolverNamespace(tenantID) {
			continue
		}
		for i := range slice.Endpoints {
			// ⚠️ Ready being nil means ready, per the API contract. Treating nil
			// as not-ready would disable the feature wherever the endpoint
			// controller leaves it unset.
			if ptr.Deref(slice.Endpoints[i].Conditions.Ready, true) && len(slice.Endpoints[i].Addresses) > 0 {
				return true
			}
		}
	}
	return false
}

// Selector is the label selector the informer must be built with, so that the
// store holds tenant resolvers and nothing else.
func Selector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{ServiceLabelKey: "true"})
}

// For answers where tenantID's pods should send their queries.
//
// Not-ok every time the answer is not known for certain: before the informer
// has synced, and for a tenant whose resolver has not been provisioned yet.
// dnsconfig.go explains what a not-ok does -- the pod keeps the platform
// resolver, which is the behaviour that came before any of this.
func (r *Resolver) For(tenantID string) (convert.TenantDNS, bool) {
	if r == nil || r.store == nil || tenantID == "" {
		return convert.TenantDNS{}, false
	}
	if r.endpointsSyn != nil && !r.endpointsSyn() {
		return convert.TenantDNS{}, false
	}
	if r.synced != nil && !r.synced() {
		// ⚠️ Before sync, an empty store is indistinguishable from "this tenant
		// has no resolver". Answering not-ok is the same outcome either way, but
		// saying so here keeps a restart from looking like deprovisioning.
		klog.V(4).InfoS("tenant DNS informer not synced yet; pods keep the platform resolver",
			"tenant", tenantID)
		return convert.TenantDNS{}, false
	}
	obj, exists, err := r.store.GetByKey(ResolverNamespace(tenantID) + "/" + ResolverName)
	if err != nil || !exists {
		return convert.TenantDNS{}, false
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return convert.TenantDNS{}, false
	}
	if svc.Labels[TenantLabelKey] != tenantID {
		// ⚠️ The namespace said one tenant and the label says another. The
		// namespace is the trustworthy half -- kubezoo derives it -- and the
		// label now sits on an object in a namespace the tenant can write, so
		// this is the check that stops a tenant from labelling its own resolver
		// with a neighbour's id and seeing where that leads.
		klog.InfoS("tenant DNS Service namespace and tenant label disagree; pods keep the platform resolver",
			"tenant", tenantID, "labelled", svc.Labels[TenantLabelKey])
		return convert.TenantDNS{}, false
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		// A headless resolver Service has no address for a pod to point at.
		// Provisioning should never make one; refusing to answer beats writing
		// "None" into a pod's nameservers, which upstream rejects -- and that
		// would fail every pod create for the tenant, turning a DNS nicety into
		// an outage.
		klog.InfoS("tenant DNS Service has no usable ClusterIP; pods keep the platform resolver",
			"tenant", tenantID, "clusterIP", svc.Spec.ClusterIP)
		return convert.TenantDNS{}, false
	}
	if !r.serving(tenantID) {
		klog.InfoS("tenant resolver has no ready endpoint; pods keep the platform resolver",
			"tenant", tenantID, "service", ResolverNamespace(tenantID)+"/"+ResolverName)
		return convert.TenantDNS{}, false
	}
	return convert.TenantDNS{Nameserver: nameserverFor(svc), ClusterDomain: r.clusterDomain}, true
}

// nameserverFor picks the address a POD can dial, which is not always the one
// upstream allocated.
//
// ⛔ This informer reads the upstream cluster directly, so it sees Services as
// STORED -- the read-path translation that shows a tenant the data plane's
// address never runs here. Returning spec.clusterIP unconditionally was correct
// only for as long as the resolver ran on the platform's own nodes.
//
// It stops being correct the moment the resolver becomes a capsule, which is
// the point of moving it: only then does its pod get an OVN address and become
// eligible as a load balancer member. At that moment its Service acquires a VIP
// on the tenant's network, and the upstream service-CIDR address stops being
// reachable from the pods that have to use it.
//
// ⛔ Getting this wrong is not a degradation. The injected spec has
// dnsPolicy: None, which has NO fallback, so a nameserver the pod cannot reach
// means that pod has no name resolution at all -- worse than never having
// injected anything. That is why this reads the same annotation the read path
// does rather than assuming the two addresses agree.
func nameserverFor(svc *corev1.Service) string {
	if address, translated := convert.TenantClusterIPFrom(svc.Annotations, svc.Spec.ClusterIP); translated {
		return address
	}
	return svc.Spec.ClusterIP
}
