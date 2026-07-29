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
	"net/http"
	"strings"

	"k8s.io/klog"

	"github.com/kubewharf/kubezoo/pkg/proxy"
	"github.com/kubewharf/kubezoo/pkg/util"
)

// OpenAPI v3 is served as an index at /openapi/v3 plus one document per group
// version below it. kubezoo builds its own from the resources it serves, which
// is right for the native surface and describes no custom resources at all: the
// index was byte-identical for every tenant, and `kubectl explain`, which
// prefers v3, could not find a resource the tenant had just created and could
// otherwise use.
//
// So the two halves come from different places, and that is the point rather
// than an accident. The native half stays kubezoo's own, because upstream's
// describes resources kubezoo does not serve and would advertise them to
// tenants. The custom half can only come from upstream, because that is where
// the schemas live -- filtered to this tenant and stripped of the prefix, the
// same two passes /openapi/v2 needs and for the same reasons.

const openAPIV3Prefix = "openapi/v3"

// handleOpenAPIV3 answers a /openapi/v3 request for a tenant, and reports
// whether it did. A false return means the request is not one of these and the
// caller should carry on.
func handleOpenAPIV3(w http.ResponseWriter, r *http.Request, next http.Handler,
	discoveryProxy proxy.DiscoveryProxy, tenantID, path string) bool {
	switch {
	case path == openAPIV3Prefix:
		serveOpenAPIV3Index(w, r, next, discoveryProxy, tenantID)
		return true
	case strings.HasPrefix(path, openAPIV3Prefix+"/"):
		serveOpenAPIV3Document(w, r, next, discoveryProxy, tenantID,
			strings.TrimPrefix(path, openAPIV3Prefix+"/"))
		return true
	}
	return false
}

// serveOpenAPIV3Index merges kubezoo's own index with the tenant's custom
// resource group versions from upstream.
func serveOpenAPIV3Index(w http.ResponseWriter, r *http.Request, next http.Handler,
	discoveryProxy proxy.DiscoveryProxy, tenantID string) {
	own := recordResponse(next, r)
	if own.status != http.StatusOK {
		own.replay(w)
		return
	}

	upstream, err := discoveryProxy.OpenAPIV3("", nil)
	if err != nil {
		// The native half is still correct and is what most clients came for,
		// so answer with it rather than failing the request outright. Custom
		// resources go missing, which is the behaviour this replaces.
		klog.Errorf("failed to fetch upstream openapi v3 index for tenant %s, "+
			"serving the native surface only: %v", tenantID, err)
		own.replay(w)
		return
	}

	merged, err := mergeOpenAPIV3Index(own.body.Bytes(), upstream, tenantID)
	if err != nil {
		responseDiscoveryError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(merged); err != nil {
		klog.Errorf("failed to write openapi v3 index: %v", err)
	}
}

// serveOpenAPIV3Document answers for one group version: kubezoo's own for the
// native groups, upstream's for a custom resource group.
func serveOpenAPIV3Document(w http.ResponseWriter, r *http.Request, next http.Handler,
	discoveryProxy proxy.DiscoveryProxy, tenantID, groupVersionPath string) {
	group, ok := customResourceGroupOf(groupVersionPath)
	if !ok {
		next.ServeHTTP(w, r)
		return
	}

	upstreamPath := strings.Replace(groupVersionPath, group,
		util.AddTenantIDPrefix(tenantID, group), 1)
	document, err := discoveryProxy.OpenAPIV3(upstreamPath, r.URL.Query())
	if err != nil {
		responseDiscoveryError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(stripTenantPrefix(document, tenantID)); err != nil {
		klog.Errorf("failed to write openapi v3 document: %v", err)
	}
}

// customResourceGroupOf returns the API group of a group-version path such as
// "apis/acme.io/v1", and reports false for the native surface -- including
// "api/v1", which has no group at all.
func customResourceGroupOf(groupVersionPath string) (string, bool) {
	parts := strings.Split(groupVersionPath, "/")
	if len(parts) != 3 || parts[0] != "apis" {
		return "", false
	}
	if util.IsNativeAPIGroup(parts[1]) {
		return "", false
	}
	return parts[1], true
}

// mergeOpenAPIV3Index adds this tenant's group versions to kubezoo's own index.
//
// Only the tenant's are taken from upstream: the native entries there describe
// the upstream apiserver, not kubezoo, and other tenants' entries are what made
// the equivalent v2 document a directory of everyone's custom resources.
func mergeOpenAPIV3Index(own, upstream []byte, tenantID string) ([]byte, error) {
	var ownIndex, upstreamIndex map[string]interface{}
	if err := json.Unmarshal(own, &ownIndex); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(upstream, &upstreamIndex); err != nil {
		return nil, err
	}

	ownPaths, _ := ownIndex["paths"].(map[string]interface{})
	if ownPaths == nil {
		ownPaths = map[string]interface{}{}
		ownIndex["paths"] = ownPaths
	}
	upstreamPaths, _ := upstreamIndex["paths"].(map[string]interface{})

	for key, entry := range upstreamPaths {
		if !ownsGroupVersion(key, tenantID) {
			continue
		}
		rewritten, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		var stripped interface{}
		if err := json.Unmarshal(stripTenantPrefix(rewritten, tenantID), &stripped); err != nil {
			return nil, err
		}
		ownPaths[strings.ReplaceAll(key, tenantID+"-", "")] = stripped
	}
	return json.Marshal(ownIndex)
}

// ownsGroupVersion reports whether an index key such as "apis/111111-acme.io/v1"
// belongs to this tenant. Native keys are excluded too: kubezoo's own index is
// authoritative for those.
func ownsGroupVersion(key, tenantID string) bool {
	for _, segment := range strings.Split(key, "/") {
		if owner, ok := tenantPrefixOf(segment); ok {
			return owner == tenantID
		}
	}
	return false
}

// stripTenantPrefix removes the tenant prefix throughout a document.
//
// As in the v2 filter this is textual, and safe for the same reason: it runs
// only on documents that are already known to be this tenant's, so every
// occurrence of the prefix is the prefix, whether it sits in a path, a schema
// name, a $ref or an x-kubernetes-group-version-kind extension.
func stripTenantPrefix(document []byte, tenantID string) []byte {
	return []byte(strings.ReplaceAll(string(document), tenantID+"-", ""))
}
