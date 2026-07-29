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

package controller

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclient "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-helpers/auth/rbac/reconciliation"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/rbac"
	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"

	"github.com/kubewharf/kubezoo/pkg/common"
)

// Names of the two shared RBAC objects. Unlike the per-tenant ClusterRole these
// are not tenant-specific: one ClusterRole is referenced from a RoleBinding
// inside each tenant namespace, and it is the binding's namespace -- not the
// role -- that confines what it grants.
const (
	tenantNamespaceAdminRole    = "kubezoo:tenant-namespace-admin"
	tenantNamespaceAdminBinding = "kubezoo:tenant-admin"
)

// tenantUser is the upstream identity kubezoo impersonates on behalf of a
// tenant. pkg/dynamic sets the impersonation headers on every request, so
// upstream RBAC is evaluated against this user and not against kubezoo's own
// credentials -- which is what makes any of this effective.
func tenantUser(tenantID string) string {
	return tenantID + "-admin"
}

// tenantClusterRole is the per-tenant ClusterRole holding the cluster-scoped
// half of a tenant's permissions.
//
// The name is historical: it used to grant "*" on "*" and deserved it. It is
// kept because renaming would leave the old ClusterRole and its binding behind,
// still granting cluster-admin, and the downgrade would silently not happen.
// Reconciling the same name with RemoveExtraPermissions narrows it in place.
func tenantClusterRole(tenantID string) string {
	return tenantID + "-cluster-admin"
}

