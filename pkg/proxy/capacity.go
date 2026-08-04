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

package proxy

import (
	"k8s.io/klog"

	tenantv1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/tenant/v1alpha1"
	tenantlister "github.com/fivetime/kubezoo-contract/pkg/generated/listers/tenant/v1alpha1"
)

// capacityOf resolves one of a tenant's caps: what the Tenant object says, or
// the platform's default when it says nothing.
//
// ⛔ The object first, and that ordering is the point. A cap on a flag can only
// be changed by restarting the gateway -- a single-replica StatefulSet, so every
// tenant's API drops and every tenant's operator watches break in order to give
// ONE tenant a larger number. The same reasoning already moved published storage
// classes off flags and onto labels.
//
// ⚠️ Fails to the DEFAULT, never to unlimited. A lister miss is a local fault:
// the tenant may well have a raised cap that this cannot see, and refusing on
// the default is the conservative reading -- it can be retried, whereas a write
// wrongly allowed cannot be taken back.
func capacityOf(tenants tenantlister.TenantLister, tenantID string, fallback int,
	pick func(*tenantv1alpha1.TenantCapacity) *int32) int {

	if tenants == nil || tenantID == "" {
		return fallback
	}
	tenant, err := tenants.Get(tenantID)
	if err != nil {
		klog.V(4).Infof("tenant %s: reading its capacity, falling back to the platform default: %v",
			tenantID, err)
		return fallback
	}
	if tenant.Spec.Capacity == nil {
		return fallback
	}
	value := pick(tenant.Spec.Capacity)
	if value == nil {
		return fallback
	}
	// ⚠️ Negative is not "unlimited" by accident. Zero already means that, and a
	// negative number is a typo -- reading it as no limit would turn a mistake
	// into the widest possible setting.
	if *value < 0 {
		klog.Warningf("tenant %s: capacity %d is negative, which is not a limit; using the "+
			"platform default %d", tenantID, *value, fallback)
		return fallback
	}
	return int(*value)
}

func maxNamespacesFor(tenants tenantlister.TenantLister, tenantID string, fallback int) int {
	return capacityOf(tenants, tenantID, fallback,
		func(c *tenantv1alpha1.TenantCapacity) *int32 { return c.MaxNamespaces })
}

func maxCRDsFor(tenants tenantlister.TenantLister, tenantID string, fallback int) int {
	return capacityOf(tenants, tenantID, fallback,
		func(c *tenantv1alpha1.TenantCapacity) *int32 { return c.MaxCRDs })
}

func maxClusterRoleBindingsFor(tenants tenantlister.TenantLister, tenantID string, fallback int) int {
	return capacityOf(tenants, tenantID, fallback,
		func(c *tenantv1alpha1.TenantCapacity) *int32 { return c.MaxClusterRoleBindings })
}
