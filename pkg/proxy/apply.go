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

	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// deducedTypeConverter reads an object with no schema to hand, which is what a
// custom resource is here.
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
// gives the same fragment the tenant sent with every reference rewritten -- and
// nothing else, because the field set says exactly what to take.
func (tp *tenantProxy) forwardApply(ctx context.Context, upstream *unstructured.Unstructured,
	fieldManager string, options *metav1.UpdateOptions) (*unstructured.Unstructured, error) {

	entry, found := applyEntry(upstream, fieldManager)
	if !found {
		return nil, nil
	}
	if tp.typeConverter == nil {
		// Without a schema the field set cannot be turned back into an object,
		// and guessing would mean applying fields nobody asked for.
		return nil, nil
	}

	fields := &fieldpath.Set{}
	if err := fields.FromJSON(entry.FieldsV1.GetRawReader()); err != nil {
		return nil, fmt.Errorf("reading the field set this apply owns: %w", err)
	}

	// managedFields never travel in a patch body, and upstream refuses a request
	// carrying them.
	stripped := upstream.DeepCopy()
	stripped.SetManagedFields(nil)

	typedObj, err := tp.typeConverter.ObjectToTyped(stripped)
	if err != nil {
		// A custom resource whose CRD carries no usable schema. Upstream reads
		// one the same way, treating every list as a whole rather than merging
		// it by key.
		typedObj, err = deducedTypeConverter.ObjectToTyped(stripped)
		if err != nil {
			klog.V(4).Infof("not forwarding this apply as an apply, its shape could not be read: %v", err)
			return nil, nil
		}
	}
	extracted, ok := typedObj.ExtractItems(fields.Leaves()).AsValue().Unstructured().(map[string]interface{})
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
