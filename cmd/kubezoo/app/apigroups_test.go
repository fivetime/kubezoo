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
	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	master "k8s.io/kubernetes/pkg/controlplane"

	"github.com/fivetime/kubezoo-gateway/pkg/proxy"
)

// allDeclaredGroups is every group kubezoo exposes to tenants, legacy and not.
func allDeclaredGroups() []apiconfig.APIGroupConfig {
	return append([]apiconfig.APIGroupConfig{legacyGroup}, nonLegacyGroups...)
}

// TestDeclaredGroupVersionsAreServedUpstream guards apigroups.go against drifting
// away from the Kubernetes release we target.
//
// The table is hand-maintained, and nothing used to notice when a group-version
// it named was retired: kubezoo would still install a REST endpoint, which then
// proxied to an upstream path that answers 404. Before this test the table still
// carried twelve of them, extensions/v1beta1 among others, four years after that
// group was removed.
//
// DefaultAPIResourceConfigSource is the set of group-versions a kube-apiserver of
// our target version knows about at all. Anything we declare outside it cannot be
// served by the upstream we proxy to, whatever --runtime-config says.
func TestDeclaredGroupVersionsAreServedUpstream(t *testing.T) {
	known := master.DefaultAPIResourceConfigSource().GroupVersionConfigs

	var unknown []string
	for _, g := range allDeclaredGroups() {
		for version := range g.StorageConfigs {
			gv := schema.GroupVersion{Group: g.Group, Version: version}
			if _, ok := known[gv]; !ok {
				unknown = append(unknown, gv.String())
			}
		}
	}
	sort.Strings(unknown)
	for _, gv := range unknown {
		t.Errorf("apigroups.go declares %s, which kube-apiserver does not know at "+
			"this version; requests would proxy upstream and 404", gv)
	}
}

// TestDeclaredResourcesSurviveTheResourceConfig is the stricter of the two, and
// it works in both directions.
//
// NewRESTStorage now filters resources through the APIResourceConfigSource, so a
// mistake there would silently stop serving resources rather than serving too
// many; everything the table declares must still come out the far side.
//
// It also catches what the test above cannot. That one only sees group-versions
// the apiserver has never heard of. A version can instead still be known but be
// off by default and, in the meantime, have been repurposed for entirely
// different resources -- coordination.k8s.io/v1beta1 served leases at our fork
// point and serves leasecandidates now. Declaring one of those fails here,
// because a disabled group-version enables none of its resources.
//
// The invariant is therefore: kubezoo only declares what a default kube-apiserver
// serves. Declaring anything that needs --runtime-config would have to relax it.
func TestDeclaredResourcesSurviveTheResourceConfig(t *testing.T) {
	src := master.DefaultAPIResourceConfigSource()

	for _, g := range allDeclaredGroups() {
		providers, err := proxy.NewRESTStorageProviders(g)
		if err != nil {
			t.Fatalf("group %q: %v", g.Group, err)
		}
		info, err := providers[0].NewRESTStorage(src, nil)
		if err != nil {
			t.Fatalf("group %q: %v", g.Group, err)
		}
		for version, declared := range g.StorageConfigs {
			installed := info.VersionedResourcesStorageMap[version]
			for resource := range declared {
				if _, ok := installed[resource]; !ok {
					t.Errorf("%s/%s: declared resource %q was dropped by the resource config",
						g.Group, version, resource)
				}
			}
		}
	}
}
