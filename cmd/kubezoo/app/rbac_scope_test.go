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

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

// TestClusterScopedGrantsCoverWhatWeServe keeps the tenant's cluster-scoped RBAC
// grant in step with the resources kubezoo actually exposes.
//
// The grant is a hand-written list in kubezoo-contract, because the controller
// that builds the tenant's ClusterRole from it lives in a third repository and
// cannot see what this one serves. This test closes that loop from the side that
// can see both.
//
// Getting it wrong is quiet in both directions. Serving a cluster-scoped
// resource that the grant omits breaks tenants at runtime with a Forbidden that
// looks like a kubezoo bug. Granting one that is no longer served hands out
// permission for nothing, which is how "*" on "*" survives a refactor.
func TestClusterScopedGrantsCoverWhatWeServe(t *testing.T) {
	served := map[string]map[string]bool{}
	collect := func(g apiconfig.APIGroupConfig) {
		for _, resources := range g.StorageConfigs {
			for name, sc := range resources {
				// Connecters carry no scope of their own -- pods/log and
				// pods/exec are pod subresources whose namespace is rewritten in
				// the connecter's own path -- so they say nothing about
				// cluster-scoped grants.
				if sc.IsConnecter || sc.Resource == "" || sc.NamespaceScoped {
					continue
				}
				if served[g.Group] == nil {
					served[g.Group] = map[string]bool{}
				}
				served[g.Group][name] = true
			}
		}
	}
	collect(legacyGroup)
	for _, g := range nonLegacyGroups {
		collect(g)
	}

	granted := map[string]map[string]bool{}
	for _, rule := range common.ClusterScopedRules() {
		for _, group := range rule.APIGroups {
			if granted[group] == nil {
				granted[group] = map[string]bool{}
			}
			for _, resource := range rule.Resources {
				granted[group][resource] = true
			}
		}
	}

	// CRDs reach upstream through the CRD handler rather than the generic proxy,
	// so apigroups.go does not declare them while the grant must still carry
	// them. Tenants do create CRDs.
	exemptFromServed := map[string]map[string]bool{
		"apiextensions.k8s.io": {"customresourcedefinitions": true, "customresourcedefinitions/status": true},
	}

	// Serving a resource is not a reason to authorize tenants for it. The grant
	// was once derived straight from the served surface, which is how
	// nodes/proxy came to be granted and gave tenants the kubelet API on every
	// node. Refusals are listed explicitly so they read as decisions, and so
	// that an accidental gap still fails below.
	refused := map[string]map[string]bool{}
	for group, resources := range common.NotGrantedToTenants() {
		refused[group] = map[string]bool{}
		for _, resource := range resources {
			refused[group][resource] = true
		}
	}

	var missing, extra []string
	for group, resources := range served {
		for resource := range resources {
			if !granted[group][resource] && !refused[group][resource] {
				missing = append(missing, group+"/"+resource)
			}
		}
	}
	for group, resources := range granted {
		for resource := range resources {
			if served[group][resource] || exemptFromServed[group][resource] {
				continue
			}
			extra = append(extra, group+"/"+resource)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	for _, r := range missing {
		t.Errorf("apigroups.go serves cluster-scoped %s but the tenant grant omits it and it is "+
			"not in notGrantedToTenants; either grant it or refuse it on purpose, with a reason", r)
	}
	for _, r := range extra {
		t.Errorf("the tenant grant covers cluster-scoped %s, which apigroups.go no longer "+
			"serves; drop it rather than leaving the permission behind", r)
	}
}