// clusterScopedRules is the cluster-scoped half of what a tenant may do
// upstream: every cluster-scoped resource kubezoo actually serves, and nothing
// else. cmd/kubezoo/app has a test that fails if apigroups.go and this list
// drift apart.
//
// RBAC cannot bound these by name. resourceNames is an exact match with no
// prefix or wildcard form, and a tenant's cluster-scoped objects are
// distinguished from another tenant's only by a "<id>-" prefix that it cannot
// express. So for these resources the rewriting layer remains the only thing
// keeping tenants apart, exactly as before this change. What follows is
// therefore not a security boundary for cluster-scoped resources; it is one for
// the namespaced ones, which are the large majority.
//
// customresourcedefinitions is here even though apigroups.go does not declare
// it: CRDs reach upstream through the CRD handler rather than the generic
// proxy, and tenants do create them.
func clusterScopedRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		rbacv1helpers.NewRule("*").Groups("").Resources(
			"componentstatuses",
			"namespaces", "namespaces/finalize", "namespaces/status",
			"nodes", "nodes/proxy", "nodes/status",
			"persistentvolumes", "persistentvolumes/status",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("admissionregistration.k8s.io").Resources(
			"mutatingwebhookconfigurations", "validatingwebhookconfigurations",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("apiextensions.k8s.io").Resources(
			"customresourcedefinitions", "customresourcedefinitions/status",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("authentication.k8s.io").Resources(
			"tokenreviews",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("authorization.k8s.io").Resources(
			"selfsubjectaccessreviews", "selfsubjectrulesreviews", "subjectaccessreviews",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("networking.k8s.io").Resources(
			"ingressclasses",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("node.k8s.io").Resources(
			"runtimeclasses",
		).RuleOrDie(),
		rbacv1helpers.NewRule("*").Groups("rbac.authorization.k8s.io").Resources(
			"clusterroles", "clusterrolebindings",
		).RuleOrDie(),
	}
}

// syncClusterRoles reconciles the two ClusterRoles a tenant needs.
//
// Before this, a tenant was bound to a ClusterRole granting "*" on "*" plus
// every non-resource URL, so upstream RBAC allowed a tenant to reach any object
// in the cluster and the rewriting layer was the only thing that stopped them.
// Now the cluster-scoped grants are enumerated, and everything namespaced is
// granted per namespace instead (see syncNamespaceRoleBindings).
//
// Non-resource URLs are no longer granted at all. Discovery still works because
// Kubernetes binds system:discovery to system:authenticated out of the box.
func syncClusterRoles(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantID string) error {
	if _, err := rbacClient.ClusterRoles().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"}); err != nil {
		klog.Warningf("Failed to list the clusterroles %s with error %v", tenantID, err)
		return err
	}

	clusterRoles := []rbacv1.ClusterRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: tenantClusterRole(tenantID)},
			Rules:      clusterScopedRules(),
		},
		{
			// Shared by every tenant. Broad on purpose: a RoleBinding may only
			// grant inside its own namespace, so "*" on "*" here means "admin of
			// this one namespace" wherever it is bound. Keeping it broad also
			// means custom resources from tenant CRDs are covered, which an
			// aggregated role such as the built-in admin would miss.
			ObjectMeta: metav1.ObjectMeta{Name: tenantNamespaceAdminRole},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("*").Groups("*").Resources("*").RuleOrDie(),
			},
		},
		{
			// The tenant's copy of the built-in admin ClusterRole. Nothing here
			// binds it, which makes it look dead -- it is not. A tenant writing
			// a RoleBinding against ClusterRole "admin" has the reference
			// rewritten to "<id>-admin" by pkg/convert, so a role by that name
			// has to exist upstream or the binding dangles.
			//
			// Only admin is mirrored. A tenant referencing "edit" or "view" gets
			// a dangling "<id>-edit"; that gap predates this change.
			//
			// Reconciling an aggregated role is safe alongside
			// RemoveExtraPermissions: when both sides carry an AggregationRule,
			// reconciliation does not compute extra rules, so the aggregation
			// controller keeps ownership of the contents.
			ObjectMeta: metav1.ObjectMeta{Name: tenantID + "-admin"},
			AggregationRule: &rbacv1.AggregationRule{
				ClusterRoleSelectors: []metav1.LabelSelector{
					{MatchLabels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-admin": "true"}},
				},
			},
		},
	}

	for i := range clusterRoles {
		clusterRole := clusterRoles[i]
		opts := reconciliation.ReconcileRoleOptions{
			Role:    reconciliation.ClusterRoleRuleOwner{ClusterRole: &clusterRole},
			Client:  reconciliation.ClusterRoleModifier{Client: rbacClient.ClusterRoles()},
			Confirm: true,
			// Reconciliation is additive by default. Without this, narrowing the
			// rules above would leave the "*" on "*" already present on an
			// existing cluster untouched, and the downgrade would apply to new
			// tenants only while looking like it had applied to all of them.
			RemoveExtraPermissions: true,
		}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			result, err := opts.Run()
			if err != nil {
				return err
			}
			switch {
			case result.Protected && result.Operation != reconciliation.ReconcileNone:
				klog.Warningf("skipped reconcile-protected clusterrole.%s/%s with missing permissions: %v", rbac.GroupName, clusterRole.Name, result.MissingRules)
			case result.Operation == reconciliation.ReconcileUpdate:
				klog.V(2).Infof("updated clusterrole.%s/%s", rbac.GroupName, clusterRole.Name)
			case result.Operation == reconciliation.ReconcileCreate:
				klog.V(2).Infof("created clusterrole.%s/%s", rbac.GroupName, clusterRole.Name)
			}
			return nil
		})
		if err != nil {
			klog.Warningf("unable to reconcile clusterrole.%s/%s: %v", rbac.GroupName, clusterRole.Name, err)
			return err
		}
	}
	return nil
}

