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

package app

import "strings"

// sharedCRDGroups are the platform's own CustomResourceDefinition groups that
// tenants address under their real names.
//
// ⛔ THIS IS AN ALLOWLIST AND HAS TO STAY ONE. Every other CRD a tenant can
// reach carries the tenant's prefix on its API group, which is what makes it the
// tenant's: no other tenant can name it. An entry here gives up that property
// for one group on purpose, so what stands in its place is a review of the
// controller behind it -- who reconciles a tenant's spec, with whose
// credentials, reading which fields.
//
// TODO-kaaas.md §W deferred sharing system CRDs for exactly that reason, and
// named the condition for revisiting it: a real consumer. Volume snapshots are
// the first one, because the CRD arrives with the platform's storage rather than
// with a tenant. docs/design-volumesnapshot-cn.md is the review; it is what an
// entry here costs.
//
// ⚠️ Being addressable is not being permitted. Which resources of a shared group
// a tenant may touch, and what happens to the fields inside them, is decided
// elsewhere -- see refuseUnpublishedSnapshotClass and
// refusePreProvisionedSnapshot in pkg/proxy, and pkg/convert/volumesnapshot.go.
// This map only answers "does this group resolve at all".
// ⚠️ Keyed by RESOURCE, not only by group, and that is load-bearing rather than
// tidy. Making a group resolvable makes every resource in it addressable, and
// one resource of this group is the escape the design refuses.
var sharedCRDResources = map[string]map[string]bool{
	"snapshot.storage.k8s.io": {
		"volumesnapshots": true,
		// A tenant chooses a class, so it has to be able to read the ones the
		// platform published. Writes are refused separately: these are the
		// platform's objects, like a StorageClass.
		"volumesnapshotclasses": true,

		// ⛔ volumesnapshotcontents is absent DELIBERATELY and must not be added.
		//
		// spec.source.snapshotHandle on a content is the real handle on the
		// storage system. A tenant able to create one could name another tenant's
		// snapshot, point volumeSnapshotRef back at itself -- which passes every
		// check upstream makes, and upstream does check the namespace -- and
		// restore a PersistentVolumeClaim from it. That is the PersistentVolume
		// escape again with snapshotHandle where nfs.server was.
		//
		// ⚠️ And the fix from that one does NOT carry over. There, requiring a
		// claimRef reserved the volume for its owner. Here volumeSnapshotRef only
		// says who the content binds to; snapshotHandle is the payload, and
		// pointing the reference at yourself is entirely legitimate while the
		// data is still someone else's. Pre-provisioning is a data-import
		// primitive and stays platform-only.
	},
}

// sharedCRD reports whether a CRD name -- <plural>.<group> -- is one the platform
// shares with every tenant under its real name.
func sharedCRD(crdName string) bool {
	plural, group, found := strings.Cut(crdName, ".")
	if !found {
		return false
	}
	return sharedCRDResources[group][plural]
}

// sharedCRDGroup reports whether a group has any shared resource, which is what
// discovery needs to decide whether to advertise it at all.
func sharedCRDGroup(group string) bool {
	return len(sharedCRDResources[group]) > 0
}
