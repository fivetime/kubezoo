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

package common

const (
	TenantNamespaceLabelKey = "kubezoo.io/tenant"

	// TenantFrozenLabelKey marks a frozen tenant's namespaces so that a policy
	// in the upstream API server can refuse the tenant's remaining credentials.
	//
	// Withdrawing the RoleBindings kubezoo issued is not enough on its own: a
	// tenant that bound its own ServiceAccount keeps that binding, and its pods
	// reach upstream directly without passing through kubezoo at all. Measured
	// -- a frozen tenant's pod still listed and created objects. The label is
	// how the front door tells upstream which namespaces are frozen, since
	// upstream has no view of the Tenant object.
	TenantFrozenLabelKey = "kubezoo.io/frozen"

	TenantQuotaNamePrefix = "kubezoo-tenant-quota"

	// ProjectedClusterRoleBindingLabelKey marks the RoleBindings that carry a
	// tenant's ClusterRoleBinding.
	//
	// A tenant's cluster is its namespaces, so its cluster-wide binding is one
	// RoleBinding in each of them. The label is how those are told apart from
	// the tenant's own RoleBindings, which share the namespace and must not
	// appear when it lists ClusterRoleBindings -- nor the other way round.
	ProjectedClusterRoleBindingLabelKey = "kubezoo.io/clusterrolebinding"
)

// ReservedClusterRoleNames are the upstream names kubezoo keeps for itself, per
// tenant, in the RBAC group.
//
// They collide with what a tenant produces: names of cluster-scoped objects get
// the tenant prefix, so a tenant creating a ClusterRole called cluster-admin
// addresses <tid>-cluster-admin -- which is the very role the controller creates
// and binds cluster-wide to that tenant. Measured: with escalate granted, a
// tenant overwrote it with star-on-star and reached kube-system's and another
// tenant's secrets. Without escalate it can still delete it, which is self-harm
// the controller repairs, but the collision is what turns any future privilege
// on this path into an escape.
func ReservedClusterRoleNames(tenantID string) []string {
	return []string{
		tenantID + "-cluster-admin",
		tenantID + "-admin",
	}
}

// IsReservedClusterName reports whether an upstream name is one kubezoo manages
// for this tenant and the tenant must not write.
func IsReservedClusterName(tenantID, upstreamName string) bool {
	for _, reserved := range ReservedClusterRoleNames(tenantID) {
		if upstreamName == reserved {
			return true
		}
	}
	return false
}