// syncClusterRoleBindings binds the tenant's upstream user to its cluster-scoped
// role. The namespaced half is bound per namespace, not here.
func syncClusterRoleBindings(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantId string) error {
	if _, err := rbacClient.ClusterRoleBindings().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"}); err != nil {
		klog.Warningf("Failed to list the clusterrolebindings %s with error %v", tenantId, err)
		return err
	}

	clusterRoleBindings := []rbacv1.ClusterRoleBinding{
		rbacv1helpers.NewClusterBinding(tenantClusterRole(tenantId)).Users(tenantUser(tenantId)).BindingOrDie(),
	}

	for i := range clusterRoleBindings {
		clusterRoleBinding := clusterRoleBindings[i]
		opts := reconciliation.ReconcileRoleBindingOptions{
			RoleBinding: reconciliation.ClusterRoleBindingAdapter{ClusterRoleBinding: &clusterRoleBinding},
			Client:      reconciliation.ClusterRoleBindingClientAdapter{Client: rbacClient.ClusterRoleBindings()},
			Confirm:     true,
		}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			result, err := opts.Run()
			if err != nil {
				return err
			}
			switch {
			case result.Protected && result.Operation != reconciliation.ReconcileNone:
				klog.Warningf("skipped reconcile-protected clusterrolebinding.%s/%s with missing subjects: %v", rbac.GroupName, clusterRoleBinding.Name, result.MissingSubjects)
			case result.Operation == reconciliation.ReconcileUpdate:
				klog.V(2).Infof("updated clusterrolebinding.%s/%s with additional subjects: %v", rbac.GroupName, clusterRoleBinding.Name, result.MissingSubjects)
			case result.Operation == reconciliation.ReconcileCreate:
				klog.V(2).Infof("created clusterrolebinding.%s/%s", rbac.GroupName, clusterRoleBinding.Name)
			case result.Operation == reconciliation.ReconcileRecreate:
				klog.V(2).Infof("recreated clusterrolebinding.%s/%s", rbac.GroupName, clusterRoleBinding.Name)
			}
			return nil
		})
		if err != nil {
			klog.Warningf("unable to reconcile clusterrolebinding.%s/%s: %v", rbac.GroupName, clusterRoleBinding.Name, err)
			return err
		}
	}
	return nil
}

// syncNamespaceRoleBindings puts a RoleBinding in each of the tenant's
// namespaces, which is what actually bounds the tenant: upstream now refuses a
// namespaced request outside them rather than relying on the rewriting layer to
// have addressed it correctly.
//
// The namespaces are found by the kubezoo.io/tenant label. Every tenant
// namespace carries it -- the four system ones because the controller creates
// them that way, and tenant-created ones because pkg/convert stamps it on the
// way through and refuses to let a tenant set it to another tenant's id.
func syncNamespaceRoleBindings(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantId string) error {
	selector := labels.SelectorFromSet(labels.Set{common.TenantNamespaceLabelKey: tenantId})
	namespaces, err := coreClient.Namespaces().List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("listing namespaces of tenant %s: %w", tenantId, err)
	}

	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		if ns.DeletionTimestamp != nil {
			continue
		}
		// Built by hand rather than with rbacv1helpers.NewRoleBinding, which
		// hardwires RoleRef.Kind to Role and takes the binding's name from the
		// role's. This one has to reference a ClusterRole from inside a
		// namespace, which is the whole trick: the role is shared, the binding
		// is what confines it.
		roleBinding := rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantNamespaceAdminBinding,
				Namespace: ns.Name,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     tenantNamespaceAdminRole,
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     tenantUser(tenantId),
			}},
		}

		opts := reconciliation.ReconcileRoleBindingOptions{
			RoleBinding: reconciliation.RoleBindingAdapter{RoleBinding: &roleBinding},
			Client:      reconciliation.RoleBindingClientAdapter{Client: rbacClient, NamespaceClient: coreClient.Namespaces()},
			Confirm:     true,
		}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			result, err := opts.Run()
			if err != nil {
				return err
			}
			switch result.Operation {
			case reconciliation.ReconcileCreate:
				klog.V(2).Infof("created rolebinding.%s/%s in %s", rbac.GroupName, roleBinding.Name, ns.Name)
			case reconciliation.ReconcileUpdate, reconciliation.ReconcileRecreate:
				klog.V(2).Infof("reconciled rolebinding.%s/%s in %s", rbac.GroupName, roleBinding.Name, ns.Name)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("reconciling rolebinding in namespace %s: %w", ns.Name, err)
		}
	}
	return nil
}

// ClusterScopedRulesForTest exposes the tenant's cluster-scoped grant so that
// cmd/kubezoo/app can check it against the resources apigroups.go serves. That
// comparison has to live there, since pkg cannot import cmd.
func ClusterScopedRulesForTest() []rbacv1.PolicyRule {
	return clusterScopedRules()
}
