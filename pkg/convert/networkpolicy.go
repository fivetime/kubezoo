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
	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	networking "k8s.io/kubernetes/pkg/apis/networking"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// NetworkPolicyTransformer confines a tenant's namespaceSelectors to the
// tenant's own namespaces.
//
// ⛔ A NetworkPolicy peer selects namespaces by LABEL, cluster-wide, and nothing
// translated that -- not kubezoo, not the policies, not kubetron (checked: zero
// references in all three). So a tenant's stated intent and what upstream
// enforced were two different things:
//
//   - `namespaceSelector: {}` means every namespace IN THE CLUSTER, not every
//     namespace the tenant owns. A tenant narrowing ingress to "my namespaces"
//     was in fact opening its pods to every other tenant.
//   - `matchLabels: {kubernetes.io/metadata.name: default}` names the namespace
//     literally called `default`, which upstream is the PLATFORM's. The label is
//     set by the apiserver itself (pkg/registry/core/namespace/strategy.go), so
//     a tenant's own namespace carries `<tid>-default` there.
//
// ⚠️ Neither is an escape -- a NetworkPolicy only governs pods in its own
// namespace, so a tenant can only misconfigure its own. What it is, is the API
// lying: the tenant writes an isolation rule, the cluster enforces a different
// one, and nothing says so. Making the tenant's view mean what it says is the
// job kubezoo exists for.
//
// Two changes, and both are needed:
//
//  1. The tenant label is ANDed into every namespaceSelector, so no selector can
//     reach past the tenant whatever else it says. This also covers a selector
//     matching some label a tenant put on its own namespaces that a platform
//     namespace happens to carry too.
//  2. kubernetes.io/metadata.name is prefixed, so naming your own namespace
//     works.
type NetworkPolicyTransformer struct{}

var _ ObjectTransformer = &NetworkPolicyTransformer{}

// NewNetworkPolicyTransformer initiates a NetworkPolicyTransformer.
func NewNetworkPolicyTransformer() ObjectTransformer { return &NetworkPolicyTransformer{} }

// Forward confines every namespaceSelector to the tenant.
func (t *NetworkPolicyTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	policy, ok := obj.(*networking.NetworkPolicy)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of networkpolicy")
	}
	forEachPeer(policy, func(peer *networking.NetworkPolicyPeer) {
		if peer.NamespaceSelector == nil {
			// ⚠️ Left alone deliberately. A nil namespaceSelector means "this
			// policy's own namespace", which is already the tenant's -- the
			// namespace was prefixed on the way in. Adding the label here would
			// change that to "every namespace of mine", which is a different and
			// wider rule than the tenant wrote.
			return
		}
		confineToTenant(peer.NamespaceSelector, tenantID)
	})
	return policy, nil
}

// Backward hands back the selector the tenant wrote.
func (t *NetworkPolicyTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	policy, ok := obj.(*networking.NetworkPolicy)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of networkpolicy")
	}
	forEachPeer(policy, func(peer *networking.NetworkPolicyPeer) {
		if peer.NamespaceSelector == nil {
			return
		}
		releaseFromTenant(peer.NamespaceSelector, tenantID)
	})
	return policy, nil
}

func forEachPeer(policy *networking.NetworkPolicy, visit func(*networking.NetworkPolicyPeer)) {
	for i := range policy.Spec.Ingress {
		for j := range policy.Spec.Ingress[i].From {
			visit(&policy.Spec.Ingress[i].From[j])
		}
	}
	for i := range policy.Spec.Egress {
		for j := range policy.Spec.Egress[i].To {
			visit(&policy.Spec.Egress[i].To[j])
		}
	}
}

func confineToTenant(selector *metav1.LabelSelector, tenantID string) {
	if selector.MatchLabels == nil {
		selector.MatchLabels = map[string]string{}
	}
	if name, ok := selector.MatchLabels[corev1.LabelMetadataName]; ok && name != "" {
		selector.MatchLabels[corev1.LabelMetadataName] = util.UpstreamNamespace(tenantID, name)
	}
	selector.MatchLabels[common.TenantNamespaceLabelKey] = tenantID
}

func releaseFromTenant(selector *metav1.LabelSelector, tenantID string) {
	if selector.MatchLabels == nil {
		return
	}
	if name, ok := selector.MatchLabels[corev1.LabelMetadataName]; ok && name != "" {
		selector.MatchLabels[corev1.LabelMetadataName] = util.TrimTenantIDPrefix(tenantID, name)
	}
	delete(selector.MatchLabels, common.TenantNamespaceLabelKey)
	// ⚠️ An empty map is not the same object a tenant wrote: it applied {} and
	// would read back {matchLabels: {}}, which a server-side apply then treats as
	// a field it owns and keeps re-sending. Put it back the way it came.
	if len(selector.MatchLabels) == 0 {
		selector.MatchLabels = nil
	}
}
