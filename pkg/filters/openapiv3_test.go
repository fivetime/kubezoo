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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// kubezoo's own index: the native surface it actually serves.
const ownIndexFixture = `{"paths": {
  "api/v1":            {"serverRelativeURL": "/openapi/v3/api/v1?hash=aaa"},
  "apis/apps/v1":      {"serverRelativeURL": "/openapi/v3/apis/apps/v1?hash=bbb"}
}}`

// Upstream's index: the same native groups as the real apiserver serves them,
// plus every tenant's custom resources.
const upstreamIndexFixture = `{"paths": {
  "api/v1":                     {"serverRelativeURL": "/openapi/v3/api/v1?hash=zzz"},
  "apis/apps/v1":               {"serverRelativeURL": "/openapi/v3/apis/apps/v1?hash=yyy"},
  "apis/resource.k8s.io/v1":    {"serverRelativeURL": "/openapi/v3/apis/resource.k8s.io/v1?hash=xxx"},
  "apis/111111-acme.io/v1":     {"serverRelativeURL": "/openapi/v3/apis/111111-acme.io/v1?hash=ccc"},
  "apis/222222-beta.io/v1":     {"serverRelativeURL": "/openapi/v3/apis/222222-beta.io/v1?hash=ddd"}
}}`

func mergedIndex(t *testing.T, tenantID string) (map[string]interface{}, string) {
	t.Helper()
	merged, err := mergeOpenAPIV3Index([]byte(ownIndexFixture), []byte(upstreamIndexFixture), tenantID)
	if err != nil {
		t.Fatalf("mergeOpenAPIV3Index: %v", err)
	}
	var index map[string]interface{}
	if err := json.Unmarshal(merged, &index); err != nil {
		t.Fatalf("merged index does not parse: %v", err)
	}
	return index, string(merged)
}

// TestIndexGainsTheTenantsCustomResources is the defect: the index contained no
// custom resources at all, so kubectl explain could not resolve one.
func TestIndexGainsTheTenantsCustomResources(t *testing.T) {
	index, _ := mergedIndex(t, ownTenant)
	paths := index["paths"].(map[string]interface{})

	entry, ok := paths["apis/acme.io/v1"]
	if !ok {
		t.Fatalf("the tenant's own group version is missing; got %v", keysOf(paths))
	}
	url := entry.(map[string]interface{})["serverRelativeURL"].(string)
	if strings.Contains(url, ownTenant) {
		t.Errorf("serverRelativeURL still carries the prefix: %q; the client follows this link, "+
			"so it has to be in the tenant's terms too", url)
	}
	if !strings.Contains(url, "hash=ccc") {
		t.Errorf("serverRelativeURL lost upstream's hash: %q", url)
	}
}

// TestIndexKeepsOurOwnNativeEntries -- upstream's native entries describe the
// upstream apiserver, which serves resources kubezoo does not. Taking them would
// advertise those to tenants.
func TestIndexKeepsOurOwnNativeEntries(t *testing.T) {
	index, _ := mergedIndex(t, ownTenant)
	paths := index["paths"].(map[string]interface{})

	for key, wantHash := range map[string]string{
		"api/v1":       "hash=aaa",
		"apis/apps/v1": "hash=bbb",
	} {
		entry, ok := paths[key]
		if !ok {
			t.Errorf("native entry %s went missing", key)
			continue
		}
		if url := entry.(map[string]interface{})["serverRelativeURL"].(string); !strings.Contains(url, wantHash) {
			t.Errorf("native entry %s came from upstream (%q), not from kubezoo's own index", key, url)
		}
	}
	if _, ok := paths["apis/resource.k8s.io/v1"]; ok {
		t.Error("a native group upstream serves and kubezoo does not was advertised to the tenant")
	}
}

// TestIndexDropsOtherTenants -- the leak the v2 document had.
func TestIndexDropsOtherTenants(t *testing.T) {
	_, raw := mergedIndex(t, ownTenant)

	if strings.Contains(raw, otherTenant) || strings.Contains(raw, "beta.io") {
		t.Errorf("another tenant's group version reached tenant %s:\n%s", ownTenant, raw)
	}
}

// TestIndexIsSymmetric is the control: neither tenant is privileged.
func TestIndexIsSymmetric(t *testing.T) {
	index, raw := mergedIndex(t, otherTenant)
	paths := index["paths"].(map[string]interface{})

	if _, ok := paths["apis/beta.io/v1"]; !ok {
		t.Errorf("tenant %s did not get its own group version; got %v", otherTenant, keysOf(paths))
	}
	if strings.Contains(raw, ownTenant) || strings.Contains(raw, "acme.io") {
		t.Errorf("tenant %s's entries leaked to tenant %s:\n%s", ownTenant, otherTenant, raw)
	}
}

