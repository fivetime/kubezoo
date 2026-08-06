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
	"strings"
	"testing"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
)

// TestNoSubresourceCarriesAShortName -- a subresource inherited its parent's
// short name, and every kubectl command paid for it.
//
// ⛔ Measured against a tenant, and against the upstream apiserver for contrast:
//
//	upstream /api/v1:  pods ["po"]
//	kubezoo  /api/v1:  pods ["po"], pods/binding ["po"], pods/eviction ["po"],
//	                   pods/ephemeralcontainers ["po"], pods/resize ["po"],
//	                   pods/status ["po"]
//
// kubectl resolves a short name through discovery, so six resources claiming
// "po" makes it print one "short name po could also match lower priority
// resource ..." per extra match, on every `kubectl get po`. Upstream gives
// short names to the parent only.
//
// ⚠️ Nothing was broken -- kubectl still picks the parent -- which is exactly
// why it survived: the output is noise, not an error, and noise is what people
// learn to read past.
//
// ⭐ Checked over the built configuration rather than the source text. The fix
// itself was a scripted edit over apigroups.go and it went wrong twice, both
// times by deleting the PARENT's short name: entry boundaries in that file are
// not what a brace count or a line window says they are. What is unambiguous is
// the map key -- a subresource is keyed "parent/sub" -- and that is what both
// the fix and this test use.
func TestNoSubresourceCarriesAShortName(t *testing.T) {
	checked, offenders := 0, []string(nil)

	groups := append([]apiconfig.APIGroupConfig{legacyGroup}, nonLegacyGroups...)
	for _, cfg := range groups {
		for version, byResource := range cfg.StorageConfigs {
			for key, sc := range byResource {
				if !strings.Contains(key, "/") || sc == nil {
					continue
				}
				checked++
				if len(sc.ShortNames) > 0 {
					offenders = append(offenders,
						cfg.Group+"/"+version+" "+key+" ["+strings.Join(sc.ShortNames, ",")+"]")
				}
			}
		}
	}

	// ⚠️ A walk that finds no subresources at all would pass in silence, which is
	// the failure this test exists to prevent.
	if checked < 20 {
		t.Fatalf("only %d subresource entries were walked; the configuration is not being read", checked)
	}
	for _, o := range offenders {
		t.Errorf("subresource carries a short name: %s\n"+
			"kubectl resolves short names through discovery, so this makes every "+
			"`kubectl get <short>` print a \"could also match lower priority resource\" "+
			"warning. Upstream gives short names to the parent resource only.", o)
	}
}

// TestParentResourcesKeepTheirShortNames is the other half, and it is here
// because the fix removed them twice by accident. Without it, a regression that
// strips every short name would leave the test above perfectly green.
func TestParentResourcesKeepTheirShortNames(t *testing.T) {
	want := map[string]string{
		"pods": "po", "nodes": "no", "services": "svc",
		"replicationcontrollers": "rc", "configmaps": "cm",
	}
	found := map[string]bool{}
	for _, byResource := range legacyGroup.StorageConfigs {
		for key, sc := range byResource {
			if sc == nil {
				continue
			}
			if short, ok := want[key]; ok {
				for _, s := range sc.ShortNames {
					if s == short {
						found[key] = true
					}
				}
			}
		}
	}
	for resource, short := range want {
		if !found[resource] {
			t.Errorf("%s no longer offers the short name %q", resource, short)
		}
	}
}
