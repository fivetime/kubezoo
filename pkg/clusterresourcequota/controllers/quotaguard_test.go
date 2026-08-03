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

package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	quotav1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/quota/v1alpha1"
)

// TestQuotaWithoutOwnerIsSkippedNotDereferenced guards a denial of service any
// tenant could mount against every other tenant.
//
// ⚠️ A tenant is admin in its own namespaces, so it can create an ordinary
// ResourceQuota carrying the autoupdate label and NO ownerReferences.
// GetControllerOf then returns nil and the unguarded `owner.Kind` dereferenced
// it. The panic does not surface in the webhook handler: the upstream quota
// evaluator calls GetQuotas from a worker goroutine wrapped in
// runtime.HandleCrashWithContext, which re-panics by default, so the process
// dies. One replica plus failurePolicy: Fail meant every tenant's pod creation
// was refused while it was down, and the attacking tenant's own retrying
// ReplicaSet controller killed each restart.
//
// The check is one line; this test is here because the line is invisible.
func TestQuotaWithoutOwnerIsSkippedNotDereferenced(t *testing.T) {
	tenantWritten := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decoy",
			Namespace: "111111-default",
			Labels:    map[string]string{LabelClusterResourceQuotaAutoUpdate: "true"},
			// No ownerReferences: exactly what a tenant can write.
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := quotav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	accessor := &quotaAccessor{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantWritten).Build(),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a tenant-written ResourceQuota with no owner panicked GetQuotas: %v.\n"+
				"In production that panic happens on an evaluator worker goroutine under "+
				"HandleCrashWithContext, which re-panics, so the process dies and every "+
				"tenant's pod creation is refused while it is down", r)
		}
	}()

	quotas, err := accessor.GetQuotas("111111-default")
	if err != nil {
		t.Fatalf("GetQuotas: %v", err)
	}
	if len(quotas) != 0 {
		t.Errorf("a quota with no ClusterResourceQuota owner was enforced as one of ours: %v", quotas)
	}
}

// TestStrippedAutoupdateLabelIsRestored is a quota escape that costs a tenant
// one kubectl label.
//
// ⛔ The derived ResourceQuota carries two labels, read by two different
// components:
//
//	createdBy  -- how THIS reconciler finds the quota to repair
//	autoupdate -- how the admission webhook finds the quota to ENFORCE
//
// and the repair below compares spec only. A tenant is admin in its own
// namespaces -- tenantNamespaceAdminRole is "*" on "*", deliberately, so that
// custom resources from tenant CRDs are covered -- so it can edit that object.
// Removing just the autoupdate label leaves the reconciler still finding the
// quota by createdBy, still seeing an unchanged spec, and doing nothing, while
// the webhook now selects nothing at all.
//
// ⚠️ The per-namespace quota keeps being enforced by upstream's own admission,
// which does not read labels. What stops is kubezoo's TENANT-WIDE aggregate --
// so every namespace admits the full allowance independently and the tenant's
// real ceiling becomes allowance times namespaces.
//
// ⭐ The same rule this file already states elsewhere: never key enforcement on
// a label the tenant can write. It was applied to createdBy when the decoy-quota
// adoption was fixed, and not to autoupdate.
func TestStrippedAutoupdateLabelIsRestored(t *testing.T) {
	clusterQuota := &quotav1alpha1.ClusterResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "kubezoo-tenant-quota-111111"},
		Spec: quotav1alpha1.ClusterResourceQuotaSpec{
			ResourceQuotaSpec: corev1.ResourceQuotaSpec{},
		},
	}
	owned := metav1.NewControllerRef(clusterQuota,
		quotav1alpha1.SchemeGroupVersion.WithKind(ClusterResourceQuotaKind))

	// What a tenant leaves behind: createdBy intact, autoupdate gone, spec
	// untouched.
	stripped := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "111111-default",
			Name:      "kubezoo-tenant-quota-111111-abcde",
			Labels: map[string]string{
				quotav1alpha1.ClusterResourceQuotaCreatedby: clusterQuota.Name,
			},
			OwnerReferences: []metav1.OwnerReference{*owned},
		},
		Spec: clusterQuota.Spec.ResourceQuotaSpec,
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	if err := quotav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(clusterQuota, stripped).Build()

	reconciler := &ClusterResourceQuotaReconciler{Client: client, Logger: logr.Discard()}
	if _, err := reconciler.ensureResourceQuotaInNamespace(context.TODO(), clusterQuota,
		"111111-default", []*corev1.ResourceQuota{stripped}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	got := &corev1.ResourceQuota{}
	key := types.NamespacedName{Namespace: stripped.Namespace, Name: stripped.Name}
	if err := client.Get(context.TODO(), key, got); err != nil {
		t.Fatalf("reading the quota back: %v", err)
	}
	if got.Labels[LabelClusterResourceQuotaAutoUpdate] != "true" {
		t.Errorf("the autoupdate label is %q after reconciling, want \"true\" -- the webhook "+
			"selects on it, so this tenant's aggregate quota is no longer enforced and "+
			"nothing will put it back",
			got.Labels[LabelClusterResourceQuotaAutoUpdate])
	}
}
