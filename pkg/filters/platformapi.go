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
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// platformGroups are the API groups kubezoo serves out of its OWN storage rather
// than proxying upstream. Nothing a tenant is meant to reach lives in them.
var platformGroups = map[string]bool{
	"tenant.kubezoo.io": true,
	"quota.kubezoo.io":  true,
}

// WithPlatformAPIGuard refuses a tenant any access to the API groups kubezoo
// serves for itself.
//
// ⚠️ This is not defence in depth, it is the only defence. kubezoo's handler
// chain is hand-built and installs no authorization filter at all, so the
// authorizer the server constructs is never consulted and --authorization-mode
// is inert; the deployment defaults to AlwaysAllow on top of that, on the
// stated reasoning that authorization belongs upstream. That reasoning holds
// for every proxied resource and fails completely for these two, because they
// have no upstream: they are served from kubezoo's own etcd by a plain
// cluster-scoped store whose strategy does no tenant scoping.
//
// Measured before this filter existed: a tenant holding nothing but its own
// issued certificate could
//
//	kubectl get --raw /apis/tenant.kubezoo.io/v1alpha1/tenants
//
// and read every other tenant's kubeconfig -- client certificate and private
// key both, kept in an annotation -- and then be that tenant. A raw DELETE on
// the same path set a deletionTimestamp and the controller destroyed that
// tenant's whole estate. Discovery hides the group from a tenant's /apis, which
// is why kubectl without --raw fails, but that is obscurity and not a control.
//
// The test is "does this identity carry a tenant id", which is the same
// boundary the rest of kubezoo uses: the platform's own credential carries none.
func WithPlatformAPIGuard(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		info, ok := request.RequestInfoFrom(ctx)
		if !ok || !info.IsResourceRequest || !platformGroups[info.APIGroup] {
			handler.ServeHTTP(w, req)
			return
		}
		tenantID, ok := util.TenantFrom(ctx)
		if !ok || tenantID == "" {
			handler.ServeHTTP(w, req)
			return
		}
		refusePlatformAPI(w, info, tenantID)
	})
}

func refusePlatformAPI(w http.ResponseWriter, info *request.RequestInfo, tenantID string) {
	message := fmt.Sprintf(
		"%s.%s belongs to the kubezoo platform and is not part of tenant %s's cluster",
		info.Resource, info.APIGroup, tenantID)
	status := &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     http.StatusForbidden,
		Reason:   metav1.StatusReasonForbidden,
		Message:  message,
	}
	body, err := json.Marshal(status)
	if err != nil {
		http.Error(w, message, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if _, err := w.Write(body); err != nil {
		klog.Errorf("failed to write the platform-API refusal for tenant %s: %v", tenantID, err)
	}
}
