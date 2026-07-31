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

package app

import (
	"fmt"
	"github.com/fivetime/kubezoo-proxy/pkg/apiconfig"
	"reflect"
	"testing"

)

// subresourcesWithTheirOwnBody lists the subresources whose request body is not
// the parent resource, and what it is instead.
//
// Most subresources take their parent -- pods/status is a Pod -- so the table
// generally reuses the parent's NewFunc. These do not, and getting it wrong is
// not subtle at runtime: the request fails at decode with "X in version v1
// cannot be handled as a Y: unknown conversion", which is what
// `kubectl create token`, evictions and bindings all did.
//
// The tell, when it was found, was that scale already had this right through
// its own GroupVersionKindFunc. One instance had been handled rather than the
// class, so this test names the class.
var subresourcesWithTheirOwnBody = map[string]string{
	"pods/eviction":         "Eviction",
	"pods/binding":          "Binding",
	"serviceaccounts/token": "TokenRequest",
	// */scale is covered by scaleSubresources below, since the type name is
	// shared but the containing group version is not.
}

// scaleSubresources all take an autoscaling Scale.
var scaleSubresources = "Scale"

func TestSubresourcesDecodeTheirOwnBody(t *testing.T) {
	all := map[string]*apiconfig.StorageConfig{}
	collect := func(g apiconfig.APIGroupConfig) {
		for version, resources := range g.StorageConfigs {
			for name, storageConfig := range resources {
				all[fmt.Sprintf("%s/%s/%s", g.Group, version, name)] = storageConfig
			}
		}
	}
	collect(legacyGroup)
	for _, g := range nonLegacyGroups {
		collect(g)
	}

	for key, storageConfig := range all {
		if storageConfig.Subresource == "" || storageConfig.IsConnecter || storageConfig.NewFunc == nil {
			continue
		}
		resourcePath := storageConfig.Resource + "/" + storageConfig.Subresource
		want, special := subresourcesWithTheirOwnBody[resourcePath]
		if !special && storageConfig.Subresource == "scale" {
			want, special = scaleSubresources, true
		}
		got := reflect.TypeOf(storageConfig.NewFunc()).Elem().Name()

		if special {
			if got != want {
				t.Errorf("%s: NewFunc returns %s, want %s; the body of this subresource is not its "+
					"parent, so decoding it as the parent fails the request outright", key, got, want)
			}
			continue
		}
		// Everything else should decode as its parent. Catching a new
		// divergent subresource here is the point: it forces a decision rather
		// than leaving it to fail at runtime.
		if got != storageConfig.Kind.Kind {
			t.Errorf("%s: NewFunc returns %s but the declared kind is %s; if this subresource takes a "+
				"different body, add it to subresourcesWithTheirOwnBody with its GroupVersionKindFunc",
				key, got, storageConfig.Kind.Kind)
		}
	}

	// Guard the guard: every entry above must correspond to a served
	// subresource, so a renamed or dropped one does not sit here passing.
	for resourcePath := range subresourcesWithTheirOwnBody {
		var found bool
		for _, storageConfig := range all {
			if storageConfig.Resource+"/"+storageConfig.Subresource == resourcePath {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subresourcesWithTheirOwnBody names %s, which is no longer served", resourcePath)
		}
	}
}
