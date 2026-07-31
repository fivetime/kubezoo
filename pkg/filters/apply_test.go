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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// TestApplyIsReadFromTheRequest pins the one place the answer lives.
//
// ⚠️ Whether a write was a server-side apply used to be inferred downstream from
// the object's managedFields, which made an ordinary PUT by a manager that had
// applied once before into an apply of that stale entry -- dropping the rest of
// the write and, upstream, deleting the field it had just changed.
func TestApplyIsReadFromTheRequest(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		contentType string
		query       string
		wantApply   bool
		wantForce   bool
	}{
		{"kubectl apply --server-side", http.MethodPatch, "application/apply-patch+yaml", "", true, false},
		{"with force", http.MethodPatch, "application/apply-patch+yaml", "?force=true", true, true},
		{"with charset parameter", http.MethodPatch, "application/apply-patch+yaml; charset=utf-8", "", true, false},
		{"CBOR apply", http.MethodPatch, "application/apply-patch+cbor", "", true, false},
		{"strategic merge patch", http.MethodPatch, "application/strategic-merge-patch+json", "", false, false},
		{"json merge patch", http.MethodPatch, "application/merge-patch+json", "", false, false},
		{"json patch", http.MethodPatch, "application/json-patch+json", "", false, false},
		{"an ordinary update", http.MethodPut, "application/json", "", false, false},
		{"a create", http.MethodPost, "application/json", "", false, false},
		{"no content type", http.MethodPatch, "", "", false, false},
		{"malformed content type", http.MethodPatch, "application/;;;", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotApply, gotForce bool
			handler := WithApplyForce(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				gotApply = util.IsApplyPatch(req.Context())
				gotForce = util.ApplyForceFrom(req.Context())
			}))

			req := httptest.NewRequest(tc.method, "/api/v1/namespaces/default/configmaps/cm"+tc.query,
				strings.NewReader("{}"))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if gotApply != tc.wantApply {
				t.Errorf("read as an apply = %v, want %v", gotApply, tc.wantApply)
			}
			if gotForce != tc.wantForce {
				t.Errorf("read as forced = %v, want %v", gotForce, tc.wantForce)
			}
		})
	}
}
