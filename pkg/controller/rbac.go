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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclient "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-helpers/auth/rbac/reconciliation"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/rbac"
	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"

	tenantv1alpha1 "github.com/kubewharf/kubezoo/pkg/apis/tenant/v1alpha1"
	"github.com/kubewharf/kubezoo/pkg/common"
)

// Names of the two shared RBAC objects. Unlike the per-tenant ClusterRole these
// are not tenant-specific: one ClusterRole is referenced from a RoleBinding
// inside each tenant namespace, and it is the binding's namespace -- not the
// role -- that confines what it grants.
const (
	tenantNamespaceAdminRole    = "kubezoo:tenant-namespace-admin"
	tenantNamespaceAdminBinding = "kubezoo:tenant-admin"
	// The read-only counterpart, bound in place of the admin role while a
	// tenant is suspended read-only. A separate role rather than a narrowed one,
	// because both are shared by every tenant and only the binding differs.
	tenantNamespaceReadOnlyRole = "kubezoo:tenant-namespace-readonly"
)

// readOnlyVerbs is what a read-only suspension leaves a tenant.
var readOnlyVerbs = []string{"get", "list", "watch"}

// namespaceRoleFor picks which shared ClusterRole the per-namespace binding
// should reference.
func namespaceRoleFor(mode tenantv1alpha1.TenantSuspensionMode) string {
	if mode == tenantv1alpha1.SuspensionReadOnly {
		return tenantNamespaceReadOnlyRole
	}
	return tenantNamespaceAdminRole
}

// narrowToReadOnly strips every verb but the reading ones from a rule set, so
// that a read-only suspension covers the cluster-scoped half of a tenant's
// permissions as well as the namespaced half. Rules left with no verbs are
// dropped, since a rule granting nothing is noise.
func narrowToReadOnly(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	narrowed := make([]rbacv1.PolicyRule, 0, len(rules))
	for _, rule := range rules {
		kept := make([]string, 0, len(readOnlyVerbs))
		for _, verb := range rule.Verbs {
			for _, allowed := range readOnlyVerbs {
				if verb == allowed || verb == "*" {
					kept = append(kept, allowed)
					break
				}
			}
		}
		if len(kept) == 0 {
			continue
		}
		rule.Verbs = kept
		narrowed = append(narrowed, rule)
	}
	return narrowed
}

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
		// Namespaces are the tenant's own, created through kubezoo, which
		// prefixes them. status and finalize belong to the namespace controller.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch", "delete").
			Groups("").Resources("namespaces").RuleOrDie(),

		// Read-only. Whether tenants should see nodes at all is a separate
		// question -- kubezoo shows them unconditionally today -- but nothing
		// gives a tenant a reason to write one.
		rbacv1helpers.NewRule("get", "list", "watch").
			Groups("").Resources("nodes", "componentstatuses").RuleOrDie(),

		// Prefixed by the convertor, so tenants cannot collide or reach each
		// other's.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("").Resources("persistentvolumes", "persistentvolumes/status").RuleOrDie(),

		// Confined by pkg/convert's webhook transformer, which forces the
		// namespace selector, the rule scope and the client config.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("admissionregistration.k8s.io").
			Resources("mutatingwebhookconfigurations", "validatingwebhookconfigurations").RuleOrDie(),

		// The tenant's own CRDs, group-prefixed by the CRD convertor.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("apiextensions.k8s.io").
			Resources("customresourcedefinitions", "customresourcedefinitions/status").RuleOrDie(),

		// Create-only APIs: they take a request and return an answer, and there
		// is nothing to list or delete.
		rbacv1helpers.NewRule("create").Groups("authentication.k8s.io").
			Resources("tokenreviews").RuleOrDie(),
		rbacv1helpers.NewRule("create").Groups("authorization.k8s.io").
			Resources("selfsubjectaccessreviews", "selfsubjectrulesreviews", "subjectaccessreviews").RuleOrDie(),

		// Cluster-scoped but name-prefixed like any other.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("networking.k8s.io").Resources("ingressclasses").RuleOrDie(),
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("node.k8s.io").Resources("runtimeclasses").RuleOrDie(),

		// Verbs are spelled out here rather than "*", and the two that are
		// missing are the point.
		//
		// "escalate" and "bind" exist precisely to permit privilege escalation:
		// RBAC normally refuses to let you create a role carrying permissions you
		// do not hold, and those verbs are the documented exemption. Granting
		// "*" grants them, and a tenant could then create a ClusterRole with "*"
		// on "*", bind its own upstream identity to it, and undo every bound in
		// this file. That was verified against a real cluster: with "*" the
		// tenant reached other tenants' secrets and kube-system; with the verbs
		// below it cannot create such a role at all.
		rbacv1helpers.NewRule("get", "list", "watch", "create", "update", "patch",
			"delete", "deletecollection").
			Groups("rbac.authorization.k8s.io").
			Resources("clusterroles", "clusterrolebindings").RuleOrDie(),
	}
}

