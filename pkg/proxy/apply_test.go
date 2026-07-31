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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"sigs.k8s.io/structured-merge-diff/v6/typed"

	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// recordingResource captures the request a forwarded apply actually makes. Only
// Patch is implemented; anything else this test reaches would be a test bug, and
// the embedded nil interface says so loudly.
type recordingResource struct {
	kubezoodynamic.NamespaceableResourceInterface
	patchType types.PatchType
	body      []byte
}

func (r *recordingResource) Namespace(string) kubezoodynamic.ResourceInterface { return r }

func (r *recordingResource) Patch(_ context.Context, name string, pt types.PatchType, data []byte,
	_ metav1.PatchOptions, _ ...string) (*unstructured.Unstructured, bool, error) {
	r.patchType = pt
	r.body = data
	out := &unstructured.Unstructured{Object: map[string]interface{}{}}
	out.SetName(name)
	return out, false, nil
}

// applyThrough drives the real forwardApply and returns the body that went
// upstream. ⚠️ Going through forwardApply is the point: the first version of
// this test called conversionDelta directly and did its own union, so deleting
// the union from the product code left the whole suite green -- a guard that
// guards nothing. Every assertion below has been checked to go red when the fix
// it names is removed.
func applyThrough(t *testing.T, tenantForm, upstream *unstructured.Unstructured,
	owned string, isApply bool) map[string]interface{} {
	t.Helper()

	upstream = upstream.DeepCopy()
	upstream.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:   "ctrl",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1:  &metav1.FieldsV1{Raw: []byte(owned)},
	}})

	recorder := &recordingResource{}
	tp := &tenantProxy{
		kind:            schema.GroupVersionKind{Version: "v1", Kind: upstream.GetKind()},
		resource:        strings.ToLower(upstream.GetKind()) + "s",
		namespaceScoped: upstream.GetNamespace() != "",
		typeConverter:   deducedTypeConverter,
		dynamicClient:   &recordingClient{resource: recorder},
	}

	ctx := request.WithUser(context.Background(), &user.DefaultInfo{
		Name:  "111111-admin",
		Extra: map[string][]string{"tenant": {"111111"}},
	})
	ctx = request.WithRequestInfo(ctx, &request.RequestInfo{Verb: "patch", Namespace: "default"})
	if isApply {
		ctx = util.WithApplyPatch(ctx)
	}

	applied, err := tp.forwardApply(ctx, upstream, tenantForm, "ctrl",
		&metav1.UpdateOptions{FieldManager: "ctrl"})
	if err != nil {
		t.Fatalf("forwardApply: %v", err)
	}
	if !isApply {
		if applied != nil || recorder.body != nil {
			t.Fatalf("an ordinary write was forwarded upstream as a server-side apply: %s", recorder.body)
		}
		return nil
	}
	if applied == nil {
		t.Fatal("a real apply was not forwarded")
	}
	if recorder.patchType != types.ApplyPatchType {
		t.Fatalf("forwarded as %q, not as an apply", recorder.patchType)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(recorder.body, &sent); err != nil {
		t.Fatalf("unmarshalling what went upstream: %v", err)
	}
	return sent
}

type recordingClient struct{ resource *recordingResource }

func (c *recordingClient) Resource(schema.GroupVersionResource) kubezoodynamic.NamespaceableResourceInterface {
	return c.resource
}

