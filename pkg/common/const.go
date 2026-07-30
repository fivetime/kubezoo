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
)
