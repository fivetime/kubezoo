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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/apis/core"
)

// TestEchoRequestNamespace pins the answer to the question "which of a
// namespace's two names does the caller get back".
//
// ⚠️ The upstream spelling is not a curiosity to be tolerated. A workload's
// client-go reads its namespace out of the projected service account file, which
// kubelet writes from the upstream apiserver's view, so every in-cluster
// controller a tenant runs addresses its own namespace as <tid>-default. Answering
// such a request with the object relabelled "default" made the generic patch
// handler refuse the write -- see the comment on
// convertUpstreamObjectToTenantObject.
func TestEchoRequestNamespace(t *testing.T) {
	const tenantID = "909090"

	object := func(namespace string) runtime.Object {
		return &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: namespace}}
	}
	namespaceOf := func(obj runtime.Object) string {
		return obj.(*core.ConfigMap).Namespace
	}

	for _, tc := range []struct {
		what      string
		converted string // what the backward conversion left on the object
		requested string // how the caller spelled it
		want      string
	}{
		{
			what:      "the caller used the tenant's own name",
			converted: "default", requested: "default", want: "default",
		},
		{
			what:      "the caller used the upstream name, which is what an in-cluster client does",
			converted: "default", requested: "909090-default", want: "909090-default",
		},
		{
			// A list across every namespace the tenant owns. Its items come from
			// namespaces the caller never named, so there is nothing to echo and
			// stamping any one name on them would be a lie.
			what:      "the request named no namespace at all",
			converted: "default", requested: "", want: "default",
		},
		{
			// A guard against the echo becoming a rubber stamp: if the object did
			// not come from where the caller asked, saying that it did would hide
			// the discrepancy rather than surface it.
			what:      "the object is from a different namespace than the one asked for",
			converted: "other", requested: "909090-default", want: "other",
		},
		{
			// Falls out of the same-namespace guard above rather than a check of
			// its own; pinned because "" reaching SetNamespace would put a
			// namespace on a cluster-scoped object.
			what:      "the object carries no namespace",
			converted: "", requested: "909090-default", want: "",
		},
	} {
		obj := object(tc.converted)
		echoRequestNamespace(obj, tenantID, tc.requested)
		if got := namespaceOf(obj); got != tc.want {
			t.Errorf("%s: namespace is %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestEchoRequestNamespaceIgnoresLists -- a List reaches the same code path and
// has no namespace of its own to correct. Its items are converted individually.
func TestEchoRequestNamespaceIgnoresLists(t *testing.T) {
	list := &unstructured.UnstructuredList{}
	echoRequestNamespace(list, "909090", "909090-default") // must not panic
}

// TestEchoRequestNamespaceOnUnstructured -- custom resources travel as
// unstructured all the way through, and must get the same answer as native ones.
func TestEchoRequestNamespaceOnUnstructured(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "stable.example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]interface{}{"name": "w", "namespace": "default"},
	}}
	echoRequestNamespace(object, "909090", "909090-default")
	if got := object.GetNamespace(); got != "909090-default" {
		t.Errorf("a custom resource kept namespace %q, want the spelling the caller used", got)
	}
}