// TestCustomResourceGroupOf decides which requests go upstream and which stay
// with kubezoo's own handler. Sending a native group upstream would serve the
// upstream apiserver's description of it.
func TestCustomResourceGroupOf(t *testing.T) {
	for path, want := range map[string]string{
		"apis/acme.io/v1":                   "acme.io",
		"apis/apps/v1":                      "",
		"apis/rbac.authorization.k8s.io/v1": "",
		"apis/apiextensions.k8s.io/v1":      "",
		"api/v1":                            "",
		"apis/acme.io":                      "",
		"":                                  "",
	} {
		got, ok := customResourceGroupOf(path)
		if want == "" {
			if ok {
				t.Errorf("customResourceGroupOf(%q) = %q, want it treated as native/invalid", path, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("customResourceGroupOf(%q) = %q,%v, want %q,true", path, got, ok, want)
		}
	}
}

// TestOpenAPIV3RequestRouting exercises the whole filter rather than the merge
// alone: which requests are answered from upstream, which fall through to
// kubezoo's own handler, and what the tenant ends up seeing.
func TestOpenAPIV3RequestRouting(t *testing.T) {
	const tenantID = "111111"

	// Stands in for the handler chain below the filter -- kubezoo's own v3.
	var nextCalledFor string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalledFor = r.URL.Path
		if r.URL.Path == "/openapi/v3" {
			_, _ = w.Write([]byte(ownIndexFixture))
			return
		}
		_, _ = w.Write([]byte(`{"native":true}`))
	})

	discoveryProxy := &fakeDiscoveryProxy{
		openAPIV3: map[string][]byte{
			"": []byte(upstreamIndexFixture),
			"apis/111111-acme.io/v1": []byte(
				`{"components":{"schemas":{"io.111111-acme.v1.Widget":{` +
					`"x-kubernetes-group-version-kind":[{"group":"111111-acme.io","kind":"Widget","version":"v1"}]}}}}`),
		},
	}

	serve := func(path string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		nextCalledFor = ""
		ctx := request.WithUser(context.Background(),
			util.AddTenantIDToUserInfo(tenantID, &user.DefaultInfo{}))
		ctx = request.WithRequestInfo(ctx, &request.RequestInfo{Verb: "get", Path: path})
		r := (&http.Request{URL: &url.URL{Path: path}}).WithContext(ctx)
		w := httptest.NewRecorder()
		WithDiscoveryProxy(next, discoveryProxy).ServeHTTP(w, r)
		return w, nextCalledFor
	}

	// The index: kubezoo's own, plus this tenant's custom resources.
	indexResp, calledFor := serve("/openapi/v3")
	if calledFor != "/openapi/v3" {
		t.Error("the index was not taken from kubezoo's own handler, so the native half " +
			"would describe the upstream apiserver instead")
	}
	var index map[string]interface{}
	if err := json.Unmarshal(indexResp.Body.Bytes(), &index); err != nil {
		t.Fatalf("index does not parse: %v", err)
	}
	paths := index["paths"].(map[string]interface{})
	if _, ok := paths["apis/acme.io/v1"]; !ok {
		t.Errorf("the tenant's custom resource is missing from the index; got %v", keysOf(paths))
	}
	if strings.Contains(indexResp.Body.String(), otherTenant) {
		t.Error("another tenant reached the index")
	}

	// A custom resource group version comes from upstream, prefix removed.
	document, calledFor := serve("/openapi/v3/apis/acme.io/v1")
	if calledFor != "" {
		t.Errorf("a custom resource group was answered by kubezoo's own handler (%q), which "+
			"has no schema for it", calledFor)
	}
	if body := document.Body.String(); strings.Contains(body, tenantID) {
		t.Errorf("the prefix survived in the document a client will read: %s", body)
	} else if !strings.Contains(body, `"group":"acme.io"`) {
		t.Errorf("the group-version-kind extension was not rewritten: %s", body)
	}

	// A native group version stays with kubezoo's own handler.
	native, calledFor := serve("/openapi/v3/apis/apps/v1")
	if calledFor != "/openapi/v3/apis/apps/v1" {
		t.Error("a native group version was fetched from upstream, which would describe " +
			"resources kubezoo does not serve")
	}
	if native.Body.String() != `{"native":true}` {
		t.Errorf("the native document was altered: %s", native.Body.String())
	}
}

// TestOpenAPIV3IndexSurvivesAnUpstreamFailure -- the native half is still
// correct and is what most clients came for. Failing the whole request would be
// worse than the behaviour this replaces.
func TestOpenAPIV3IndexSurvivesAnUpstreamFailure(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ownIndexFixture))
	})
	discoveryProxy := &fakeDiscoveryProxy{openAPIV3Err: errors.New("upstream is down")}

	ctx := request.WithUser(context.Background(),
		util.AddTenantIDToUserInfo("111111", &user.DefaultInfo{}))
	ctx = request.WithRequestInfo(ctx, &request.RequestInfo{Verb: "get", Path: "/openapi/v3"})
	r := (&http.Request{URL: &url.URL{Path: "/openapi/v3"}}).WithContext(ctx)
	w := httptest.NewRecorder()
	WithDiscoveryProxy(next, discoveryProxy).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the native surface", w.Code)
	}
	var index map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatalf("index does not parse: %v", err)
	}
	if _, ok := index["paths"].(map[string]interface{})["apis/apps/v1"]; !ok {
		t.Error("the native surface was lost along with the upstream half")
	}
}
