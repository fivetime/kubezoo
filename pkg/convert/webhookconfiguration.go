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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	admissioninternal "k8s.io/kubernetes/pkg/apis/admissionregistration"

	"github.com/kubewharf/kubezoo/pkg/common"
	"github.com/kubewharf/kubezoo/pkg/util"
)

// WebhookConfigurationTransformer confines a tenant's admission webhooks to
// that tenant.
//
// A webhook configuration is cluster-scoped, so its name is prefixed like any
// other cluster-scoped object, but its *effect* is not bounded by its name.
// Left alone, a tenant's webhook matched every object in the cluster and its
// clientConfig named a namespace in the platform's own namespace space. That
// was a cluster-wide denial of service, or -- pointed at a reachable endpoint
// with failurePolicy Ignore -- a read of every object being created anywhere.
//
// Three things are rewritten, and all three are needed. Any one of them alone
// leaves a way out:
//
//   - clientConfig.service.namespace gets the tenant prefix. Without it a
//     tenant cannot reach its own service at all -- it resolves to the
//     platform's namespace of that name -- so this is also what makes tenant
//     webhooks work for the first time.
//   - namespaceSelector is replaced with one that matches only namespaces
//     labelled for this tenant.
//   - every rule's scope is forced to Namespaced. namespaceSelector does not
//     apply to cluster-scoped resources, so without this a rule naming, say,
//     clusterroles still fires cluster-wide.
//
// clientConfig.url is rejected rather than rewritten: it points wherever the
// tenant says, and nothing about it can be confined.
//
// Replacement is unconditional. The transformer runs on update as well as
// create, so a tenant cannot widen its webhook after the fact.
type WebhookConfigurationTransformer struct{}

var _ ObjectTransformer = &WebhookConfigurationTransformer{}

// NewWebhookConfigurationTransformer initiates a transformer for mutating and
// validating webhook configurations.
func NewWebhookConfigurationTransformer() ObjectTransformer {
	return &WebhookConfigurationTransformer{}
}

// Forward transforms tenant object reference to upstream object reference.
func (t *WebhookConfigurationTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	switch config := obj.(type) {
	case *admissioninternal.MutatingWebhookConfiguration:
		for i := range config.Webhooks {
			w := &config.Webhooks[i]
			if err := forwardWebhookClientConfig(&w.ClientConfig, tenantID, w.Name); err != nil {
				return nil, err
			}
			w.NamespaceSelector = tenantNamespaceSelector(tenantID)
			confineRulesToNamespacedScope(w.Rules)
		}
		return config, nil
	case *admissioninternal.ValidatingWebhookConfiguration:
		for i := range config.Webhooks {
			w := &config.Webhooks[i]
			if err := forwardWebhookClientConfig(&w.ClientConfig, tenantID, w.Name); err != nil {
				return nil, err
			}
			w.NamespaceSelector = tenantNamespaceSelector(tenantID)
			confineRulesToNamespacedScope(w.Rules)
		}
		return config, nil
	default:
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of a webhook configuration")
	}
}

// Backward transforms upstream object reference to tenant object reference.
//
// Only the service namespace is reversed. The enforced namespaceSelector and
// rule scopes are left visible on purpose: they are the actual semantics of the
// tenant's webhook, not platform bookkeeping, and a tenant should be able to see
// what its webhook will and will not match.
func (t *WebhookConfigurationTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	switch config := obj.(type) {
	case *admissioninternal.MutatingWebhookConfiguration:
		for i := range config.Webhooks {
			if err := backwardWebhookClientConfig(&config.Webhooks[i].ClientConfig, tenantID, config.Webhooks[i].Name); err != nil {
				return nil, err
			}
		}
		return config, nil
	case *admissioninternal.ValidatingWebhookConfiguration:
		for i := range config.Webhooks {
			if err := backwardWebhookClientConfig(&config.Webhooks[i].ClientConfig, tenantID, config.Webhooks[i].Name); err != nil {
				return nil, err
			}
		}
		return config, nil
	default:
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of a webhook configuration")
	}
}

// tenantNamespaceSelector matches exactly the namespaces belonging to a tenant.
// pkg/convert stamps this label on every namespace on the way through and
// refuses to let a tenant claim another tenant's id, so a tenant cannot label
// its way out.
func tenantNamespaceSelector(tenantID string) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{common.TenantNamespaceLabelKey: tenantID},
	}
}

// forwardWebhookClientConfig points a webhook at the tenant's own copy of the
// namespace it named, and refuses a raw URL.
func forwardWebhookClientConfig(clientConfig *admissioninternal.WebhookClientConfig, tenantID, webhookName string) error {
	if clientConfig.URL != nil {
		return errors.Errorf("webhook %s: clientConfig.url is not available to tenants, "+
			"because a URL cannot be confined to the tenant; use clientConfig.service to "+
			"name a service in one of your namespaces", webhookName)
	}
	if clientConfig.Service != nil && len(clientConfig.Service.Namespace) > 0 {
		clientConfig.Service.Namespace = util.AddTenantIDPrefix(tenantID, clientConfig.Service.Namespace)
	}
	return nil
}

// backwardWebhookClientConfig restores the namespace the tenant wrote.
func backwardWebhookClientConfig(clientConfig *admissioninternal.WebhookClientConfig, tenantID, webhookName string) error {
	if clientConfig.Service == nil || len(clientConfig.Service.Namespace) == 0 {
		return nil
	}
	if !strings.HasPrefix(clientConfig.Service.Namespace, tenantID+"-") {
		return errors.Errorf("webhook %s: clientConfig points at namespace %s, which does not belong to tenant %s",
			webhookName, clientConfig.Service.Namespace, tenantID)
	}
	clientConfig.Service.Namespace = util.TrimTenantIDPrefix(tenantID, clientConfig.Service.Namespace)
	return nil
}

// confineRulesToNamespacedScope stops a webhook from matching cluster-scoped
// objects, which no namespaceSelector can exclude.
func confineRulesToNamespacedScope(rules []admissioninternal.RuleWithOperations) {
	namespaced := admissioninternal.NamespacedScope
	for i := range rules {
		rules[i].Scope = &namespaced
	}
}
