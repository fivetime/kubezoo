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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	internal "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

// TestNamespaceCarriesPodSecurity is the whole point of stamping it here.
//
// ⭐ Pod Security Admission runs inside the apiserver, so a namespace carrying
// this label refuses hostNetwork, hostPID, privileged containers and hostPath
// with every webhook in the cluster gone. Stamping it on the way through kubezoo
// means nothing outside this process has to be alive for that to be true --
// which is what the Kyverno mutate that also stamps it could not promise, since
// a namespace created while its webhook was unregistered got no label at all.
func TestNamespaceCarriesPodSecurity(t *testing.T) {
	got, err := NewNamespaceTransformer().Forward(
		&internal.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "111111-team"}}, "111111")
	if err != nil {
		t.Fatalf("converting a plain namespace: %v", err)
	}
	labels := got.(*internal.Namespace).Labels
	if labels[common.PodSecurityEnforceLabelKey] != common.PodSecurityLevel {
		t.Errorf("enforce label is %q, want %q -- without it the namespace has no "+
			"webhook-free protection at all",
			labels[common.PodSecurityEnforceLabelKey], common.PodSecurityLevel)
	}
	if labels[common.PodSecurityEnforceVersionLabelKey] != common.PodSecurityVersion {
		t.Errorf("enforce-version label is %q, want %q",
			labels[common.PodSecurityEnforceVersionLabelKey], common.PodSecurityVersion)
	}
	// The label that was already here must survive.
	if labels[common.TenantNamespaceLabelKey] != "111111" {
		t.Errorf("the tenant label was lost: %q", labels[common.TenantNamespaceLabelKey])
	}
}

// TestTenantCannotWeakenPodSecurity is the escape this closes. A tenant owns its
// namespaces and can label them; one setting enforce: privileged on its own is
// why config/policy/README.md exists.
func TestTenantCannotWeakenPodSecurity(t *testing.T) {
	for _, weaker := range []string{"privileged", "baseline", ""} {
		got, err := NewNamespaceTransformer().Forward(&internal.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "111111-team",
				Labels: map[string]string{
					common.PodSecurityEnforceLabelKey:        weaker,
					common.PodSecurityEnforceVersionLabelKey: "v1.24",
				},
			},
		}, "111111")
		if err != nil {
			t.Fatalf("enforce=%q: %v", weaker, err)
		}
		labels := got.(*internal.Namespace).Labels
		if labels[common.PodSecurityEnforceLabelKey] != common.PodSecurityLevel {
			t.Errorf("a tenant asking for enforce=%q got %q, want it overwritten with %q",
				weaker, labels[common.PodSecurityEnforceLabelKey], common.PodSecurityLevel)
		}
		if labels[common.PodSecurityEnforceVersionLabelKey] != common.PodSecurityVersion {
			t.Errorf("a tenant pinning an old enforce-version kept it: %q",
				labels[common.PodSecurityEnforceVersionLabelKey])
		}
	}
}

// TestWeakenedNamespaceIsRepairedNotRejected pins the choice of overwriting over
// refusing.
//
// ⚠️ Refusing reads stricter and is worse: a namespace that DID end up weaker --
// written during the very outage this hardening is for -- would become one its
// tenant could never write to again, fixable only by an administrator. This is
// the shape of that repair: an ordinary later write puts the level back.
func TestWeakenedNamespaceIsRepairedNotRejected(t *testing.T) {
	stored := &internal.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "111111-team",
		Labels: map[string]string{
			common.TenantNamespaceLabelKey:    "111111",
			common.PodSecurityEnforceLabelKey: "privileged",
		},
	}}
	// Whatever else the tenant was doing to the namespace.
	stored.Annotations = map[string]string{"team": "payments"}

	got, err := NewNamespaceTransformer().Forward(stored, "111111")
	if err != nil {
		t.Fatalf("a later write to a weakened namespace was rejected, "+
			"leaving the tenant unable to repair it: %v", err)
	}
	if got.(*internal.Namespace).Labels[common.PodSecurityEnforceLabelKey] != common.PodSecurityLevel {
		t.Error("the weakened level survived a write that should have repaired it")
	}
}
