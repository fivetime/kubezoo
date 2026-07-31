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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/typed"

	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// deducedTypeConverter reads an object with no schema to hand.
//
// It is a last resort, and deliberately not the custom-resource path: a CRD's
// own schema is what tells apply which lists merge by key, and losing it means
// claiming to own list entries somebody else wrote. See typeObject.
var deducedTypeConverter = managedfields.NewDeducedTypeConverter()

// forwardApply sends a tenant's server-side apply upstream as an apply.
//
// Resolving the apply here and writing the result with a PUT is what kubezoo
// used to do, and it produces the right object once. What it does not produce is
// the right bookkeeping: upstream records the field manager as having performed
// an update, so the next apply from the same manager conflicts with the last one
// -- with itself -- and needs --force-conflicts to get through. Converging on
// repeated application is the whole point of the mechanism, and a controller
// applies on every pass of its loop.
//
// The difficulty is that an apply is a partial object and kubezoo's convertors
// are written for whole ones. Running them over a fragment and serialising the
// result back would materialise every default the type has, and the apply would
// then claim to own fields the tenant never wrote.
//
// So the fragment is never converted. The apiserver resolves the apply against
// the current object as before, which yields a complete object that the ordinary
// conversion path handles, and it also records which fields this manager now
// owns. Those fields are then lifted back out of the converted object, which
// gives the same fragment the tenant sent with every reference rewritten.
//
// tenantForm is that same object as it stood before conversion, and it is
// required: what to lift is not the owned set alone. See conversionDelta.
func (tp *tenantProxy) forwardApply(ctx context.Context, upstream, tenantForm *unstructured.Unstructured,
	fieldManager string, options *metav1.UpdateOptions) (*unstructured.Unstructured, error) {

	// The verb comes from the request, never from the object. A write is not an
	// apply because its manager happened to apply something once before.
	if !util.IsApplyPatch(ctx) {
		return nil, nil
	}
	entry, found := applyEntry(upstream, fieldManager)
	if !found {
		return nil, nil
	}
	if tp.typeConverter == nil {
		// Without a schema the field set cannot be turned back into an object,
		// and guessing would mean applying fields nobody asked for.
		return nil, nil
	}
	if tenantForm == nil {
		// ⚠️ Loud, not a quiet fall-through. Every caller reaching here on an
		// apply takes the snapshot first, so a nil is a programming error -- and
		// falling back to an ordinary write would hide it perfectly, because that
		// write carries the whole converted object and the result upstream looks
		// right. What is lost is the bookkeeping, and it stays lost silently
		// until the manager's next apply conflicts with its own last one.
		// Removing the snapshot from both callers was measured to leave every
		// test in the repository green.
		return nil, fmt.Errorf("cannot forward this apply: the object as the tenant sent it was not kept")
	}

	fields := &fieldpath.Set{}
	if err := fields.FromJSON(entry.FieldsV1.GetRawReader()); err != nil {
		return nil, fmt.Errorf("reading the field set this apply owns: %w", err)
	}

	// managedFields never travel in a patch body, and upstream refuses a request
	// carrying them.
	stripped := upstream.DeepCopy()
	stripped.SetManagedFields(nil)

	// Both objects are typed in the tenant's own terms, so that they are the
	// same type and can be compared. The upstream form differs only in the values
	// kubezoo rewrote, plus the group prefix on a custom resource -- and the
	// schema is the tenant CRD's either way.
	tenantAPIVersion := tenantForm.GetAPIVersion()
	typedUpstream, converter, err := tp.typeObject(stripped, tenantAPIVersion)
	if err != nil {
		klog.V(4).Infof("not forwarding this apply as an apply, its shape could not be read: %v", err)
		return nil, nil
	}

	injected, err := conversionDelta(converter, tenantForm, stripped, tenantAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("working out which fields kubezoo added to this apply: %w", err)
	}
	fields = fields.Union(injected)
	if tp.injectedPaths != nil {
		// What a storage above this one added before the snapshot was taken, and
		// which the comparison therefore cannot see. See tenantProxy.injectedPaths.
		fields = fields.Union(tp.injectedPaths)
	}

	extracted, ok := typedUpstream.ExtractItems(fields.Leaves()).AsValue().Unstructured().(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("the fields this apply owns did not come back as an object")
	}

	// The field set carries no identity: name, namespace and the type are how
	// the request is addressed, not fields anyone owns. Put them back, in their
	// upstream form.
	patch := &unstructured.Unstructured{Object: extracted}
	patch.SetAPIVersion(upstream.GetAPIVersion())
	patch.SetKind(upstream.GetKind())
	patch.SetName(upstream.GetName())
	if namespace := upstream.GetNamespace(); namespace != "" {
		patch.SetNamespace(namespace)
	}

	body, err := json.Marshal(patch.Object)
	if err != nil {
		return nil, err
	}

	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, err
	}
	patchOptions := metav1.PatchOptions{DryRun: options.DryRun, FieldManager: fieldManager}
	if force := util.ApplyForceFrom(ctx); force {
		patchOptions.Force = &force
	}
	applied, _, err := client.Patch(ctx, upstream.GetName(), types.ApplyPatchType, body, patchOptions)
	if err != nil {
		return nil, err
	}
	return applied, nil
}

