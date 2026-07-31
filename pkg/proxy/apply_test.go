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

package proxy

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// TestConversionDeltaCarriesInjectedFields is the guard on a cross-tenant escape.
//
// The field set a forwarded apply is built from was computed against the object
// the tenant sent, before conversion, so a field kubezoo *injects* during
// conversion is owned by nobody and used to be silently absent from the patch
// that went upstream.
//
// The namespace label is the victim used here because it discriminates under a
// deduced schema. The other one was worse and has the same cause: the webhook
// transformer's namespaceSelector, which is what confines a tenant's
// ValidatingWebhookConfiguration to its own namespaces -- and the contract grants
// tenants cluster-wide write on webhook configurations *because* of it. An
// applied webhook reached upstream with no selector and a default failurePolicy
// of Fail, so the tenant's own service being down rejected matching writes in
// every namespace in the cluster.
func TestConversionDeltaCarriesInjectedFields(t *testing.T) {
	tenantForm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":   "team-a",
			"labels": map[string]interface{}{"env": "prod"},
		},
	}}
	// What pkg/convert's namespace transformer produces: the name rewritten -- a
	// path the tenant owns -- and the tenant label injected, which nobody owns.
	upstream := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": "111111-team-a",
			"labels": map[string]interface{}{
				"env":               "prod",
				"kubezoo.io/tenant": "111111",
			},
		},
	}}

	delta, err := conversionDelta(deducedTypeConverter, tenantForm, upstream, tenantForm.GetAPIVersion())
	if err != nil {
		t.Fatalf("computing the delta: %v", err)
	}

	// What the tenant's field manager owns: the label it wrote, and not the one
	// it never sent.
	owned := &fieldpath.Set{}
	if err := owned.FromJSON(strings.NewReader(
		`{"f:metadata":{"f:labels":{"f:env":{}}}}`)); err != nil {
		t.Fatalf("reading the owned set: %v", err)
	}

	typedUpstream, err := deducedTypeConverter.ObjectToTyped(upstream)
	if err != nil {
		t.Fatalf("typing the upstream object: %v", err)
	}
	extracted, ok := typedUpstream.ExtractItems(owned.Union(delta).Leaves()).
		AsValue().Unstructured().(map[string]interface{})
	if !ok {
		t.Fatal("extraction did not come back as an object")
	}

	labels, found, err := unstructured.NestedStringMap(extracted, "metadata", "labels")
	if err != nil || !found {
		t.Fatalf("the patch carries no labels: %v", extracted)
	}
	if labels["kubezoo.io/tenant"] != "111111" {
		t.Errorf("the forwarded apply dropped kubezoo.io/tenant: %v.\n"+
			"An unlabelled namespace is invisible to every cluster-wide list, watch and "+
			"teardown that selects on it, while the tenant still sees it by name", labels)
	}
	if labels["env"] != "prod" {
		t.Errorf("the tenant's own label did not survive: %v", labels)
	}
}

// TestApplyIsDecidedByTheRequest pins where the verb comes from.
//
// ⚠️ It used to come from the object: any write whose field manager happened to
// have an Apply entry was rewritten into an apply of whatever that stale entry
// owned, dropping the rest of the write -- and, because the apiserver moves a
// changed path out of the Apply entry into an Update entry, deleting upstream
// the very field the write had changed.
func TestApplyIsDecidedByTheRequest(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cm", "namespace": "111111-default"},
		"data":       map[string]interface{}{"a": "1"},
	}}
	object.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:   "ctrl",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:a":{}}}`)},
	}})

	tp := &tenantProxy{typeConverter: deducedTypeConverter}
	// A plain PUT by the same manager. The Apply entry is right there, and it
	// still is not an apply.
	applied, err := tp.forwardApply(context.Background(), object, object.DeepCopy(), "ctrl",
		&metav1.UpdateOptions{FieldManager: "ctrl"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != nil {
		t.Fatal("an ordinary update was forwarded upstream as a server-side apply")
	}

	// The same object, now on a request that really is one. It gets far enough
	// to need a client, which this proxy has not got -- reaching that point is
	// the assertion.
	ctx := util.WithApplyPatch(context.Background())
	if _, err := tp.forwardApply(ctx, object, object.DeepCopy(), "ctrl",
		&metav1.UpdateOptions{FieldManager: "ctrl"}); err == nil {
		t.Error("a real apply was declined")
	}
}
