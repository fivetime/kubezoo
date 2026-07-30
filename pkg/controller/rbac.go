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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclient "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-helpers/auth/rbac/reconciliation"
	"sort"
	"strings"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/rbac"
	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"

	tenantv1alpha1 "github.com/kubewharf/kubezoo/pkg/apis/tenant/v1alpha1"
	"github.com/kubewharf/kubezoo/pkg/common"
	"github.com/kubewharf/kubezoo/pkg/util"
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
	// The role behind util.RoleAuthorGroup. Shared by every tenant, because what
	// it grants is not tenant-specific and it grants nothing on its own: escalate
	// is an exemption from a check, not a permission to write anything.
	tenantRoleAuthorRole    = "kubezoo:tenant-role-author"
	tenantRoleAuthorBinding = "kubezoo:tenant-role-author"
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

// isFreeze reports whether a mode withdraws the tenant's bindings outright.
//
// Anything set but unrecognised counts. Validation refuses unknown modes on the
// way in, so this only covers objects written before it existed, and a
// suspension that cannot be interpreted should hold rather than leave upstream
// RBAC at full admin while the front door refuses writes -- which is what the
// two layers used to do, disagreeing with each other.
func isFreeze(mode tenantv1alpha1.TenantSuspensionMode) bool {
	return mode != "" && mode != tenantv1alpha1.SuspensionReadOnly
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
	return util.TenantAdminUser(tenantID)
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
// ownCustomResourceRules grants the tenant, cluster-wide, everything in the API
// groups that are its own.
//
// Without this a tenant cannot create a ClusterRole mentioning its own custom
// resources. RBAC refuses to let anyone write a role carrying permissions they
// do not hold, and a tenant holds its rights per namespace, so the check fails
// even for a group only it can have anything in. That is what stops an operator
// chart from installing: measured with cert-manager, twenty-one refusals.
//
// Safe because the group name carries the tenant. A group called
// 111111-cert-manager.io can only ever contain tenant 111111's objects, since
// kubezoo prefixes a tenant's CRD groups on the way in, so cluster-wide here
// means "all of mine" rather than "everyone's".
//
// Enumerated rather than matched by prefix because RBAC has no prefix: an
// apiGroup is a literal or "*", and "111111-*" is neither. So the rules are
// rebuilt from the tenant's CRDs on every pass, and a CRD added or removed
// shows up on the next resync.
func ownCustomResourceRules(tenantID string, crdClient *apiextensions.Clientset) []rbacv1.PolicyRule {
	if crdClient == nil {
		return nil
	}
	crds, err := crdClient.ApiextensionsV1().CustomResourceDefinitions().List(
		context.TODO(), metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		klog.Warningf("could not read tenant %s's CRDs, so its ClusterRoles naming its own "+
			"custom resources will be refused until the next resync: %v", tenantID, err)
		return nil
	}
	prefix := tenantID + "-"
	seen := map[string]bool{}
	groups := make([]string, 0, len(crds.Items))
	for i := range crds.Items {
		group := crds.Items[i].Spec.Group
		if !strings.HasPrefix(group, prefix) || seen[group] {
			continue
		}
		seen[group] = true
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil
	}
	sort.Strings(groups)
	return []rbacv1.PolicyRule{
		rbacv1helpers.NewRule("*").Groups(groups...).Resources("*").RuleOrDie(),
	}
}

func syncClusterRoles(coreClient v1.CoreV1Interface, rbacClient rbacclient.RbacV1Interface, tenantID string,
	mode tenantv1alpha1.TenantSuspensionMode,
	crdClient *apiextensions.Clientset) error {
	if _, err := rbacClient.ClusterRoles().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"}); err != nil {
		klog.Warningf("Failed to list the clusterroles %s with error %v", tenantID, err)
		return err
	}

	// A read-only suspension narrows the cluster-scoped half too. Reconciling
	// with RemoveExtraPermissions rewrites the role in place, so lifting the
	// suspension restores it on the next pass.
	tenantRules := append(clusterScopedRules(), ownCustomResourceRules(tenantID, crdClient)...)
	if mode == tenantv1alpha1.SuspensionReadOnly {
		tenantRules = narrowToReadOnly(tenantRules)
	}

	clusterRoles := []rbacv1.ClusterRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: tenantClusterRole(tenantID)},
			Rules:      tenantRules,
		},
		{
			// Lets a tenant create the ClusterRoles an operator chart ships.
			//
			// RBAC refuses to let anyone write a role granting permissions they
			// do not hold, and it asks that at cluster scope, while a tenant
			// holds its namespaced permissions per namespace. So a ClusterRole
			// over pods or secrets is refused even though the tenant has both in
			// every namespace it owns. escalate is the documented exemption.
			//
			// The write verbs are not here: they come from the tenant's own role,
			// so this grants nothing by itself and a suspended tenant gains
			// nothing. And it covers clusterroles only -- binding one
			// cluster-wide still faces the check, which is what keeps a role the
			// tenant could not otherwise have written from taking effect outside
			// its own namespaces. See util.RoleAuthorGroup.
			ObjectMeta: metav1.ObjectMeta{Name: tenantRoleAuthorRole},
			Rules: []rbacv1.PolicyRule{
				rbacv1helpers.NewRule("escalate").Groups("rbac.authorization.k8s.io").
					Resources("clusterroles").RuleOrDie(),
			},
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

	// Freezing withdraws the binding rather than narrowing it. The tenant is
	// then not a subject of anything kubezoo created, and upstream refuses it
	// outright. Nothing is deleted but the binding itself, so lifting the
	// suspension puts it back on the next pass.
	if isFreeze(mode) {
		name := rbacv1helpers.NewClusterBinding(tenantClusterRole(tenantId)).
			Groups(util.ProxiedGroup(tenantId)).BindingOrDie().Name
		err := rbacClient.ClusterRoleBindings().Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("withdrawing clusterrolebinding %s to freeze the tenant: %w", name, err)
		}
		return nil
	}

	// The subject is a group kubezoo asserts when it forwards a request, not the
	// tenant's user. Cluster-scoped grants cannot be bounded by name -- RBAC has
	// no prefix form -- so held by the user they were a grant over every
	// tenant's cluster-scoped objects, usable by anything holding that identity.
	// Held by a group they exist only on a forwarded request. See
	// util.ProxiedGroup.
	clusterRoleBindings := []rbacv1.ClusterRoleBinding{
		rbacv1helpers.NewClusterBinding(tenantClusterRole(tenantId)).
			Groups(util.ProxiedGroup(tenantId)).BindingOrDie(),
		// Shared by every tenant, like the role it names, and reconciled here so
		// that a cluster with tenants on it has it without a separate install
		// step. Its subject is a group no tenant can present.
		{
			ObjectMeta: metav1.ObjectMeta{Name: tenantRoleAuthorBinding},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     tenantRoleAuthorRole,
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.GroupKind,
				Name:     util.RoleAuthorGroup,
			}},
		},
	}

	for i := range clusterRoleBindings {
		clusterRoleBinding := clusterRoleBindings[i]
		opts := reconciliation.ReconcileRoleBindingOptions{
			RoleBinding: reconciliation.ClusterRoleBindingAdapter{ClusterRoleBinding: &clusterRoleBinding},
			Client:      reconciliation.ClusterRoleBindingClientAdapter{Client: rbacClient.ClusterRoleBindings()},
			Confirm:     true,
			// Reconciliation only adds subjects by default. Without this an
			// existing cluster keeps the user subject alongside the new group,
			// so the permissions stay reachable without kubezoo and the change
			// applies to new tenants only while looking as though it applied to
			// all of them -- the same trap RemoveExtraPermissions covers for the
			// rules.
			RemoveExtraSubjects: true,
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

// markFrozen puts the frozen label on one of the tenant's namespaces, or takes
// it off again.
//
// This is the half of a freeze that reaches the tenant's own workloads.
// Withdrawing the RoleBindings kubezoo issued stops the tenant's kubectl, and
// nothing else: a tenant may bind its own ServiceAccount inside its namespace --
// RBAC permits it, since the tenant already holds those rights -- and the
// resulting pod talks to the upstream API server directly, never reaching
// kubezoo. A frozen tenant's pod was measured still listing and creating
// objects. The label lets a policy in the upstream API server refuse those
// credentials, which is the only place that sees both paths.
//
// Patched rather than updated so that a namespace changing underneath does not
// turn into a conflict, and skipped when already in the wanted state so a freeze
// does not rewrite every namespace on each resync.
func markFrozen(coreClient v1.CoreV1Interface, ns *corev1.Namespace, frozen bool) error {
	_, labelled := ns.Labels[common.TenantFrozenLabelKey]
	if labelled == frozen {
		return nil
	}
	value := "null"
	if frozen {
		value = strconv.Quote("true")
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%s:%s}}}`,
		strconv.Quote(common.TenantFrozenLabelKey), value)
	_, err := coreClient.Namespaces().Patch(context.TODO(), ns.Name, types.MergePatchType,
		[]byte(patch), metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marking namespace %s frozen=%v: %w", ns.Name, frozen, err)
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
		if err := markFrozen(coreClient, ns, isFreeze(mode)); err != nil {
			return err
		}
		if isFreeze(mode) {
			err := rbacClient.RoleBindings(ns.Name).Delete(context.TODO(), tenantNamespaceAdminBinding, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("withdrawing rolebinding in namespace %s to freeze the tenant: %w", ns.Name, err)
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
