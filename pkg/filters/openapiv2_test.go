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

package filters

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	ownTenant   = "111111"
	otherTenant = "222222"
)

// A cut-down document with the three places a group name appears: the path key,
// the $ref inside it, and the x-kubernetes-group-version-kind extension. The
// first version of this filter rewrote only the keys, which left the refs
// dangling and kubectl explain broken.
const documentFixture = `{
  "paths": {
    "/apis/111111-acme.io/v1/widgets": {
      "get": {"responses": {"200": {"schema": {"$ref": "#/definitions/io.111111-acme.v1.WidgetList"}}}}
    },
    "/apis/222222-beta.io/v1/gadgets": {
      "get": {"responses": {"200": {"schema": {"$ref": "#/definitions/io.222222-beta.v1.GadgetList"}}}}
    },
    "/apis/apps/v1/namespaces/{namespace}/deployments": {
      "get": {"responses": {"200": {"schema": {"$ref": "#/definitions/io.k8s.api.apps.v1.DeploymentList"}}}}
    }
  },
  "definitions": {
    "io.111111-acme.v1.Widget": {
      "x-kubernetes-group-version-kind": [{"group": "111111-acme.io", "kind": "Widget", "version": "v1"}]
    },
    "io.111111-acme.v1.WidgetList": {"type": "object"},
    "io.222222-beta.v1.Gadget": {
      "x-kubernetes-group-version-kind": [{"group": "222222-beta.io", "kind": "Gadget", "version": "v1"}]
    },
    "io.222222-beta.v1.GadgetList": {"type": "object"},
    "io.k8s.api.apps.v1.DeploymentList": {"type": "object"},
    "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta": {"type": "object"}
  }
}`

func filterFixture(t *testing.T, tenantID string) (map[string]interface{}, string) {
	t.Helper()
	filtered, err := filterOpenAPIV2([]byte(documentFixture), tenantID)
	if err != nil {
		t.Fatalf("filterOpenAPIV2: %v", err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(filtered, &document); err != nil {
		t.Fatalf("filtered document does not parse: %v", err)
	}
	return document, string(filtered)
}

// TestDropsOtherTenantsEntirely is the leak: without this a tenant reads every
// other tenant's id, group and kind out of a document it is entitled to fetch.
func TestDropsOtherTenantsEntirely(t *testing.T) {
	_, raw := filterFixture(t, ownTenant)

	if strings.Contains(raw, otherTenant) {
		t.Errorf("tenant %s appears in the document served to tenant %s:\n%s",
			otherTenant, ownTenant, raw)
	}
	if strings.Contains(raw, "Gadget") || strings.Contains(raw, "beta.io") {
		t.Errorf("another tenant's kind or group survived:\n%s", raw)
	}
}

// TestStripsOwnPrefixEverywhere covers the half that key rewriting missed: a
// path, its $ref and the group-version-kind extension all have to agree, or the
// document is internally inconsistent and explain cannot resolve the type.
func TestStripsOwnPrefixEverywhere(t *testing.T) {
	document, raw := filterFixture(t, ownTenant)

	if strings.Contains(raw, ownTenant+"-") {
		t.Errorf("the tenant's own prefix survived somewhere:\n%s", raw)
	}

	paths := document["paths"].(map[string]interface{})
	if _, ok := paths["/apis/acme.io/v1/widgets"]; !ok {
		t.Errorf("own path was not rewritten; got %v", keysOf(paths))
	}
	definitions := document["definitions"].(map[string]interface{})
	if _, ok := definitions["io.acme.v1.Widget"]; !ok {
		t.Errorf("own definition was not rewritten; got %v", keysOf(definitions))
	}

	// Every $ref must resolve against the definitions that remain. This is the
	// assertion that would have caught the key-only rewrite.
	for _, ref := range refsIn(raw) {
		name := strings.TrimPrefix(ref, "#/definitions/")
		if _, ok := definitions[name]; !ok {
			t.Errorf("$ref %q does not resolve; definitions are %v", ref, keysOf(definitions))
		}
	}
}

// TestLeavesNativeSurfaceAlone -- the prefix test is positional, so a native
// name of about the right shape must not be mistaken for a tenant's.
func TestLeavesNativeSurfaceAlone(t *testing.T) {
	document, _ := filterFixture(t, ownTenant)

	paths := document["paths"].(map[string]interface{})
	if _, ok := paths["/apis/apps/v1/namespaces/{namespace}/deployments"]; !ok {
		t.Errorf("a native path was dropped or rewritten; got %v", keysOf(paths))
	}
	definitions := document["definitions"].(map[string]interface{})
	for _, key := range []string{
		"io.k8s.api.apps.v1.DeploymentList",
		"io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta",
	} {
		if _, ok := definitions[key]; !ok {
			t.Errorf("native definition %s was dropped or rewritten", key)
		}
	}
}

// TestTheOtherTenantSeesTheMirrorImage is the control that makes the two above
// mean something: neither tenant is privileged by the filter.
func TestTheOtherTenantSeesTheMirrorImage(t *testing.T) {
	document, raw := filterFixture(t, otherTenant)

	if strings.Contains(raw, ownTenant) || strings.Contains(raw, "Widget") {
		t.Errorf("tenant %s's entries leaked to tenant %s:\n%s", ownTenant, otherTenant, raw)
	}
	paths := document["paths"].(map[string]interface{})
	if _, ok := paths["/apis/beta.io/v1/gadgets"]; !ok {
		t.Errorf("own path missing for tenant %s; got %v", otherTenant, keysOf(paths))
	}
}

func keysOf(m map[string]interface{}) []string {
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func refsIn(raw string) []string {
	var refs []string
	for _, part := range strings.Split(raw, `"$ref":"`)[1:] {
		if end := strings.Index(part, `"`); end >= 0 {
			refs = append(refs, part[:end])
		}
	}
	return refs
}
