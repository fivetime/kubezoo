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

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// WithApplyForce carries the force flag of a server-side apply into the request.
//
// It arrives as a query parameter and the storage layer is handed
// metav1.UpdateOptions, which has no field for it. kubezoo forwards a tenant's
// apply upstream as an apply, and force is what decides whether a field owned by
// another manager stops the request or is taken from it -- so dropping it here
// would silently turn every forced apply into one that gives up.
func WithApplyForce(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch && req.URL.Query().Get("force") == "true" {
			req = req.WithContext(util.WithApplyForce(req.Context(), true))
		}
		handler.ServeHTTP(w, req)
	})
}
