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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

const (
	// DefaultNamespace is the platform namespace tenant resolvers live in.
	//
	// ⚠️ Not a tenant namespace, and not configurable to one: see Resolver.
	DefaultNamespace = "kubezoo-tenant-dns"
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
// ⛔ The Services it reads live in a PLATFORM namespace, never in the tenant's
// own <tid>-kube-system. A tenant holds namespaced write on every namespace
// kubezoo makes for it, so a resolver kept there would be one the tenant can
// repoint or delete -- and since the pods reading it are that same tenant's,
// the damage would be silent: names resolve to whatever it chose, and nothing
// upstream would call that an error.
//
// ⚠️ Keeping it out of reach is the whole reason for the extra namespace. The
// credential the resolver uses is that tenant's own, so a tenant reading it
// gains nothing -- the reason to move it is the WRITE, not the read.
type Resolver struct {
	store  cache.Store
	synced cache.InformerSynced
	// namespace is the platform namespace the resolver Services live in, and
	// with the Service being named for its tenant it makes the lookup a keyed
	// one rather than a scan of every tenant's entry on every pod create.
	namespace     string
	clusterDomain string
}

// New builds a Resolver over a label-selected Service informer's store.
func New(store cache.Store, synced cache.InformerSynced, namespace, clusterDomain string) *Resolver {
	return &Resolver{store: store, synced: synced, namespace: namespace, clusterDomain: clusterDomain}
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
	if r.synced != nil && !r.synced() {
		// ⚠️ Before sync, an empty store is indistinguishable from "this tenant
		// has no resolver". Answering not-ok is the same outcome either way, but
		// saying so here keeps a restart from looking like deprovisioning.
		klog.V(4).InfoS("tenant DNS informer not synced yet; pods keep the platform resolver",
			"tenant", tenantID)
		return convert.TenantDNS{}, false
	}
	obj, exists, err := r.store.GetByKey(r.namespace + "/" + tenantID)
	if err != nil || !exists {
		return convert.TenantDNS{}, false
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return convert.TenantDNS{}, false
	}
	if svc.Labels[TenantLabelKey] != tenantID {
		// ⚠️ The name said one tenant and the label says another. Rather than
		// trust either, refuse -- pointing a tenant's pods at another tenant's
		// resolver would let them read that tenant's service names, and it would
		// look like working DNS the whole time.
		klog.InfoS("tenant DNS Service name and tenant label disagree; pods keep the platform resolver",
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
	return convert.TenantDNS{Nameserver: svc.Spec.ClusterIP, ClusterDomain: r.clusterDomain}, true
}
