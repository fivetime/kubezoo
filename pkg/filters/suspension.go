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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/util"
	tenantv1alpha1 "github.com/kubewharf/kubezoo/pkg/apis/tenant/v1alpha1"
	tenantlister "github.com/kubewharf/kubezoo/pkg/generated/listers/tenant/v1alpha1"
)

// WithTenantSuspension refuses a suspended tenant's requests at the front door,
// with an explanation.
//
// This is the half of a suspension the tenant experiences. The other half is
// upstream RBAC, which the tenant controller narrows or withdraws, and which is
// what actually holds -- a tenant's workloads talk to the upstream API server
// directly, without passing through here at all. Both are needed: RBAC alone
// answers a tenant with a bare Forbidden that reads like a malfunction, and this
// alone would stop nothing that does not come through kubezoo.
//
// Neither mode touches what the tenant is running. Suspension is about the
// tenant's ability to act, and both cases -- an unpaid invoice, an investigation
// -- want the workloads left exactly as they are.
func WithTenantSuspension(handler http.Handler, tenants tenantlister.TenantLister) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		suspension, tenantID := suspensionFor(req, tenants)
		if suspension == nil {
			handler.ServeHTTP(w, req)
			return
		}
		requestInfo, ok := request.RequestInfoFrom(req.Context())
		if !ok {
			handler.ServeHTTP(w, req)
			return
		}
		if allowedUnderSuspension(requestInfo, suspension.Mode) {
			handler.ServeHTTP(w, req)
			return
		}
		refuse(w, tenantID, suspension)
	})
}

// suspensionFor returns the suspension in force for the request's tenant, if
// any.
//
// A tenant that cannot be found in the cache is treated as not suspended. This
// fails open on purpose: the cache missing an entry is a local fault, and
// refusing every tenant's every request because of one would be a worse outcome
// than briefly not enforcing a suspension that upstream RBAC is enforcing
// anyway. That second layer is the reason this one can afford to be lenient.
func suspensionFor(req *http.Request, tenants tenantlister.TenantLister) (*tenantv1alpha1.TenantSuspension, string) {
	tenantID := util.TenantIDFrom(req.Context())
	if tenantID == "" {
		return nil, ""
	}
	tenant, err := tenants.Get(tenantID)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Warningf("cannot tell whether tenant %s is suspended, allowing the request; "+
				"upstream RBAC still applies: %v", tenantID, err)
		}
		return nil, tenantID
	}
	return tenant.Spec.Suspension, tenantID
}

// allowedUnderSuspension decides whether one request survives a suspension.
func allowedUnderSuspension(requestInfo *request.RequestInfo, mode tenantv1alpha1.TenantSuspensionMode) bool {
	// Discovery, version and health are left reachable in both modes. They
	// carry nothing of the tenant's, and without them kubectl fails while
	// building its request rather than showing the refusal -- the tenant would
	// see a broken client instead of the reason it was suspended.
	if !requestInfo.IsResourceRequest {
		return true
	}

	if mode != tenantv1alpha1.SuspensionReadOnly {
		// Frozen, or a mode this build does not recognise. Validation refuses
		// an unknown mode on the way in, so reaching here means an object
		// written before that existed -- and a suspension nobody can interpret
		// should hold rather than half-open. The alternative silently left
		// upstream RBAC at full admin while the front door behaved read-only.
		return false
	}

	// Read-only from here down.
	switch requestInfo.Subresource {
	case "exec", "attach", "portforward":
		// Reads in name only: each one is a way into a running container, and
		// from inside it the tenant can change whatever it likes.
		return false
	case "token":
		// A token request mints a working credential, which is a change to the
		// world however the verb reads.
		return false
	}

	switch requestInfo.Verb {
	case "get", "list", "watch":
		return true
	case "create":
		// The access reviews are questions that happen to be POSTed. Refusing
		// them would break `kubectl auth can-i` for a tenant trying to work out
		// what it may still do.
		return requestInfo.APIGroup == "authorization.k8s.io"
	default:
		return false
	}
}

func refuse(w http.ResponseWriter, tenantID string, suspension *tenantv1alpha1.TenantSuspension) {
	message := fmt.Sprintf("tenant %s is suspended", tenantID)
	switch suspension.Mode {
	case tenantv1alpha1.SuspensionReadOnly:
		message += " and is read-only: this request would change something. " +
			"Your objects are still readable and your workloads are still running"
	case tenantv1alpha1.SuspensionFrozen:
		message += " and is frozen: no request is accepted. Nothing of yours has been " +
			"deleted and your workloads are still running"
	default:
		message += fmt.Sprintf(" with an unrecognised mode %q, and is being treated as frozen",
			suspension.Mode)
	}
	if suspension.Reason != "" {
		message += fmt.Sprintf(" (%s)", suspension.Reason)
	}
	message += ". Contact the platform operator."

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
		klog.Errorf("failed to write suspension response for tenant %s: %v", tenantID, err)
	}
}
