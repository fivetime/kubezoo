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
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	sa "k8s.io/apiserver/pkg/authentication/serviceaccount"
	authzinternal "k8s.io/kubernetes/pkg/apis/authorization"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// AccessReviewTransformer converts the question an access review asks, so that
// it is asked about the tenant's objects rather than the platform's.
//
// An access review is a question, and every part of the question names things
// in the tenant's own namespace of names: the namespace, the API group of a
// custom resource, the subject. Forwarded as written, the question upstream is
// a different question -- "can this tenant create pods in the platform's
// default namespace" rather than in its own -- and its answer is confidently
// wrong. `kubectl auth can-i create pods` answered no while the very same
// create succeeded.
//
// The wrong answer is not only a nuisance. A SubjectAccessReview names an
// arbitrary subject, so unconverted it also reads the platform's RBAC: a tenant
// could ask what the platform's own users may do.
//
// This was invisible while tenants held "*" on "*" upstream, because every
// namespace answered yes. Tightening the grant is what made it show.
type AccessReviewTransformer struct{}

var _ ObjectTransformer = &AccessReviewTransformer{}

// NewAccessReviewTransformer initiates a transformer for the access review
// kinds in authorization.k8s.io.
func NewAccessReviewTransformer() ObjectTransformer {
	return &AccessReviewTransformer{}
}

// Forward transforms tenant object reference to upstream object reference.
func (t *AccessReviewTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	switch review := obj.(type) {
	case *authzinternal.SelfSubjectAccessReview:
		forwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		return review, nil
	case *authzinternal.LocalSubjectAccessReview:
		forwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		if err := forwardSubject(&review.Spec, tenantID); err != nil {
			return nil, err
		}
		return review, nil
	case *authzinternal.SubjectAccessReview:
		forwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		if err := forwardSubject(&review.Spec, tenantID); err != nil {
			return nil, err
		}
		return review, nil
	case *authzinternal.SelfSubjectRulesReview:
		if len(review.Spec.Namespace) > 0 {
			review.Spec.Namespace = util.UpstreamNamespace(tenantID, review.Spec.Namespace)
		}
		return review, nil
	default:
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of an access review")
	}
}

// Backward transforms upstream object reference to tenant object reference.
//
// Clients read the spec back -- kubectl prints it on a denial -- so the
// question has to come back in the terms it was asked in.
func (t *AccessReviewTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	switch review := obj.(type) {
	case *authzinternal.SelfSubjectAccessReview:
		backwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		return review, nil
	case *authzinternal.LocalSubjectAccessReview:
		backwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		backwardSubject(&review.Spec, tenantID)
		return review, nil
	case *authzinternal.SubjectAccessReview:
		backwardResourceAttributes(review.Spec.ResourceAttributes, tenantID)
		backwardSubject(&review.Spec, tenantID)
		return review, nil
	case *authzinternal.SelfSubjectRulesReview:
		review.Spec.Namespace = util.TrimTenantIDPrefix(tenantID, review.Spec.Namespace)
		backwardRules(review.Status.ResourceRules, tenantID)
		return review, nil
	default:
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of an access review")
	}
}

// forwardResourceAttributes rewrites the parts of the question that name
// tenant objects.
//
// Name is deliberately left alone. Only cluster-scoped names carry the tenant
// prefix, and telling those apart here would need the kind, which
// resourceAttributes does not carry -- it has the plural resource. It costs
// nothing today: a tenant's upstream grants never use resourceNames, because
// resourceNames matches exactly and cannot express a prefix, so an answer about
// a named object is the same as the answer without the name.
func forwardResourceAttributes(attributes *authzinternal.ResourceAttributes, tenantID string) {
	if attributes == nil {
		return
	}
	if len(attributes.Namespace) > 0 {
		// ⚠️ Idempotent, and it has to be. An in-cluster client reads its own
		// namespace from the projected serviceaccount/namespace file, which
		// carries the UPSTREAM spelling -- so a SelfSubjectAccessReview from an
		// operator asking about its own namespace arrived already prefixed, and
		// the unconditional concatenation made it <tid>-<tid>-default. Upstream
		// RBAC is genuinely per-namespace now, so that answers allowed:false with
		// 200 OK and no error: the operator concludes it lacks a permission it
		// actually has and switches the feature off. This transformer's own doc
		// comment says it exists because "can-i create pods" answered no while
		// the create succeeded; that was fixed for the tenant's spelling and left
		// broken for the upstream one.
		attributes.Namespace = util.UpstreamNamespace(tenantID, attributes.Namespace)
	}
	if len(attributes.Group) > 0 && !util.IsNativeAPIGroup(attributes.Group) {
		attributes.Group = util.AddTenantIDPrefix(tenantID, attributes.Group)
	}
}

func backwardResourceAttributes(attributes *authzinternal.ResourceAttributes, tenantID string) {
	if attributes == nil {
		return
	}
	attributes.Namespace = util.TrimTenantIDPrefix(tenantID, attributes.Namespace)
	attributes.Group = util.TrimTenantIDPrefix(tenantID, attributes.Group)
}

