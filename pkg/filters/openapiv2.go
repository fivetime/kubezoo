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

	openapiv2 "github.com/google/gnostic-models/openapiv2"

	"github.com/kubewharf/kubezoo/pkg/util"
)

// The OpenAPI v2 document is the upstream one, so it describes every tenant's
// custom resources under their upstream names. Two things have to happen to it
// before a tenant may see it, and neither was being done in full: other
// tenants' entries were left in place entirely, and the tenant's own kept the
// prefix everywhere except in the definition keys.
//
// The result was a tenant able to enumerate every other tenant's id, custom
// resource group, kind and schema from one GET, and -- because a path, its
// $ref and the definition key disagreed about the group -- `kubectl explain`
// failing on a custom resource the tenant had just created and could otherwise
// use normally.
//
// The two are done in that order and for different reasons. Dropping is
// structural: only the top-level paths and definitions maps say who owns an
// entry, so ownership has to be decided there. Stripping is textual: once
// nothing of another tenant's is left, every remaining occurrence of this
// tenant's prefix is the prefix, whether it sits in a key, a $ref, or the
// x-kubernetes-group-version-kind extension -- and those last two are why
// rewriting keys alone left the document internally inconsistent.
//
// /openapi/v3 and the discovery endpoints have their own conversion; this
// document was the one path where none happened.
func filterOpenAPIV2(raw []byte, tenantID string) ([]byte, error) {
	var document map[string]interface{}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	dropOtherTenants(document, "paths", "/", tenantID)
	dropOtherTenants(document, "definitions", ".", tenantID)

	filtered, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return []byte(strings.ReplaceAll(string(filtered), tenantID+"-", "")), nil
}

// dropOtherTenants removes the entries of one top-level map that belong to
// somebody else.
func dropOtherTenants(document map[string]interface{}, field, separator, tenantID string) {
	entries, ok := document[field].(map[string]interface{})
	if !ok {
		return
	}
	for key := range entries {
		if !keyBelongsToTenant(key, separator, tenantID) {
			delete(entries, key)
		}
	}
}

// keyBelongsToTenant reports whether a path or definition key is the tenant's
// own or part of the native API surface, as opposed to another tenant's.
func keyBelongsToTenant(key, separator, tenantID string) bool {
	for _, segment := range strings.Split(key, separator) {
		owner, ok := tenantPrefixOf(segment)
		if ok && owner != tenantID {
			return false
		}
	}
	return true
}

// tenantPrefixOf reports whether a segment carries a tenant prefix, and which
// tenant's.
//
// A tenant id is exactly six characters followed by a dash, so the test is
// positional and does not need to know which tenants exist. It can in principle
// misread a native name of that shape as a tenant prefix, but no Kubernetes API
// group or definition segment has one, and a name that did would be
// indistinguishable from a real tenant's anyway -- a property of the prefixing
// scheme rather than of this function.
func tenantPrefixOf(segment string) (string, bool) {
	if len(segment) <= util.TenantIDLength+1 || segment[util.TenantIDLength] != '-' {
		return "", false
	}
	owner := segment[:util.TenantIDLength]
	if util.ValidateTenantName(owner) != nil {
		return "", false
	}
	return owner, true
}

// dropOtherTenantsFromDocument does the structural half for the protobuf form.
//
// The same document in gnostic's representation is a list of named entries
// rather than a JSON object, so the map-based drop above sees nothing to do and
// silently passes it through -- which is exactly what happened: the textual
// strip ran, the drop did not, and the tenant got a document with its own
// prefix removed and everybody else's intact.
func dropOtherTenantsFromDocument(doc *openapiv2.Document, tenantID string) {
	if doc == nil {
		return
	}
	if doc.Paths != nil {
		kept := doc.Paths.Path[:0]
		for _, path := range doc.Paths.Path {
			if keyBelongsToTenant(path.Name, "/", tenantID) {
				kept = append(kept, path)
			}
		}
		doc.Paths.Path = kept
	}
	if doc.Definitions != nil {
		kept := doc.Definitions.AdditionalProperties[:0]
		for _, definition := range doc.Definitions.AdditionalProperties {
			if keyBelongsToTenant(definition.Name, ".", tenantID) {
				kept = append(kept, definition)
			}
		}
		doc.Definitions.AdditionalProperties = kept
	}
}
