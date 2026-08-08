/*
Copyright 2022 The KubeZoo Authors.

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
	"net"

	core "k8s.io/kubernetes/pkg/apis/core"
)

// NodePortsAreNew reports whether a Service is asking for node ports it did not
// already have.
//
// ⛔ A node is not something a tenant has. It cannot see the node inventory as
// an inventory, cannot choose where its pods land, and owns none of the machines
// -- so a port opened on EVERY node in the cluster is not a thing it can own
// either. Whoever reaches the node network reaches that port, with no tenancy in
// the path: not the tenant's own routing, not its NetworkPolicies, not the
// isolation the rest of this layer maintains. It is also a shared, finite
// range, first-come-first-served, so one tenant pinning 30080 takes it from
// everybody.
//
// That reason stands on its own. The one below is why refusing costs nothing,
// not why it is right.
//
// ⭐ Nothing needs them. kubetron, the data plane, deliberately forked away from
// the node:nodePort shape it inherited: an Octavia pool member is the backing
// pod's IP and L4 port directly (kubetron/pkg/service/members.go, "forked from
// CPO's buildBatchUpdateMemberOpts which was node:nodePort"). So a node port
// under this platform is exposure that buys nothing.
//
// ⚠️ Do not read kubetron's "ports" as these ports. A Neutron/OVN port is a
// network attachment -- the interface a pod is plugged into, inside the tenant's
// own network, with what may reach it governed by security groups the tenant
// administers in OpenStack through Horizon, on a control plane that is not this
// one. A node port is an L4 port number on one of the platform's machines,
// outside any tenant network, governed by no policy surface anywhere. The words
// collide; the things are opposites.
//
// ⭐ Same subset rule as ExternalIPsAreNew, for the same reason: a Service that
// already carries node ports -- allocated by upstream before this rule existed
// -- has to stay writable by its owner, who would otherwise be unable even to
// remove them. On a create, old is nil and every port is new.
//
// This covers only ports a tenant NAMES. Refusing the type outright is a
// separate decision and lives in the proxy, because it is about the Service, not
// about a field.
func NodePortsAreNew(svc, old *core.Service) bool {
	stored := map[int32]bool{}
	if old != nil {
		for _, p := range old.Spec.Ports {
			if p.NodePort != 0 {
				stored[p.NodePort] = true
			}
		}
	}
	for _, p := range svc.Spec.Ports {
		if p.NodePort != 0 && !stored[p.NodePort] {
			return true
		}
	}
	// healthCheckNodePort is the same shape and the same range, so it gets the
	// same rule rather than a second one.
	if svc.Spec.HealthCheckNodePort != 0 {
		if old == nil || old.Spec.HealthCheckNodePort != svc.Spec.HealthCheckNodePort {
			return true
		}
	}
	return false
}

// ExternalIPsAreNew reports whether a Service is claiming external IPs it was
// not already claiming.
//
// ⛔ A Service carrying spec.externalIPs makes the data plane on EVERY node
// intercept traffic to those addresses and deliver it to that Service's
// endpoints. Nothing checks that the writer has any claim to the address. A
// tenant can therefore take:
//
//   - another tenant's service, given its external IP;
//   - the platform's own -- the apiserver, DNS, a database, a registry;
//   - any address outside the cluster, so that every pod in the cluster talking
//     to it reaches the tenant's pods instead.
//
// This is CVE-2020-8554, and Kubernetes ships a mitigation for it:
// plugin/pkg/admission/network/denyserviceexternalips. ⚠️ That plugin denies the
// field to EVERYONE including the platform, which is exactly why the decision
// belongs here instead, where the writer's tenancy is known.
//
// ⭐ The rule is the upstream plugin's, not one invented here: allow when the new
// set is a SUBSET of the stored one. Keeping or dropping an address is fine;
// adding one is not. So a Service that already carries an address stays writable
// -- an outright refusal would leave it unwritable by its owner, who then cannot
// even remove the address.
//
// old is nil on a create, where every address is new by definition.
func ExternalIPsAreNew(svc, old *core.Service) bool {
	if len(svc.Spec.ExternalIPs) == 0 {
		return false
	}
	stored := map[string]bool{}
	if old != nil {
		for _, ip := range old.Spec.ExternalIPs {
			stored[ip] = true
		}
	}
	for _, ip := range svc.Spec.ExternalIPs {
		if !stored[ip] {
			return true
		}
	}
	return false
}

// ClusterIPAnnotation carries the address a tenant's own data plane can actually
// reach this Service at, and is what a tenant is shown as spec.clusterIP.
//
// ⛔ The problem it solves is measured, not theoretical. A tenant's pods run as
// Zun capsules on the tenant's own OVN network. The ClusterIP the upstream
// cluster allocates comes from that cluster's service CIDR, which does not exist
// on the tenant's network -- so the address kubezoo would otherwise report is one
// no tenant workload can dial. The address that works is the VIP the data plane
// allocates on the tenant's network for that Service's load balancer.
//
// ⭐ Named in kubezoo's own namespace rather than the data plane's, because
// kubezoo must not know that this deployment's data plane is Octavia. Anything
// that can allocate a reachable address for a Service can fill this in.
//
// ⛔ WRITTEN BY THE PLATFORM, READ BY THE TENANT -- the opposite direction from
// the annotation it resembles (lbipam.cilium.io/ips is a REQUEST, written by the
// user). A tenant that could set this could make kubezoo report an address of
// its choosing as its own ClusterIP, and the tenant's CoreDNS -- which reads
// Services back through kubezoo -- would answer with it. StripClusterIPAnnotation
// is what keeps the direction one-way.
const ClusterIPAnnotation = "kubezoo.io/cluster-ip"

// TenantClusterIP is the address to show a tenant for svc, and whether it
// differs from what upstream stored.
//
// ⚠️ A genuinely headless Service is left alone even if the annotation is
// present. "None" is the tenant's own words -- a StatefulSet's governing Service
// is the usual writer -- and overwriting it would turn per-pod DNS into a single
// address, silently, for a workload that specifically asked not to have one.
//
// ⚠️ Absent annotation reports the upstream address rather than nothing. The
// data plane fills this in shortly after the Service is created, and reporting
// an empty clusterIP in that window would invent a state stock Kubernetes never
// produces -- a ClusterIP Service with no address -- which client code and
// controllers have no branch for.
func TenantClusterIP(svc *core.Service) (string, bool) {
	if svc == nil {
		return "", false
	}
	return TenantClusterIPFrom(svc.Annotations, svc.Spec.ClusterIP)
}

// TenantClusterIPFrom is TenantClusterIP over the two fields it actually reads.
//
// ⚠️ Exists because the same decision has to be made on a *core.Service (the
// internal type, on the request path) and on a *corev1.Service (the external
// type, which is what an informer over the upstream cluster holds). Converting
// between them for this would be absurd, and writing the rule twice is how the
// two copies drift -- silently, because both answers are valid addresses.
func TenantClusterIPFrom(annotations map[string]string, clusterIP string) (string, bool) {
	if clusterIP == core.ClusterIPNone || clusterIP == "" {
		return "", false
	}
	address := annotations[ClusterIPAnnotation]
	if address == "" || net.ParseIP(address) == nil {
		return "", false
	}
	if address == clusterIP {
		return "", false
	}
	return address, true
}

// RestoreUpstreamClusterIP puts back the address upstream actually allocated,
// after a tenant submitted the one it was shown.
//
// ⭐ This is what makes `kubectl get svc -o yaml | kubectl apply -f -` work. The
// tenant is shown the data plane's address, so that is what it sends back; the
// upstream apiserver would see its own ClusterIP changing and refuse with
// "may not change once set".
//
// ⛔ Substituted rather than cleared. Clearing looks like the obvious fix and is
// not available: validateUpgradeDowngradeClusterIPs rejects an update whose
// primary clusterIP is unset with "primary clusterIP can not be unset"
// (k8s pkg/apis/core/validation/validation.go). The old object is therefore
// required, which is why this runs where the update paths already hold it.
//
// Returns false when the submitted address is neither the shown one nor the
// stored one -- a tenant naming some third address, which the caller refuses.
func RestoreUpstreamClusterIP(svc, old *core.Service) bool {
	if svc == nil || old == nil {
		return true
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == core.ClusterIPNone {
		// Unset or headless: not this function's business. An unset primary is
		// refused upstream and a headless-to-ClusterIP change is refused there
		// too, both with their own messages.
		return svc.Spec.ClusterIP == old.Spec.ClusterIP
	}
	if svc.Spec.ClusterIP == old.Spec.ClusterIP {
		return true
	}
	shown, translated := TenantClusterIP(old)
	if !translated || svc.Spec.ClusterIP != shown {
		return false
	}
	svc.Spec.ClusterIP = old.Spec.ClusterIP
	// ⚠️ clusterIPs travels with clusterIP and upstream validates them against
	// each other; translating one and not the other is a rejected write in the
	// dual-stack case and a silently inconsistent object in the single-stack one.
	if len(svc.Spec.ClusterIPs) > 0 {
		svc.Spec.ClusterIPs[0] = old.Spec.ClusterIP
	}
	return true
}
