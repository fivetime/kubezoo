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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networking "k8s.io/kubernetes/pkg/apis/networking"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

func policyWithPeer(peer networking.NetworkPolicyPeer) *networking.NetworkPolicy {
	return &networking.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "team"},
		Spec: networking.NetworkPolicySpec{
			Ingress: []networking.NetworkPolicyIngressRule{{
				From: []networking.NetworkPolicyPeer{peer},
			}},
		},
	}
}

func peerOf(policy *networking.NetworkPolicy) *networking.NetworkPolicyPeer {
	return &policy.Spec.Ingress[0].From[0]
}

// TestEmptyNamespaceSelectorIsConfinedToTheTenant is the one that mattered.
//
// ⛔ `namespaceSelector: {}` means every namespace IN THE CLUSTER. A tenant
// writing it to narrow ingress to "my namespaces" was in fact opening its pods
// to every other tenant -- and nothing said so, because the policy was accepted
// and did exactly what it literally said.
func TestEmptyNamespaceSelectorIsConfinedToTheTenant(t *testing.T) {
	policy := policyWithPeer(networking.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{},
	})
	if _, err := NewNetworkPolicyTransformer().Forward(policy, "111111"); err != nil {
		t.Fatalf("converting: %v", err)
	}
	got := peerOf(policy).NamespaceSelector.MatchLabels
	if got[common.TenantNamespaceLabelKey] != "111111" {
		t.Errorf("the selector is %v; without the tenant label it reaches every namespace "+
			"in the cluster, including other tenants'", got)
	}
}

// TestNamespaceNameIsPrefixed -- the label is set by the apiserver itself, so a
// tenant's own namespace carries the PREFIXED name upstream. Naming `default`
// unprefixed selects the platform's namespace of that name.
func TestNamespaceNameIsPrefixed(t *testing.T) {
	policy := policyWithPeer(networking.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{corev1.LabelMetadataName: "default"},
		},
	})
	if _, err := NewNetworkPolicyTransformer().Forward(policy, "111111"); err != nil {
		t.Fatalf("converting: %v", err)
	}
	got := peerOf(policy).NamespaceSelector.MatchLabels
	if got[corev1.LabelMetadataName] != "111111-default" {
		t.Errorf("the namespace name is %q; unprefixed it names the platform's own",
			got[corev1.LabelMetadataName])
	}
	if got[common.TenantNamespaceLabelKey] != "111111" {
		t.Error("the tenant label is missing, so a namespace outside the tenant carrying " +
			"the same name label would still match")
	}
}

// TestNilNamespaceSelectorIsLeftAlone pins the case that must NOT be touched.
//
// ⚠️ A nil namespaceSelector means "this policy's own namespace", which is
// already the tenant's -- the namespace was prefixed on the way in. Adding the
// tenant label would widen it to "every namespace of mine", a different and
// larger rule than the tenant wrote.
func TestNilNamespaceSelectorIsLeftAlone(t *testing.T) {
	policy := policyWithPeer(networking.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	})
	before := policy.DeepCopy()
	if _, err := NewNetworkPolicyTransformer().Forward(policy, "111111"); err != nil {
		t.Fatalf("converting: %v", err)
	}
	if !reflect.DeepEqual(policy.Spec, before.Spec) {
		t.Error("a peer with no namespaceSelector was widened from its own namespace to " +
			"every namespace the tenant owns")
	}
}

// TestNetworkPolicyRoundTrips -- the tenant has to read back what it wrote, or
// its next apply re-sends a selector kubezoo then confines again, and the object
// it sees never matches the object it wrote.
func TestNetworkPolicyRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		what string
		peer networking.NetworkPolicyPeer
	}{
		{"an empty selector", networking.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{}}},
		{"a named namespace", networking.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: "default"}}}},
		{"a tenant's own label", networking.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"team": "payments"}}}},
		{"no namespace selector at all", networking.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}},
	} {
		policy := policyWithPeer(tc.peer)
		want := policy.DeepCopy()

		transformer := NewNetworkPolicyTransformer()
		if _, err := transformer.Forward(policy, "111111"); err != nil {
			t.Fatalf("%s: Forward: %v", tc.what, err)
		}
		if _, err := transformer.Backward(policy, "111111"); err != nil {
			t.Fatalf("%s: Backward: %v", tc.what, err)
		}
		if !reflect.DeepEqual(policy.Spec, want.Spec) {
			t.Errorf("%s: round trip gave\n  %+v\nwant\n  %+v",
				tc.what, policy.Spec.Ingress[0].From[0], want.Spec.Ingress[0].From[0])
		}
	}
}

// TestEgressPeersAreConfinedToo -- egress rules carry the same peer type, and a
// transformer that walked only ingress would leave half the object untranslated
// with nothing to show for it.
func TestEgressPeersAreConfinedToo(t *testing.T) {
	policy := &networking.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "team"},
		Spec: networking.NetworkPolicySpec{
			Egress: []networking.NetworkPolicyEgressRule{{
				To: []networking.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{},
				}},
			}},
		},
	}
	if _, err := NewNetworkPolicyTransformer().Forward(policy, "111111"); err != nil {
		t.Fatalf("converting: %v", err)
	}
	got := policy.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels
	if got[common.TenantNamespaceLabelKey] != "111111" {
		t.Errorf("an egress peer was left reaching every namespace: %v", got)
	}
}
