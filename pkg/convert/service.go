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