// TestForwardedApplyCarriesInjectedFields is the guard on this round's
// cross-tenant escape, and it drives forwardApply itself.
//
// The field set a forwarded apply is built from was computed against the object
// the tenant sent, before conversion, so a field kubezoo *injects* during
// conversion is owned by nobody and used to be silently absent from the patch
// that went upstream. The webhook transformer's namespaceSelector went that way,
// and upstream defaults an absent selector to {} -- a selector matching every
// namespace in the cluster, on a configuration the tenant is allowed to write
// precisely because that selector confines it.
func TestForwardedApplyCarriesInjectedFields(t *testing.T) {
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

	sent := applyThrough(t, tenantForm, upstream,
		`{"f:metadata":{"f:labels":{"f:env":{}}}}`, true)

	labels, found, err := unstructured.NestedStringMap(sent, "metadata", "labels")
	if err != nil || !found {
		t.Fatalf("the patch that went upstream carries no labels: %v", sent)
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

// TestForwardedApplyNeedsTheTenantForm pins the other half of the same fix: the
// snapshot taken before conversion is what makes the delta computable, and
// losing it must not silently degrade every apply into an ordinary update.
func TestForwardedApplyNeedsTheTenantForm(t *testing.T) {
	upstream := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cm", "namespace": "111111-default"},
		"data":       map[string]interface{}{"a": "1"},
	}}
	upstream.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:   "ctrl",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:a":{}}}`)},
	}})

	recorder := &recordingResource{}
	tp := &tenantProxy{
		kind: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, resource: "configmaps",
		namespaceScoped: true, typeConverter: deducedTypeConverter,
		dynamicClient: &recordingClient{resource: recorder},
	}
	ctx := util.WithApplyPatch(request.WithRequestInfo(
		request.WithUser(context.Background(), &user.DefaultInfo{
			Name: "111111-admin", Extra: map[string][]string{"tenant": {"111111"}},
		}), &request.RequestInfo{Verb: "patch", Namespace: "default"}))

	// A real apply with the snapshot present is forwarded.
	if applied, err := tp.forwardApply(ctx, upstream, upstream.DeepCopy(), "ctrl",
		&metav1.UpdateOptions{FieldManager: "ctrl"}); err != nil || applied == nil {
		t.Fatalf("a real apply was not forwarded: applied=%v err=%v", applied, err)
	}

	// Without it the request must fail rather than quietly become an ordinary
	// write. ⚠️ The quiet version is invisible from the outside: the object
	// upstream still ends up correct, because an ordinary write carries the whole
	// converted body. Only the bookkeeping is wrong, and only the manager's next
	// apply notices, by conflicting with its own last one.
	if _, err := tp.forwardApply(ctx, upstream, nil, "ctrl",
		&metav1.UpdateOptions{FieldManager: "ctrl"}); err == nil {
		t.Error("an apply with no pre-conversion snapshot was silently downgraded to a plain write")
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

// TestForwardedApplyCarriesWhatWasInjectedAboveIt guards the blind spot the
// fifth round found in the fix above.
//
// ⚠️ conversionDelta compares the object before and after conversion, so it can
// only see what changed inside that window. asRoleBinding stamps the projection
// label above this storage -- inside objInfo.UpdatedObject -- so the label is
// identical on both sides and appears in neither Added nor Modified, and the
// tenant never wrote it so it is not in the owned set either. A
// server-side-applied ClusterRoleBinding was therefore created upstream with no
// label, which is what every consumer selects on: the tenant's
// ClusterRoleBindings listed as empty, and the controller never derived the
// cluster-scoped half of them.
func TestForwardedApplyCarriesWhatWasInjectedAboveIt(t *testing.T) {
	const label = "kubezoo.io/clusterrolebinding"

	// Both sides already carry the label: it was stamped before either was taken.
	record := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata": map[string]interface{}{
				"name":      "kubezoo:clusterrolebinding:my-crb",
				"namespace": "111111-kube-system",
				"labels":    map[string]interface{}{label: "true"},
			},
			"roleRef": map[string]interface{}{"kind": "ClusterRole", "name": "111111-my-role"},
		}}
	}

	recorder := &recordingResource{}
	upstream := record()
	upstream.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:   "kubectl",
		Operation: metav1.ManagedFieldsOperationApply,
		// What the tenant actually wrote: a roleRef, and no labels at all.
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:roleRef":{}}`)},
	}})

	tp := &tenantProxy{
		kind:            schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		resource:        "rolebindings",
		namespaceScoped: true,
		typeConverter:   deducedTypeConverter,
		dynamicClient:   &recordingClient{resource: recorder},
		injectedPaths:   projectionLabelPath(),
	}
	ctx := util.WithApplyPatch(request.WithRequestInfo(
		request.WithUser(context.Background(), &user.DefaultInfo{
			Name: "111111-admin", Extra: map[string][]string{"tenant": {"111111"}},
		}), &request.RequestInfo{Verb: "patch", Namespace: "kube-system"}))

	if _, err := tp.forwardApply(ctx, upstream, record(), "kubectl",
		&metav1.UpdateOptions{FieldManager: "kubectl"}); err != nil {
		t.Fatalf("forwardApply: %v", err)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal(recorder.body, &sent); err != nil {
		t.Fatalf("unmarshalling what went upstream: %v", err)
	}
	labels, found, _ := unstructured.NestedStringMap(sent, "metadata", "labels")
	if !found || labels[label] != "true" {
		t.Errorf("the forwarded apply created the projection record with no %s label: %s.\n"+
			"Every consumer selects on it, so the tenant's ClusterRoleBindings would list "+
			"as empty and the controller would never derive the cluster-scoped half", label, recorder.body)
	}
}

// pickyConverter accepts exactly one apiVersion, which is how a real
// crdHandler-built converter behaves: it is indexed by the tenant's GVK.
type pickyConverter struct {
	accepts string
	asked   []string
}

func (c *pickyConverter) ObjectToTyped(obj runtime.Object,
	_ ...typed.ValidationOptions) (*typed.TypedValue, error) {
	apiVersion := obj.(*unstructured.Unstructured).GetAPIVersion()
	c.asked = append(c.asked, apiVersion)
	if apiVersion != c.accepts {
		return nil, fmt.Errorf("no corresponding type for %s", apiVersion)
	}
	return deducedTypeConverter.ObjectToTyped(obj)
}

func (c *pickyConverter) TypedToObject(value *typed.TypedValue) (runtime.Object, error) {
	return deducedTypeConverter.TypedToObject(value)
}

// TestCustomResourceUsesItsOwnSchema guards the third of this round's fixes,
// which had no test in any of the three repositories.
//
// ⚠️ crdHandler builds the type converter from the CRD as the *tenant* sees it,
// whose group has had the tenant prefix trimmed. By the time forwardApply runs,
// the object has been converted upstream and its group carries the prefix, so
// the lookup missed for every custom resource -- silently, above V(4). Every CR
// therefore fell through to the deduced converter, under which all lists are
// atomic: extracting an associative-list set returns the whole list, so the
// forwarded apply claimed ownership of entries another manager wrote.
func TestCustomResourceUsesItsOwnSchema(t *testing.T) {
	converter := &pickyConverter{accepts: "example.com/v1"}
	tp := &tenantProxy{typeConverter: converter}

	upstream := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "111111-example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]interface{}{"name": "w"},
	}}

	_, used, err := tp.typeObject(upstream, "example.com/v1")
	if err != nil {
		t.Fatalf("typeObject: %v", err)
	}
	if used != converter {
		t.Errorf("a custom resource fell through to the deduced converter; under it every list "+
			"is atomic, so the forwarded apply would claim list entries another manager wrote. "+
			"apiVersions offered to the real converter: %v", converter.asked)
	}
}
