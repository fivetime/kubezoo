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

import "testing"

// TestServedResourcesIsNarrowerThanItsGroups is the property the whole change
// rests on: a group is not its contents.
//
// ⛔ What this exists to stop is measurable, and was measured. Of the 64 kinds a
// tenant's discovery advertised, eleven answered NotFound when addressed --
// among them the four machine-facing storage resources, which are withheld
// deliberately and which arrived only because their GROUP is served.
//
// ⚠️ Written against the group most likely to regress rather than against a
// count. A count would go red for any addition, including a correct one, and
// somebody would then update the number instead of reading it.
func TestServedResourcesIsNarrowerThanItsGroups(t *testing.T) {
	served := ServedAPIResources()

	storage := served["storage.k8s.io"]
	if len(storage) == 0 {
		t.Fatal("storage.k8s.io has no resources; the walk is not reading the storage configs")
	}
	for _, want := range []string{"storageclasses", "volumeattributesclasses"} {
		if !storage[want] {
			t.Errorf("storage.k8s.io does not list %s, which kubezoo does serve", want)
		}
	}
	// The four that are machine-facing. csinodes is named by the node it
	// describes and volumeattachments carries spec.nodeName, which is why they
	// went the way of Node -- and why discovery must not offer them.
	for _, unwanted := range []string{
		"csidrivers", "csinodes", "csistoragecapacities", "volumeattachments",
	} {
		if storage[unwanted] {
			t.Errorf("storage.k8s.io lists %s, which no storage config installs -- "+
				"a tenant told about it gets NotFound when it asks", unwanted)
		}
	}
}

// TestEveryServedGroupWithStorageHasResources guards the direction that fails
// silently.
//
// A group whose entry is missing is passed through UNFILTERED, by design: the
// groups installed outside the storage configs have nothing to read. That makes
// an empty or missing entry invisible -- discovery keeps working, and keeps
// advertising the resources this change removed. So the check is that every
// group built from a storage config has a non-empty set.
func TestEveryServedGroupWithStorageHasResources(t *testing.T) {
	served := ServedAPIResources()
	if len(served) < 5 {
		t.Fatalf("only %d groups were collected; the walk is not finding the group configs", len(served))
	}
	for group, resources := range served {
		if len(resources) == 0 {
			t.Errorf("group %q collected no resources, so its list is advertised unfiltered", group)
		}
	}
	// The groups a tenant cannot do without.
	for _, group := range []string{"", "apps", "batch", "rbac.authorization.k8s.io"} {
		if len(served[group]) == 0 {
			t.Errorf("group %q is served but collected no resources", group)
		}
	}
}
