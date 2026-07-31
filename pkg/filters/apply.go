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
	"mime"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// WithApplyForce carries what the storage layer cannot otherwise learn about a
// server-side apply: that it is one, and whether it asked to force.
//
// Force arrives as a query parameter and the storage layer is handed
// metav1.UpdateOptions, which has no field for it. kubezoo forwards a tenant's
// apply upstream as an apply, and force is what decides whether a field owned by
// another manager stops the request or is taken from it -- so dropping it here
// would silently turn every forced apply into one that gives up.
//
// ⚠️ Whether the request is an apply at all used to be inferred downstream from
// the object's managedFields rather than read from the request, which turned an
// ordinary PUT by a manager that had applied once before into an apply of that
// stale entry. See util.WithApplyPatch. The Content-Type is the only place the
// answer actually lives, and it is here.
func WithApplyForce(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPatch {
			handler.ServeHTTP(w, req)
			return
		}
		ctx := req.Context()
		if isApplyPatch(req.Header.Get("Content-Type")) {
			ctx = util.WithApplyPatch(ctx)
		}
		if req.URL.Query().Get("force") == "true" {
			ctx = util.WithApplyForce(ctx, true)
		}
		handler.ServeHTTP(w, req.WithContext(ctx))
	})
}

// isApplyPatch reports whether a Content-Type names one of the apply patch media
// types. Both spellings count: YAML is what kubectl and controller-runtime send,
// CBOR is what a client that negotiated CBOR sends, and the storage layer is
// handed neither the header nor the patch type.
func isApplyPatch(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A malformed header is not an apply. Being wrong in this direction
		// costs a fall back to the update path; being wrong in the other
		// rewrites somebody's write into an apply of something else.
		return false
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == string(types.ApplyPatchType) || mediaType == string(types.ApplyCBORPatchType)
}