// forwardSubject moves the subject of the question into the tenant's identity
// space, and refuses the ones that cannot be moved there.
//
// A tenant's service accounts live in its prefixed namespaces and its own user
// is the prefixed name, so those convert. A bare "system:" principal is the
// platform's, names nothing the tenant owns, and asking about it is a read of
// the platform's RBAC -- so it is refused rather than passed through.
func forwardSubject(spec *authzinternal.SubjectAccessReviewSpec, tenantID string) error {
	if len(spec.User) > 0 {
		converted, err := forwardUsername(spec.User, tenantID)
		if err != nil {
			return err
		}
		spec.User = converted
	}
	for i, group := range spec.Groups {
		converted, err := forwardGroup(group, tenantID)
		if err != nil {
			return err
		}
		spec.Groups[i] = converted
	}
	return nil
}

func backwardSubject(spec *authzinternal.SubjectAccessReviewSpec, tenantID string) {
	spec.User = backwardUsername(spec.User, tenantID)
	for i := range spec.Groups {
		spec.Groups[i] = backwardGroup(spec.Groups[i], tenantID)
	}
}

func forwardUsername(username, tenantID string) (string, error) {
	if namespace, name, err := sa.SplitUsername(username); err == nil {
		return sa.MakeUsername(util.UpstreamNamespace(tenantID, namespace), name), nil
	}
	if strings.HasPrefix(username, "system:") {
		return "", errors.Errorf("user %q belongs to the cluster itself, not to tenant %s; "+
			"an access review may only ask about the tenant's own users and service accounts",
			username, tenantID)
	}
	return util.AddTenantIDPrefix(tenantID, username), nil
}

func backwardUsername(username, tenantID string) string {
	if namespace, name, err := sa.SplitUsername(username); err == nil {
		return sa.MakeUsername(util.TrimTenantIDPrefix(tenantID, namespace), name)
	}
	return util.TrimTenantIDPrefix(tenantID, username)
}

// forwardGroup converts a group the same way, with one carve-out: the two
// groups every authenticated request carries say nothing about which tenant the
// subject belongs to, so prefixing them would only produce a group nobody is in.
func forwardGroup(group, tenantID string) (string, error) {
	switch group {
	case "system:authenticated", "system:unauthenticated", "system:serviceaccounts":
		return group, nil
	}
	if namespace, ok := serviceAccountsGroupNamespace(group); ok {
		return sa.MakeNamespaceGroupName(util.UpstreamNamespace(tenantID, namespace)), nil
	}
	if strings.HasPrefix(group, "system:") {
		return "", errors.Errorf("group %q belongs to the cluster itself, not to tenant %s; "+
			"an access review may only ask about the tenant's own groups", group, tenantID)
	}
	return util.AddTenantIDPrefix(tenantID, group), nil
}

func backwardGroup(group, tenantID string) string {
	if namespace, ok := serviceAccountsGroupNamespace(group); ok {
		return sa.MakeNamespaceGroupName(util.TrimTenantIDPrefix(tenantID, namespace))
	}
	return util.TrimTenantIDPrefix(tenantID, group)
}

// serviceAccountsGroupNamespace pulls the namespace out of
// system:serviceaccounts:<namespace>, and reports false for anything else --
// including the bare system:serviceaccounts, which names no namespace.
func serviceAccountsGroupNamespace(group string) (string, bool) {
	const prefix = "system:serviceaccounts:"
	if !strings.HasPrefix(group, prefix) {
		return "", false
	}
	namespace := strings.TrimPrefix(group, prefix)
	if len(namespace) == 0 {
		return "", false
	}
	return namespace, true
}

// backwardRules strips the prefix from what a rules review reports, so a tenant
// reading its own permissions sees its own namespace of names.
//
// Trimming alone produces duplicates, because a rule that names an object
// arrives upstream carrying both spellings: the role transformer appends the
// prefixed name to the bare one, since a rule cannot say whether the resource it
// names is namespaced (unprefixed upstream) or cluster-scoped (prefixed). Both
// map to the same tenant-side name, so the list is deduplicated after trimming.
//
// role.go solves the same problem by dropping the prefixed entries instead,
// which is right there: the tenant wrote the bare list, so the bare list is what
// it should read back. It would be wrong here. A rules review reports what
// upstream RBAC actually evaluated, which may name only cluster-scoped objects;
// dropping those would leave resourceNames empty, and an empty resourceNames
// means every name -- reporting a wider permission than the tenant has, which is
// the same kind of confidently-wrong answer this transformer exists to stop.
func backwardRules(rules []authzinternal.ResourceRule, tenantID string) {
	for i := range rules {
		rules[i].APIGroups = trimAndDedupe(rules[i].APIGroups, tenantID)
		rules[i].ResourceNames = trimAndDedupe(rules[i].ResourceNames, tenantID)
	}
}

// trimAndDedupe strips the tenant prefix from every entry and keeps the first
// occurrence of each result, preserving order.
func trimAndDedupe(values []string, tenantID string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]bool, len(values))
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = util.TrimTenantIDPrefix(tenantID, value)
		if seen[value] {
			continue
		}
		seen[value] = true
		trimmed = append(trimmed, value)
	}
	return trimmed
}
