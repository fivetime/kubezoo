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
	admissioninternal "k8s.io/kubernetes/pkg/apis/admissionregistration"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

const testTenant = "111111"

func validatingConfig(clientConfig admissioninternal.WebhookClientConfig, scope *admissioninternal.ScopeType,
	selector *metav1.LabelSelector) *admissioninternal.ValidatingWebhookConfiguration {
	return &admissioninternal.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "hook"},
		Webhooks: []admissioninternal.ValidatingWebhook{{
			Name:              "hook.example.com",
			ClientConfig:      clientConfig,
			NamespaceSelector: selector,
			Rules: []admissioninternal.RuleWithOperations{{
				Rule: admissioninternal.Rule{
					APIGroups: []string{""}, APIVersions: []string{"v1"},
					Resources: []string{"configmaps"}, Scope: scope,
				},
			}},
		}},
	}
}

func serviceRef(namespace string) admissioninternal.WebhookClientConfig {
	return admissioninternal.WebhookClientConfig{
		Service: &admissioninternal.ServiceReference{Name: "svc", Namespace: namespace},
	}
}

// TestWebhookForwardConfinesToTenant covers the three rewrites together. Each
// one alone leaves a way out, so the test asserts all three on one object.
func TestWebhookForwardConfinesToTenant(t *testing.T) {
	everything := admissioninternal.AllScopes
	config := validatingConfig(serviceRef("default"), &everything, nil)

	out, err := NewWebhookConfigurationTransformer().Forward(config, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	webhook := out.(*admissioninternal.ValidatingWebhookConfiguration).Webhooks[0]

	if got, want := webhook.ClientConfig.Service.Namespace, testTenant+"-default"; got != want {
		t.Errorf("clientConfig namespace = %q, want %q; without the prefix a tenant reaches "+
			"the platform's namespace of that name rather than its own", got, want)
	}
	if webhook.NamespaceSelector == nil ||
		webhook.NamespaceSelector.MatchLabels[common.TenantNamespaceLabelKey] != testTenant {
		t.Errorf("namespaceSelector = %+v, want a match on %s=%s; a nil selector matches every "+
			"namespace in the cluster", webhook.NamespaceSelector, common.TenantNamespaceLabelKey, testTenant)
	}
	if webhook.Rules[0].Scope == nil || *webhook.Rules[0].Scope != admissioninternal.NamespacedScope {
		t.Errorf("rule scope = %v, want Namespaced; namespaceSelector does not apply to "+
			"cluster-scoped resources, so any other scope fires cluster-wide", webhook.Rules[0].Scope)
	}
}

// TestWebhookForwardOverwritesAWidenedSelector is the update case: a tenant that
// clears the selector or reopens the scope must not keep those values. The
// transformer runs on update too, so overwriting has to be unconditional rather
// than only filling in what is absent.
func TestWebhookForwardOverwritesAWidenedSelector(t *testing.T) {
	everything := admissioninternal.AllScopes
	config := validatingConfig(serviceRef("default"), &everything, &metav1.LabelSelector{})

	out, err := NewWebhookConfigurationTransformer().Forward(config, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	webhook := out.(*admissioninternal.ValidatingWebhookConfiguration).Webhooks[0]

	if webhook.NamespaceSelector.MatchLabels[common.TenantNamespaceLabelKey] != testTenant {
		t.Errorf("an empty namespaceSelector survived Forward, so a tenant can widen its "+
			"webhook back to the whole cluster on update: %+v", webhook.NamespaceSelector)
	}
	if *webhook.Rules[0].Scope != admissioninternal.NamespacedScope {
		t.Errorf("scope %v survived Forward", *webhook.Rules[0].Scope)
	}
}

// TestWebhookForwardRejectsURL covers the escape that cannot be rewritten: a raw
// URL points wherever the tenant says and no prefix confines it.
func TestWebhookForwardRejectsURL(t *testing.T) {
	url := "https://attacker.example.com/exfil"
	config := validatingConfig(admissioninternal.WebhookClientConfig{URL: &url}, nil, nil)

	if _, err := NewWebhookConfigurationTransformer().Forward(config, testTenant); err == nil {
		t.Fatal("Forward accepted clientConfig.url; the apiserver would then call an endpoint " +
			"of the tenant's choosing with every matching object")
	}
}

// TestWebhookBackwardRestoresTheNamespace checks the round trip, so that a tenant
// reading its webhook back sees the namespace it wrote.
func TestWebhookBackwardRestoresTheNamespace(t *testing.T) {
	transformer := NewWebhookConfigurationTransformer()
	config := validatingConfig(serviceRef("default"), nil, nil)

	forward, err := transformer.Forward(config, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	back, err := transformer.Backward(forward, testTenant)
	if err != nil {
		t.Fatalf("Backward: %v", err)
	}
	if got := back.(*admissioninternal.ValidatingWebhookConfiguration).Webhooks[0].ClientConfig.Service.Namespace; got != "default" {
		t.Errorf("round trip produced namespace %q, want %q", got, "default")
	}
}

// TestWebhookBackwardDoesNotDisguiseAForeignNamespace guards what the previous
// version of this test SAID it guarded: an upstream object naming another
// tenant's namespace must not be trimmed into something that looks like it
// belongs to this one.
//
// ⚠️ It used to assert a REFUSAL instead, which is a different thing and a worse
// one. Refusing fails the whole object, and one failed object fails the whole
// list -- a tenant with a single misconfigured webhook configuration could see
// none of its own. The disguising it feared does not happen either way:
// trimIfAttributable only strips a prefix that matches, so a foreign namespace
// comes back verbatim and reads as what it is.
//
// ⭐ And such an object can only be the platform's doing: Forward prefixes with
// the REQUESTING tenant's id unconditionally, and the read path already hides
// cluster-scoped objects whose name lacks that prefix. A tenant seeing that its
// own webhook points somewhere it cannot reach is better served than a tenant
// whose list simply errors.
func TestWebhookBackwardDoesNotDisguiseAForeignNamespace(t *testing.T) {
	config := validatingConfig(serviceRef("222222-default"), nil, nil)

	out, err := NewWebhookConfigurationTransformer().Backward(config, testTenant)
	if err != nil {
		t.Fatalf("Backward refused instead of showing it: %v", err)
	}
	got := out.(*admissioninternal.ValidatingWebhookConfiguration).Webhooks[0].ClientConfig.Service.Namespace
	if got != "222222-default" {
		t.Errorf("a foreign namespace came back as %q; it must not be disguised as this tenant's", got)
	}
}

// TestWebhookTransformerHandlesMutatingToo -- the two kinds carry the same
// fields, and it would be easy to wire one and forget the other.
func TestWebhookTransformerHandlesMutatingToo(t *testing.T) {
	config := &admissioninternal.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "hook"},
		Webhooks: []admissioninternal.MutatingWebhook{{
			Name:         "hook.example.com",
			ClientConfig: serviceRef("default"),
			Rules: []admissioninternal.RuleWithOperations{{
				Rule: admissioninternal.Rule{Resources: []string{"pods"}},
			}},
		}},
	}

	out, err := NewWebhookConfigurationTransformer().Forward(config, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	webhook := out.(*admissioninternal.MutatingWebhookConfiguration).Webhooks[0]
	if webhook.ClientConfig.Service.Namespace != testTenant+"-default" ||
		webhook.NamespaceSelector == nil ||
		*webhook.Rules[0].Scope != admissioninternal.NamespacedScope {
		t.Errorf("mutating webhook was not confined the way the validating one is: %+v", webhook)
	}
}
