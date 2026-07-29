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

	authzinternal "k8s.io/kubernetes/pkg/apis/authorization"
)

// TestSelfSubjectAccessReviewAsksAboutTheTenantsNamespace is the defect that
// started this: `kubectl auth can-i create pods` answered no while the same
// create succeeded, because the question upstream named the platform's default
// namespace rather than the tenant's.
func TestSelfSubjectAccessReviewAsksAboutTheTenantsNamespace(t *testing.T) {
	review := &authzinternal.SelfSubjectAccessReview{
		Spec: authzinternal.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzinternal.ResourceAttributes{
				Namespace: "default", Verb: "create", Resource: "pods",
			},
		},
	}

	out, err := NewAccessReviewTransformer().Forward(review, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	got := out.(*authzinternal.SelfSubjectAccessReview).Spec.ResourceAttributes.Namespace
	if want := testTenant + "-default"; got != want {
		t.Errorf("namespace = %q, want %q; asking upstream about %q asks about the platform's "+
			"namespace of that name, and the answer is confidently wrong", got, want, got)
	}
}

// TestAccessReviewPrefixesCustomResourceGroupsOnly -- a tenant's custom
// resource group is prefixed upstream, a native group is not.
func TestAccessReviewPrefixesCustomResourceGroupsOnly(t *testing.T) {
	for _, tc := range []struct{ group, want string }{
		{"acme.io", testTenant + "-acme.io"},
		{"apps", "apps"},
		{"rbac.authorization.k8s.io", "rbac.authorization.k8s.io"},
		{"", ""},
	} {
		review := &authzinternal.SelfSubjectAccessReview{
			Spec: authzinternal.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authzinternal.ResourceAttributes{Group: tc.group, Resource: "x"},
			},
		}
		out, err := NewAccessReviewTransformer().Forward(review, testTenant)
		if err != nil {
			t.Fatalf("Forward(%q): %v", tc.group, err)
		}
		if got := out.(*authzinternal.SelfSubjectAccessReview).Spec.ResourceAttributes.Group; got != tc.want {
			t.Errorf("group %q -> %q, want %q", tc.group, got, tc.want)
		}
	}
}

// TestSubjectAccessReviewRefusesPlatformPrincipals: a SubjectAccessReview names
// an arbitrary subject, so left unconverted it reads the platform's RBAC rather
// than the tenant's.
func TestSubjectAccessReviewRefusesPlatformPrincipals(t *testing.T) {
	for _, subject := range []string{
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:generic-garbage-collector",
	} {
		review := &authzinternal.SubjectAccessReview{
			Spec: authzinternal.SubjectAccessReviewSpec{
				User:               subject,
				ResourceAttributes: &authzinternal.ResourceAttributes{Verb: "delete", Resource: "pods"},
			},
		}
		out, err := NewAccessReviewTransformer().Forward(review, testTenant)
		if err != nil {
			continue // refused, which is the point
		}
		// A service account can be moved into the tenant rather than refused,
		// but it must not stay pointed at the platform's namespace.
		got := out.(*authzinternal.SubjectAccessReview).Spec.User
		if got == subject {
			t.Errorf("subject %q was forwarded unchanged, so the tenant is asking about a "+
				"platform principal", subject)
		}
	}
}

// TestSubjectAccessReviewMovesServiceAccountsIntoTheTenant -- the legitimate
// case: an operator asking about its own service account.
func TestSubjectAccessReviewMovesServiceAccountsIntoTheTenant(t *testing.T) {
	review := &authzinternal.SubjectAccessReview{
		Spec: authzinternal.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:default:robot",
			Groups:             []string{"system:authenticated", "system:serviceaccounts:default"},
			ResourceAttributes: &authzinternal.ResourceAttributes{Namespace: "default", Verb: "get", Resource: "pods"},
		},
	}

	out, err := NewAccessReviewTransformer().Forward(review, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	spec := out.(*authzinternal.SubjectAccessReview).Spec
	if want := "system:serviceaccount:" + testTenant + "-default:robot"; spec.User != want {
		t.Errorf("user = %q, want %q", spec.User, want)
	}
	if spec.Groups[0] != "system:authenticated" {
		t.Errorf("system:authenticated was rewritten to %q; it names no tenant and prefixing it "+
			"only produces a group nobody is in", spec.Groups[0])
	}
	if want := "system:serviceaccounts:" + testTenant + "-default"; spec.Groups[1] != want {
		t.Errorf("group = %q, want %q", spec.Groups[1], want)
	}
}

// TestAccessReviewRoundTrip: clients read the spec back, so the question has to
// return in the terms it was asked in.
func TestAccessReviewRoundTrip(t *testing.T) {
	transformer := NewAccessReviewTransformer()
	review := &authzinternal.LocalSubjectAccessReview{
		Spec: authzinternal.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:default:robot",
			ResourceAttributes: &authzinternal.ResourceAttributes{Namespace: "default", Group: "acme.io", Resource: "widgets"},
		},
	}

	forward, err := transformer.Forward(review, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	back, err := transformer.Backward(forward, testTenant)
	if err != nil {
		t.Fatalf("Backward: %v", err)
	}
	spec := back.(*authzinternal.LocalSubjectAccessReview).Spec
	if spec.ResourceAttributes.Namespace != "default" ||
		spec.ResourceAttributes.Group != "acme.io" ||
		spec.User != "system:serviceaccount:default:robot" {
		t.Errorf("round trip did not restore the tenant's view: %+v", spec)
	}
}

// TestSelfSubjectRulesReviewIsConvertedBothWays -- `kubectl auth can-i --list`
// asks in a namespace and gets rules back naming groups and resource names.
func TestSelfSubjectRulesReviewIsConvertedBothWays(t *testing.T) {
	transformer := NewAccessReviewTransformer()
	review := &authzinternal.SelfSubjectRulesReview{
		Spec: authzinternal.SelfSubjectRulesReviewSpec{Namespace: "default"},
	}

	forward, err := transformer.Forward(review, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got := forward.(*authzinternal.SelfSubjectRulesReview).Spec.Namespace; got != testTenant+"-default" {
		t.Fatalf("namespace = %q, want %q", got, testTenant+"-default")
	}

	// Upstream answers with the rules it actually holds, in upstream terms.
	forward.(*authzinternal.SelfSubjectRulesReview).Status = authzinternal.SubjectRulesReviewStatus{
		ResourceRules: []authzinternal.ResourceRule{{
			Verbs:         []string{"get"},
			APIGroups:     []string{testTenant + "-acme.io"},
			Resources:     []string{"widgets"},
			ResourceNames: []string{testTenant + "-thing"},
		}},
	}
	back, err := transformer.Backward(forward, testTenant)
	if err != nil {
		t.Fatalf("Backward: %v", err)
	}
	status := back.(*authzinternal.SelfSubjectRulesReview).Status
	if status.ResourceRules[0].APIGroups[0] != "acme.io" ||
		status.ResourceRules[0].ResourceNames[0] != "thing" {
		t.Errorf("rules came back in upstream terms: %+v", status.ResourceRules[0])
	}
}
