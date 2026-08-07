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

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	core "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/utils/ptr"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// TenantDNS is where a tenant's pods should send their queries.
type TenantDNS struct {
	// Nameserver is the address of the tenant's own CoreDNS. It must be an IP:
	// spec.dnsConfig.nameservers is validated as one, so a name here is refused
	// upstream.
	Nameserver string
	// ClusterDomain is the zone that CoreDNS is authoritative for, normally
	// cluster.local. Carried per tenant rather than assumed, because the search
	// list built below is wrong in a silent way if it disagrees with the zone:
	// short names simply stop resolving, and nothing logs a reason.
	ClusterDomain string
}

// TenantDNSFunc answers where a tenant's CoreDNS is. Not-ok means the tenant has
// none yet, which is a normal state between the tenant being created and its
// CoreDNS being provisioned.
type TenantDNSFunc func(tenantID string) (TenantDNS, bool)

// ndotsForClusterFirst mirrors what kubelet writes for dnsPolicy: ClusterFirst.
// Pods are getting dnsPolicy: None instead, and None means kubelet contributes
// NOTHING -- no nameserver, no search list, no options -- so every value kubelet
// would have supplied has to be supplied here or the pod resolves worse than a
// normal one.
const ndotsForClusterFirst = "5"

// DNSConfigTransformer points a tenant's pods at the tenant's own CoreDNS, so
// that the names those pods resolve are the names the tenant can see.
//
// ⭐ The problem this exists for: a Service the tenant calls "rsvc" in its
// "default" namespace is stored upstream in "<tid>-default", and cluster DNS is
// built from where an object IS, not from what the tenant was told. So the
// working FQDN is rsvc.<tid>-default.svc.cluster.local -- the upstream name,
// leaked into the one place kubezoo's translation does not reach -- and the
// standard rsvc.default.svc.cluster.local resolves against the PLATFORM's own
// default namespace instead.
//
// ⛔ That second half is not merely unfriendly. The platform's default namespace
// holds the kubernetes Service, so a tenant workload written the ordinary way --
// kubernetes.default.svc.cluster.local, which is what nearly everything
// in-cluster reaches for -- lands on the upstream apiserver, going around
// kubezoo entirely. Measured on the lab cluster: it resolves, and on a node with
// normal Service routing it connects.
//
// ⚠️ One shared CoreDNS cannot fix this. The fix would need the same name to
// answer differently per asker, and CoreDNS answers by query name; client and
// node caches would cross tenants even if it did not. Hence a resolver per
// tenant, which makes "one name, many answers" no longer a contradiction --
// different resolvers, different data.
//
// The tenant's CoreDNS reads Services and EndpointSlices back OUT of kubezoo
// with the tenant's own credential, so the objects it builds records from
// already say "default". No zone rendering, no rewrite rules: the translation
// layer is already in the path. Verified against the lab cluster --
// probesvc.default.svc.cluster.local answers with the real ClusterIP, and the
// upstream-named probesvc.<tid>-default.svc.cluster.local is NXDOMAIN.
type DNSConfigTransformer struct {
	dnsFor TenantDNSFunc
}

var _ ObjectTransformer = &DNSConfigTransformer{}

// NewDNSConfigTransformer initiates a DNSConfigTransformer.
//
// A nil dnsFor disables injection entirely, which is what a deployment without
// per-tenant CoreDNS wants: pods keep the platform resolver and the behaviour
// that came before this.
func NewDNSConfigTransformer(dnsFor TenantDNSFunc) ObjectTransformer {
	return &DNSConfigTransformer{dnsFor: dnsFor}
}

