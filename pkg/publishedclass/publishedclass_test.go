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

package publishedclass

import (
	"sort"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

func classStore(t *testing.T, classes map[string]string) cache.Store {
	t.Helper()
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for name, published := range classes {
		object := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if published != "" {
			object.Labels = map[string]string{common.StorageClassPublishedLabelKey: published}
		}
		if err := store.Add(object); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
	}
	return store
}

func synced(b bool) cache.InformerSynced { return func() bool { return b } }

// TestLabelDrivesPublication is the point of the whole mechanism: what a tenant
// sees follows a label on the object, so publishing a class needs no restart.
//
// ⚠️ It used to be a startup flag. On the shipped single-replica StatefulSet that
// made "offer one more storage class" a full control-plane outage for every
// tenant, and a typo in the flag was completely silent -- the name simply never
// appeared, with no error anywhere.
func TestLabelDrivesPublication(t *testing.T) {
	store := classStore(t, map[string]string{
		"fast-ssd":          common.PublishedTrue,
		"legacy-spinning":   common.PublishedDeprecated,
		"platform-internal": "", // no label at all
	})
	set := New("storageclass", common.StorageClassPublishedLabelKey, store, synced(true), nil)

	if !set.Visible("fast-ssd") {
		t.Error("a class labelled true is not visible")
	}
	if set.Retired("fast-ssd") {
		t.Error("a class labelled true was reported retired")
	}

	// ⭐ Retired is visible. A tenant that already references it has to be able
	// to see that it exists and that it is going away; hiding it would leave an
	// unexplainable reference.
	if !set.Visible("legacy-spinning") {
		t.Error("a retired class is invisible; the tenant cannot explain its own PVC")
	}
	if !set.Retired("legacy-spinning") {
		t.Error("a class labelled deprecated was not reported retired")
	}

	if set.Visible("platform-internal") {
		t.Error("an unlabelled class is visible")
	}

	names := set.Names()
	sort.Strings(names)
	if len(names) != 2 || names[0] != "fast-ssd" || names[1] != "legacy-spinning" {
		t.Errorf("Names() = %v, want [fast-ssd legacy-spinning]", names)
	}
}

// TestFlagStillPublishes keeps an upgrade from silently un-publishing everything.
// An operator running with --public-storage-classes and no labels yet must not
// find that tenants suddenly see nothing.
func TestFlagStillPublishes(t *testing.T) {
	set := New("storageclass", common.StorageClassPublishedLabelKey,
		classStore(t, map[string]string{"labelled": common.PublishedTrue}),
		synced(true), []string{"from-the-flag"})

	if !set.Visible("from-the-flag") {
		t.Error("a class named on the command line is not visible")
	}
	if set.Retired("from-the-flag") {
		t.Error("a flag-named class cannot be retired; there is nowhere to say so")
	}
	if !set.Visible("labelled") {
		t.Error("the label and the flag must be a union, not a choice")
	}
	if len(set.Names()) != 2 {
		t.Errorf("Names() = %v, want both", set.Names())
	}
}

// TestUnsyncedCacheDoesNotInventAnswers pins the startup window.
//
// Before the cache has filled, the store is legitimately empty -- and "empty"
// is indistinguishable from "the platform published nothing". Answering from it
// would tell a tenant, truthfully as far as the cache knows, that a class it has
// been using does not exist. The server gates readiness on HasSynced for exactly
// this reason; this is the belt to that braces.
func TestUnsyncedCacheDoesNotInventAnswers(t *testing.T) {
	store := classStore(t, map[string]string{"fast-ssd": common.PublishedTrue})
	set := New("storageclass", common.StorageClassPublishedLabelKey, store, synced(false),
		[]string{"from-the-flag"})

	if set.HasSynced() {
		t.Fatal("an unsynced cache reported itself synced")
	}
	if set.Visible("fast-ssd") {
		t.Error("answered from a cache that has not filled")
	}
	// The static half is knowable without the cache and stays available.
	if !set.Visible("from-the-flag") {
		t.Error("a flag-named class should not depend on the cache")
	}
}

// TestAnyValueCountsAsPublished -- an operator who writes something other than
// the two known values has still said "offer this". Treating an unrecognised
// value as unpublished would put back the silent failure the label removes.
func TestAnyValueCountsAsPublished(t *testing.T) {
	set := New("storageclass", common.StorageClassPublishedLabelKey,
		classStore(t, map[string]string{"odd": "yes-please"}), synced(true), nil)

	if !set.Visible("odd") {
		t.Error("an unrecognised label value hid the class, which is the failure this replaced")
	}
	if set.Retired("odd") {
		t.Error("only the deprecated value means retired")
	}
}
