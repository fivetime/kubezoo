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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