// Forward points a workload's pods at the tenant's resolver.
func (t *DNSConfigTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	_, spec, err := podTemplateOf(obj)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		// A subresource carries no pod template.
		return obj, nil
	}
	if _, isPod := obj.(*core.Pod); isPod {
		// ⛔ A live Pod is pointed at its resolver on CREATE and never again.
		//
		// spec.dnsPolicy and spec.dnsConfig are not in the set of pod spec fields
		// an update may change, so writing them over a pod whose stored values
		// differ makes upstream refuse the update -- and then the tenant cannot
		// touch its own running pod at all. This is exactly the trap
		// saprojection.go hit with spec.volumes and placement.go hit with
		// spec.nodeSelector; the third time is not a coincidence, it is what
		// "the pod spec is mostly immutable" means in practice.
		//
		// tenantProxy.Create is the one caller that knows a write is a create,
		// and it calls SetPodDNS.
		return obj, nil
	}
	accessor, ok := obj.(metav1.Object)
	if !ok {
		return obj, nil
	}
	// The namespace on obj is already the upstream one: the default convertor
	// runs before every transformer.
	setTenantDNS(spec, util.TrimTenantIDPrefix(tenantID, accessor.GetNamespace()), t.dnsFor, tenantID)
	return obj, nil
}

// Backward leaves the injected DNS settings in what the tenant reads.
//
// ⚠️ Deliberate, and the opposite of what hiding a platform detail would
// suggest. Because the fields are immutable on a pod, a tenant that reads a pod
// and writes it back -- kubectl edit, an apply from a rendered manifest -- would
// send one whose dnsPolicy differs from the stored pod, and upstream would
// refuse it. Hiding the field would turn every read-modify-write into a failure.
//
// The same reasoning is why saprojection.go keeps its injected volume visible.
func (t *DNSConfigTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	return obj, nil
}

// SetPodDNS points a live pod at its tenant's resolver. CREATE only -- see
// Forward for why.
func SetPodDNS(pod *core.Pod, tenantID string, dnsFor TenantDNSFunc) {
	setTenantDNS(&pod.Spec, util.TrimTenantIDPrefix(tenantID, pod.Namespace), dnsFor, tenantID)
}

// setTenantDNS writes the whole resolver configuration, or none of it.
func setTenantDNS(spec *core.PodSpec, tenantNamespace string, dnsFor TenantDNSFunc, tenantID string) {
	if dnsFor == nil || tenantNamespace == "" {
		return
	}
	dns, ok := dnsFor(tenantID)
	if !ok || dns.Nameserver == "" || dns.ClusterDomain == "" {
		// ⭐ Fails OPEN, on purpose. Not-ok means the tenant's CoreDNS is not
		// there yet -- the window between a tenant being created and its
		// resolver being provisioned, or a deployment that runs without one.
		// Injecting dnsPolicy: None with no nameserver would give the pod NO
		// resolver whatsoever; leaving it alone gives it the platform's, which
		// is the behaviour that came before this and resolves the tenant's own
		// services under their upstream names.
		//
		// ⚠️ So the failure mode of a missing resolver is the OLD leak coming
		// back silently, which is why the provisioning side has to report a
		// tenant it never finished setting up rather than leaving it to be
		// noticed as a name that does not resolve.
		return
	}

	// ⭐ Wholesale, not merged. A tenant-supplied dnsConfig is an input to a
	// platform decision, and the rule everywhere else in kubezoo -- placement,
	// the SA namespace projection -- is that those are overwritten rather than
	// honoured. Merging would also let a tenant keep a nameserver of its own
	// choosing alongside this one, and the resolver a pod uses is precisely what
	// this is deciding.
	spec.DNSPolicy = core.DNSNone
	spec.DNSConfig = &core.PodDNSConfig{
		Nameservers: []string{dns.Nameserver},
		// The search list kubelet would have built for a pod in a namespace
		// called tenantNamespace. This is the half that makes short names work:
		// "rsvc" becomes rsvc.default.svc.cluster.local, which the tenant's own
		// CoreDNS answers, instead of rsvc.<tid>-default.svc.cluster.local.
		Searches: []string{
			tenantNamespace + ".svc." + dns.ClusterDomain,
			"svc." + dns.ClusterDomain,
			dns.ClusterDomain,
		},
		Options: []core.PodDNSConfigOption{
			{Name: "ndots", Value: ptr.To(ndotsForClusterFirst)},
		},
	}
}