// notGrantedToTenants names cluster-scoped resources kubezoo serves that tenants
// are deliberately not authorized for upstream, with the reason.
//
// The grant above used to be derived mechanically from what apigroups.go serves,
// which kept the two in step but inherited every questionable decision in the
// served surface. nodes/proxy is why that is not good enough: it was served, so
// it was granted, and a tenant could then reach the kubelet API on any node --
// verified, listing every pod on the node including kube-system's, and from
// there the logs and a shell in any container of any tenant. It is a proxy, so
// the rewriting layer never sees the path.
var notGrantedToTenants = map[string][]string{
	"": {
		// The escape above. Nothing a tenant does needs it.
		"nodes/proxy",
		// Writing node status is the kubelet's job.
		"nodes/status",
		// The namespace controller's, not a tenant's; finalize in particular
		// would let a tenant force a namespace through termination.
		"namespaces/status", "namespaces/finalize",
	},
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
func syncClusterRoles(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantID string,
	mode tenantv1alpha1.TenantSuspensionMode) error {
	if _, err := rbacClient.ClusterRoles().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"}); err != nil {
		klog.Warningf("Failed to list the clusterroles %s with error %v", tenantID, err)
		return err
	}

	// A read-only suspension narrows the cluster-scoped half too. Reconciling
	// with RemoveExtraPermissions rewrites the role in place, so lifting the
	// suspension restores it on the next pass.
	tenantRules := clusterScopedRules()
	if mode == tenantv1alpha1.SuspensionReadOnly {
		tenantRules = narrowToReadOnly(tenantRules)
	}

	clusterRoles := []rbacv1.ClusterRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: tenantClusterRole(tenantID)},
			Rules:      tenantRules,
		},
		{
			// The read-only counterpart of the namespace admin role below.
			// Always reconciled, whether or not anything is bound to it: a
			// suspension has to take effect immediately, not one resync after
			// the role is created.
			ObjectMeta: metav1.ObjectMeta{Name: tenantNamespaceReadOnlyRole},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule(readOnlyVerbs...).Groups("*").Resources("*").RuleOrDie(),
			},
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
func syncClusterRoleBindings(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantId string,
	mode tenantv1alpha1.TenantSuspensionMode) error {
	if _, err := rbacClient.ClusterRoleBindings().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"}); err != nil {
		klog.Warningf("Failed to list the clusterrolebindings %s with error %v", tenantId, err)
		return err
	}

	// Revocation withdraws the binding rather than narrowing it. The tenant is
	// then not a subject of anything kubezoo created, and upstream refuses it
	// outright.
	if mode == tenantv1alpha1.SuspensionRevoked {
		name := rbacv1helpers.NewClusterBinding(tenantClusterRole(tenantId)).Users(tenantUser(tenantId)).BindingOrDie().Name
		err := rbacClient.ClusterRoleBindings().Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("revoking clusterrolebinding %s: %w", name, err)
		}
		return nil
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
func syncNamespaceRoleBindings(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantId string,
	mode tenantv1alpha1.TenantSuspensionMode) error {
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
		if mode == tenantv1alpha1.SuspensionRevoked {
			err := rbacClient.RoleBindings(ns.Name).Delete(context.TODO(), tenantNamespaceAdminBinding, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("revoking rolebinding in namespace %s: %w", ns.Name, err)
			}
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
				// roleRef is immutable, so switching between the admin and
				// read-only roles cannot be an update. Reconciliation notices
				// and recreates the binding, which is the ReconcileRecreate
				// case handled below.
				Name: namespaceRoleFor(mode),
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

// NotGrantedToTenantsForTest exposes the deliberate exclusions so that
// cmd/kubezoo/app can tell them apart from an accidental gap.
func NotGrantedToTenantsForTest() map[string][]string {
	return notGrantedToTenants
}
