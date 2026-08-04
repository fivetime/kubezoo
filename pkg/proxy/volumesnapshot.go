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

package proxy

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	snapshotGroup       = "snapshot.storage.k8s.io"
	volumeSnapshotKind  = "VolumeSnapshot"
	snapshotClassSource = "volumeSnapshotClassName"
)

// volumeSnapshotOf returns the object as a VolumeSnapshot, or nil if it is
// something else.
//
// ⚠️ Unstructured, because a VolumeSnapshot is a custom resource: it reaches
// this proxy through the shared-CRD path, where nothing is converted into a Go
// type. Matching on the group AND the kind rather than on the kind alone --
// "VolumeSnapshot" is not a reserved word, and a tenant may define a CRD of its
// own by that name in its own group.
func volumeSnapshotOf(obj runtime.Object) *unstructured.Unstructured {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	gvk := u.GroupVersionKind()
	if gvk.Group != snapshotGroup || gvk.Kind != volumeSnapshotKind {
		return nil
	}
	return u
}

// refusePreProvisionedSnapshot stops a tenant importing a snapshot that already
// exists on the storage system.
//
// ⛔ THE ESCAPE THIS WHOLE INTEGRATION IS SHAPED AROUND. A pre-provisioned
// VolumeSnapshot names a VolumeSnapshotContent, and a content carries
// spec.source.snapshotHandle -- the real handle on the storage system. A tenant
// able to produce that pair could name ANOTHER TENANT'S snapshot, point the
// content's volumeSnapshotRef back at its own snapshot, and restore a
// PersistentVolumeClaim from it.
//
// ⚠️ Every check upstream makes would pass. getPreprovisionedContentFromStore
// compares the reference's name, namespace and UID against the snapshot -- and
// they match, because the tenant is pointing at its own snapshot. What it does
// not check, because it has no reason to, is whether the HANDLE was ever the
// tenant's. That is the same shape as the PersistentVolume escape, with
// snapshotHandle where nfs.server was, and the fix from that one does not carry
// over: requiring a reference back reserves the object, it does not stop the
// payload from being someone else's.
//
// kubezoo does not serve volumesnapshotcontents at all, which is the other half.
// This is the half that stops a tenant naming one anyway -- a content the
// PLATFORM created for some other tenant is a perfectly real object, and without
// this the tenant would only need its name.
//
// ⭐ CREATE only, and for the same reason as the storage class check:
// spec.source is immutable upstream, so a tenant cannot repair a snapshot it
// already has, and refusing on update would fail every later write -- a label, a
// finalizer -- to an object that is not going to change.
func (tp *tenantProxy) refusePreProvisionedSnapshot(obj runtime.Object) error {
	u := volumeSnapshotOf(obj)
	if u == nil {
		return nil
	}
	name, found, err := unstructured.NestedString(u.Object, "spec", "source", "volumeSnapshotContentName")
	if err != nil || !found || name == "" {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: snapshotGroup, Kind: volumeSnapshotKind},
		u.GetName(),
		field.ErrorList{field.Forbidden(
			field.NewPath("spec", "source", "volumeSnapshotContentName"),
			"a pre-existing snapshot cannot be imported: it names a VolumeSnapshotContent, "+
				"which carries the handle of a snapshot on the storage system and so is the "+
				"platform's to create. Take a snapshot of your own PersistentVolumeClaim "+
				"instead, with spec.source.persistentVolumeClaimName",
		)},
	)
}

// refuseUnpublishedSnapshotClass refuses a snapshot on a class the platform is
// not offering.
//
// A VolumeSnapshotClass names the driver, the deletion policy and the
// parameters -- including which of the platform's secrets the snapshotter uses.
// Naming one the platform did not publish is the same thing as naming an
// unpublished VolumeAttributesClass: a tier the tenant was not sold. Publication
// is the authorization, exactly as it is for storage classes.
//
// ⚠️ An empty class is left alone, and that is a hole by construction rather
// than an oversight: upstream picks the class marked default for the driver,
// published or not. Retiring a default snapshot class therefore means clearing
// that annotation too. Refusing empty would refuse the ordinary case, which is
// how storage classes ended up with the same shape.
func (tp *tenantProxy) refuseUnpublishedSnapshotClass(obj runtime.Object) error {
	if tp.publishedSnapshotClasses == nil {
		return nil
	}
	u := volumeSnapshotOf(obj)
	if u == nil {
		return nil
	}
	name, found, err := unstructured.NestedString(u.Object, "spec", snapshotClassSource)
	if err != nil || !found || name == "" {
		return nil
	}
	// Before the cache has filled, empty is indistinguishable from "nothing is
	// published", so answering from it would refuse every snapshot for the first
	// seconds after a restart. Unavailable rather than Invalid: clients retry it,
	// an operator can tell it from a real refusal, and the boundary does not open
	// during the one window an attacker can arrange.
	if !tp.publishedSnapshotClasses.HasSynced() {
		return apierrors.NewServiceUnavailable(
			"the list of published volume snapshot classes is not ready yet; retry")
	}
	if tp.publishedSnapshotClasses.Retired(name) {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: snapshotGroup, Kind: volumeSnapshotKind}, u.GetName(),
			field.ErrorList{field.Invalid(
				field.NewPath("spec", snapshotClassSource), name,
				fmt.Sprintf("volume snapshot class %q is being retired and is not taking new "+
					"snapshots; snapshots already taken on it are unaffected", name),
			)},
		)
	}
	if !tp.publishedSnapshotClasses.Visible(name) {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: snapshotGroup, Kind: volumeSnapshotKind}, u.GetName(),
			field.ErrorList{field.Invalid(
				field.NewPath("spec", snapshotClassSource), name,
				fmt.Sprintf("no volume snapshot class %q is available to this tenant", name),
			)},
		)
	}
	return nil
}
