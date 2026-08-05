/*
Copyright 2024 The KubeZoo Authors.

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
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	core "k8s.io/kubernetes/pkg/apis/core"
)

func withLastApplied(value string) *core.ConfigMap {
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "default"}}
	if value != "" {
		cm.Annotations = map[string]string{corev1.LastAppliedConfigAnnotation: value}
	}
	return cm
}

func appliedNamespace(t *testing.T, obj *core.ConfigMap) string {
	t.Helper()
	var applied map[string]interface{}
	raw := obj.Annotations[corev1.LastAppliedConfigAnnotation]
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		t.Fatalf("the annotation is no longer valid JSON: %v", err)
	}
	metadata, _ := applied["metadata"].(map[string]interface{})
	ns, _ := metadata["namespace"].(string)
	return ns
}

// TestAPlatformAppliedObjectDoesNotShowItsUpstreamNamespace is the leak the
// read-direction sweep found: the platform applies an object straight upstream
// into a tenant namespace, and the object kubectl serialised into the annotation
// still names it.
func TestAPlatformAppliedObjectDoesNotShowItsUpstreamNamespace(t *testing.T) {
	cm := withLastApplied(`{"apiVersion":"v1","kind":"ConfigMap",` +
		`"metadata":{"name":"cm","namespace":"` + testTenant + `-default"}}`)
	NewDefaultConvertor(NewOwnerReferenceTransformer(checkGroupKind)).
		ConvertUpstreamObjectToTenantObject(cm, testTenant, true)
	if got := appliedNamespace(t, cm); got != "default" {
		t.Errorf("the tenant reads back namespace %q inside last-applied, want %q -- "+
			"kubectl three-way merges against this, so an apply would try to move the object",
			got, "default")
	}
}

// TestTheAnnotationRoundTrips is the property that matters for apply: whatever
// the tenant is handed has to go back upstream as the same string, or the next
// apply diffs against a namespace that does not exist.
func TestTheAnnotationRoundTrips(t *testing.T) {
	convertor := NewDefaultConvertor(NewOwnerReferenceTransformer(checkGroupKind))
	cm := withLastApplied(`{"metadata":{"name":"cm","namespace":"default"}}`)
	if err := convertor.ConvertTenantObjectToUpstreamObject(cm, testTenant, true); err != nil {
		t.Fatal(err)
	}
	if got := appliedNamespace(t, cm); got != testTenant+"-default" {
		t.Fatalf("forward left the annotation at %q, want the upstream spelling", got)
	}
	if err := convertor.ConvertUpstreamObjectToTenantObject(cm, testTenant, true); err != nil {
		t.Fatal(err)
	}
	if got := appliedNamespace(t, cm); got != "default" {
		t.Errorf("round trip gave %q, want %q", got, "default")
	}
}

// TestForwardIsIdempotent covers the in-cluster client: its manifest may already
// carry the upstream spelling, because that is what the projected namespace file
// says. Concatenating unconditionally produced <tid>-<tid>-default once already,
// in the access review transformer.
func TestLastAppliedForwardIsIdempotent(t *testing.T) {
	cm := withLastApplied(`{"metadata":{"namespace":"` + testTenant + `-default"}}`)
	if err := NewDefaultConvertor(NewOwnerReferenceTransformer(checkGroupKind)).
		ConvertTenantObjectToUpstreamObject(cm, testTenant, true); err != nil {
		t.Fatal(err)
	}
	if got := appliedNamespace(t, cm); got != testTenant+"-default" {
		t.Errorf("got %q, want the prefix applied once", got)
	}
}

// TestAnUnparsableAnnotationDoesNotFailTheObject is the rule this repository has
// paid for repeatedly: the annotation is free-form, anything may write anything
// there, and refusing one object fails the whole list.
func TestAnUnparsableAnnotationDoesNotFailTheObject(t *testing.T) {
	for _, value := range []string{"not json at all", `{"metadata":"a string, not an object"}`, `{}`} {
		cm := withLastApplied(value)
		if err := NewDefaultConvertor(NewOwnerReferenceTransformer(checkGroupKind)).
			ConvertUpstreamObjectToTenantObject(cm, testTenant, true); err != nil {
			t.Fatalf("%q failed the conversion: %v", value, err)
		}
		if got := cm.Annotations[corev1.LastAppliedConfigAnnotation]; got != value {
			t.Errorf("%q came back as %q, want it returned untouched", value, got)
		}
	}
}

// TestAClusterScopedObjectCarriesTheTenantInItsName -- the twin case. A rule
// applied to one of the two places it holds is this repository's most repeated
// defect, so the name side is asserted as well as the namespace side.
func TestAClusterScopedObjectCarriesTheTenantInItsName(t *testing.T) {
	pv := &core.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
		Name: testTenant + "-vol",
		Annotations: map[string]string{
			corev1.LastAppliedConfigAnnotation: `{"metadata":{"name":"` + testTenant + `-vol"}}`,
		},
	}}
	if err := NewDefaultConvertor(NewOwnerReferenceTransformer(checkGroupKind)).
		ConvertUpstreamObjectToTenantObject(pv, testTenant, false); err != nil {
		t.Fatal(err)
	}
	var applied map[string]interface{}
	if err := json.Unmarshal([]byte(pv.Annotations[corev1.LastAppliedConfigAnnotation]), &applied); err != nil {
		t.Fatal(err)
	}
	metadata, _ := applied["metadata"].(map[string]interface{})
	if got, _ := metadata["name"].(string); got != "vol" {
		t.Errorf("the tenant reads back name %q inside last-applied, want %q", got, "vol")
	}
}