// typeObject reads an object against the schema kubezoo holds for it, returning
// the converter it used so that a second object can be typed the same way.
//
// ⚠️ The converter is indexed by the *tenant's* GVK -- crdHandler builds it from
// the CRD as the tenant sees it, whose group has had the tenant prefix trimmed.
// The object handed here has already been converted upstream, so its group
// carries the prefix and the lookup missed for every custom resource, silently
// above V(4). Every CR therefore fell through to the deduced converter, under
// which all lists are atomic: extracting an associative-list set returns the
// *whole* list, so the forwarded apply claimed ownership of entries another
// manager wrote and of defaults the tenant never sent. Nobody was warned, since
// the values agreed and so there was no conflict to report.
func (tp *tenantProxy) typeObject(obj *unstructured.Unstructured,
	tenantAPIVersion string) (*typed.TypedValue, managedfields.TypeConverter, error) {

	candidate := asAPIVersion(obj, tenantAPIVersion)
	if typedObj, err := tp.typeConverter.ObjectToTyped(candidate); err == nil {
		return typedObj, tp.typeConverter, nil
	}
	// Genuinely no schema. Upstream reads one the same way, treating every list
	// as a whole rather than merging it by key.
	typedObj, err := deducedTypeConverter.ObjectToTyped(candidate)
	if err != nil {
		return nil, nil, err
	}
	return typedObj, deducedTypeConverter, nil
}

// conversionDelta is the set of paths kubezoo's convertors touched.
//
// ⚠️ The owned field set is not enough on its own, and the gap it left is a
// cross-tenant one. That set was computed by this apiserver's field manager
// against the object as the tenant sent it, before conversion -- so a field
// kubezoo *adds* during conversion is owned by nobody and was silently absent
// from the patch that went upstream. Fields kubezoo rewrites were fine, because
// the tenant owns those paths already; the injected ones vanished.
//
// What vanished was load-bearing. pkg/convert's webhook transformer injects the
// namespaceSelector that confines a tenant's ValidatingWebhookConfiguration to
// its own namespaces, and kubezoo-contract's ClusterScopedRules grants tenants
// cluster-wide write on webhook configurations *because* of that confinement. An
// applied webhook reached upstream with no selector and a default failurePolicy
// of Fail, so the tenant's own service being down rejected matching writes in
// every namespace in the cluster. The namespace transformer's kubezoo.io/tenant
// label went the same way, which takes a namespace out of every cluster-wide
// list, watch and teardown that selects on it.
//
// Taking the union with everything conversion touched closes the class for every
// convertor, so one added later cannot reintroduce it.
//
// ⚠️ It closes that class and no other, and an earlier version of this comment
// claimed otherwise. The comparison can only see what changed between the
// tenantForm snapshot and the converted object -- that is, inside
// convertTenantObjectToUpstreamObject. A field injected *above* this storage is
// present on both sides and shows up in neither Added nor Modified. There is
// already one in tree: the ClusterRoleBinding projection stamps its label in
// asRoleBinding, inside objInfo.UpdatedObject, and a server-side-applied
// ClusterRoleBinding was created upstream with no label at all. Injectors above
// a storage declare themselves through tenantProxy.injectedPaths.
func conversionDelta(converter managedfields.TypeConverter, tenantForm, upstream *unstructured.Unstructured,
	tenantAPIVersion string) (*fieldpath.Set, error) {

	before, err := converter.ObjectToTyped(tenantForm)
	if err != nil {
		return nil, err
	}
	after, err := converter.ObjectToTyped(asAPIVersion(upstream, tenantAPIVersion))
	if err != nil {
		return nil, err
	}
	comparison, err := before.Compare(after)
	if err != nil {
		return nil, err
	}
	// Added is what conversion introduced, Modified what it rewrote. Removed is
	// deliberately left out: a path conversion dropped cannot be extracted from
	// an object that no longer carries it.
	return comparison.Added.Union(comparison.Modified), nil
}

// asAPIVersion returns obj under the given apiVersion, copying only if it has to.
func asAPIVersion(obj *unstructured.Unstructured, apiVersion string) *unstructured.Unstructured {
	if apiVersion == "" || apiVersion == obj.GetAPIVersion() {
		return obj
	}
	renamed := obj.DeepCopy()
	renamed.SetAPIVersion(apiVersion)
	return renamed
}

// applyEntry finds what this manager owns as a result of applying.
//
// An Update entry is not enough: it means the object was written some other way,
// and the field set of an update is not a description of what was applied.
func applyEntry(obj *unstructured.Unstructured, fieldManager string) (metav1.ManagedFieldsEntry, bool) {
	if fieldManager == "" {
		return metav1.ManagedFieldsEntry{}, false
	}
	for _, entry := range obj.GetManagedFields() {
		if entry.Manager == fieldManager &&
			entry.Operation == metav1.ManagedFieldsOperationApply &&
			entry.Subresource == "" &&
			entry.FieldsV1 != nil {
			return entry, true
		}
	}
	return metav1.ManagedFieldsEntry{}, false
}
