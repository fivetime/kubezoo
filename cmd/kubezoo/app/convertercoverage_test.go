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

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

// TestEveryConvertedKindIsServed is the direction nothing checked.
//
// TestEveryServedPodCarrierIsPlaced asks "is everything served also handled".
// This asks the opposite: is everything handled also served. A convertor keyed
// on a kind apigroups.go does not serve can never run, and that is worse than
// dead code -- it reads as "this kind is handled" to whoever looks next.
//
// ⭐ Not hypothetical. storage.k8s.io/VolumeAttachment was registered while the
// resource was unserved, and the transformer behind it refused any PV name
// without the tenant prefix -- the exact bug that made a dynamically
// provisioned PersistentVolumeClaim unreadable. Had the resource ever started
// being served, it would have shipped with that bug already in it and passing
// unit tests beside it. init.go's own comment says the same thing about the
// PriorityClass entry that was removed for this reason.
func TestEveryConvertedKindIsServed(t *testing.T) {
	served := map[schema.GroupKind]bool{}
	for _, group := range append([]apiconfig.APIGroupConfig{legacyGroup}, nonLegacyGroups...) {
		for _, byResource := range group.StorageConfigs {
			for _, config := range byResource {
				if config.Resource == "" {
					continue
				}
				served[schema.GroupKind{Group: config.Kind.Group, Kind: config.Kind.Kind}] = true
			}
		}
	}

	// Kinds kubezoo converts without apigroups.go declaring them, each because
	// something else installs the storage. A new entry here needs a reason, not
	// just a line.
	servedElsewhere := map[schema.GroupKind]string{
		{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}: "reaches upstream through the CRD handler, not the generic proxy",
	}

	native, _ := convert.InitConvertors(nil, nil, nil, nil)
	kinds := convert.ConvertedKinds(native)

	// ⚠️ A walk that finds nothing passes in silence, which is the failure this
	// test exists to prevent.
	if len(kinds) < 10 {
		t.Fatalf("only %d converted kinds were found; the convertor map is not being read", len(kinds))
	}

	for _, gk := range kinds {
		if served[gk] {
			continue
		}
		if why, ok := servedElsewhere[gk]; ok {
			t.Logf("%s is converted but not declared in apigroups.go: %s", gk, why)
			continue
		}
		t.Errorf("%s has a convertor but apigroups.go does not serve it, so the convertor "+
			"can never run. Either serve the resource or drop the entry -- one that cannot "+
			"match reads as \"this kind is handled\" and hides whatever the transformer "+
			"would have got wrong", gk)
	}
}
