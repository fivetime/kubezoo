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
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// TestPlatformAPIIsNotATenantsToRead guards the one thing standing between a
// tenant and every other tenant's credentials.
//
// ⚠️ kubezoo's handler chain installs no authorization filter, so the authorizer
// it builds is never consulted and --authorization-mode is inert. For proxied
// resources that is deliberate -- upstream authorizes them. tenant.kubezoo.io
// and quota.kubezoo.io have no upstream: they are served from kubezoo's own etcd
// by a store that does no tenant scoping. Measured before this filter: a tenant
// with nothing but its own certificate could read every other tenant's
// kubeconfig, private key included, off an annotation, and delete any tenant.
func TestPlatformAPIIsNotATenantsToRead(t *testing.T) {
	cases := []struct {
		name     string
		group    string
		resource string
		verb     string
		tenantID string
		want     int
	}{
		{"a tenant reading the tenant list", "tenant.kubezoo.io", "tenants", "list", "111111", http.StatusForbidden},
		{"a tenant reading one tenant", "tenant.kubezoo.io", "tenants", "get", "111111", http.StatusForbidden},
		{"a tenant deleting a tenant", "tenant.kubezoo.io", "tenants", "delete", "111111", http.StatusForbidden},
		{"a tenant patching a tenant", "tenant.kubezoo.io", "tenants", "patch", "111111", http.StatusForbidden},
		{"a tenant reaching the quota API", "quota.kubezoo.io", "clusterresourcequotas", "list", "111111", http.StatusForbidden},
		{"the platform reading tenants", "tenant.kubezoo.io", "tenants", "list", "", http.StatusOK},
		{"a tenant reading its own pods", "", "pods", "list", "111111", http.StatusOK},
		{"a tenant reading rbac", "rbac.authorization.k8s.io", "roles", "list", "111111", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := WithPlatformAPIGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			ctx := context.Background()
			if tc.tenantID != "" {
				ctx = request.WithUser(ctx, &user.DefaultInfo{
					Name:  tc.tenantID + "-admin",
					Extra: map[string][]string{"tenant": {tc.tenantID}},
				})
			} else {
				ctx = request.WithUser(ctx, &user.DefaultInfo{Name: "admin"})
			}
			ctx = request.WithRequestInfo(ctx, &request.RequestInfo{
				IsResourceRequest: true,
				APIGroup:          tc.group,
				Resource:          tc.resource,
				Verb:              tc.verb,
			})

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apis", nil).WithContext(ctx))

			if recorder.Code != tc.want {
				t.Errorf("%s: status %d, want %d (body %s)",
					tc.name, recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}
