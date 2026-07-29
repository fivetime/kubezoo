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

package util

import (
	"os"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestGroupKindNamespacedMatchesUpstream checks the hand-maintained scope table
// against a real kube-apiserver of the version we target.
//
// The table decides which half of the tenant rewriting an object reference gets:
// a cluster-scoped kind has the tenant ID prefixed onto its *name*, a namespaced
// one does not, because its namespace carries the prefix instead. Marking a
// cluster-scoped kind as namespaced therefore drops the only thing separating two
// tenants that pick the same name for the same cluster-scoped object. The reverse
// mistake prefixes a name that should not be, which dangles the reference. A kind
// missing from the table is the mildest case: IsGroupKindNamespaced errors and
// NewCheckGroupKindFunc falls through to "unregistered crd group".
//
// Needs envtest binaries, so it runs under `make test-integration` and skips in
// the unit run.
func TestGroupKindNamespacedMatchesUpstream(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("no envtest assets; run via make test-integration")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the control plane: %v", err)
	}
	defer func() { _ = env.Stop() }()

	_, lists, err := discovery.NewDiscoveryClientForConfigOrDie(cfg).ServerGroupsAndResources()
	if err != nil {
		// Discovery is partial when an aggregated API is unreachable, which says
		// nothing about the core groups this table covers.
		t.Logf("partial discovery, continuing: %v", err)
	}

	// Only top-level resources are the ground truth here, because only they can be
	// the target of an owner or object reference, which is the sole thing the table
	// is consulted for. Subresources are tracked separately: their kinds are request
	// bodies -- PodExecOptions, Eviction, TokenRequest -- never stored objects, so
	// they have no business in the table, but a kind that appears only there is not
	// retired either and must not be reported as stale. Scale is the case that
	// matters: it has no top-level resource, and discovery files it under whichever
	// group owns the parent, apps for deployments/scale and core for
	// replicationcontrollers/scale, neither of which is the autoscaling group the
	// table files it under.
	upstream := map[metav1.GroupKind]bool{}
	subresourceOnly := map[string]bool{}
	for _, list := range lists {
		if list == nil {
			continue
		}
		group := ""
		if i := strings.Index(list.GroupVersion, "/"); i >= 0 {
			group = list.GroupVersion[:i]
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") {
				subresourceOnly[r.Kind] = true
				continue
			}
			gk := metav1.GroupKind{Group: group, Kind: r.Kind}
			if was, seen := upstream[gk]; seen && was != r.Namespaced {
				t.Errorf("upstream disagrees with itself on the scope of %+v; the "+
					"table cannot represent that, since it is keyed by group and kind alone", gk)
			}
			upstream[gk] = r.Namespaced
		}
	}
	if len(upstream) == 0 {
		t.Fatal("discovery returned nothing; the comparisons below would pass vacuously")
	}
	for gk := range upstream {
		delete(subresourceOnly, gk.Kind)
	}

	for gk, wantNamespaced := range upstream {
		got, err := IsGroupKindNamespaced(gk)
		if err != nil {
			t.Errorf("%s/%s is served upstream but missing from groupKindNamespaced; "+
				"references to it fail with 'unregistered crd group'", gk.Group, gk.Kind)
			continue
		}
		if got != wantNamespaced {
			t.Errorf("%s/%s: table says namespaced=%v, upstream says %v -- "+
				"getting this backwards breaks tenant isolation on names",
				gk.Group, gk.Kind, got, wantNamespaced)
		}
	}

	var stale []string
	for gk := range groupKindNamespaced {
		if _, ok := upstream[gk]; ok {
			continue
		}
		if subresourceOnly[gk.Kind] {
			continue
		}
		stale = append(stale, gk.Group+"/"+gk.Kind)
	}
	sort.Strings(stale)
	for _, gk := range stale {
		t.Errorf("groupKindNamespaced still lists %s, which this Kubernetes version "+
			"does not serve", gk)
	}
}
