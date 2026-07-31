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
	"testing"

	"k8s.io/apiserver/pkg/endpoints/request"

	tenantv1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/tenant/v1alpha1"
)

// TestReadOnlySuspensionAllowsReadsAndRefusesWrites is the billing case: the
// tenant keeps its view, so that being suspended does not look like having lost
// its data.
func TestReadOnlySuspensionAllowsReadsAndRefusesWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request request.RequestInfo
		allow   bool
	}{
		{"get", request.RequestInfo{IsResourceRequest: true, Verb: "get", Resource: "pods"}, true},
		{"list", request.RequestInfo{IsResourceRequest: true, Verb: "list", Resource: "pods"}, true},
		{"watch", request.RequestInfo{IsResourceRequest: true, Verb: "watch", Resource: "pods"}, true},
		{"pod logs stay readable",
			request.RequestInfo{IsResourceRequest: true, Verb: "get", Resource: "pods", Subresource: "log"}, true},

		{"create", request.RequestInfo{IsResourceRequest: true, Verb: "create", Resource: "pods"}, false},
		{"update", request.RequestInfo{IsResourceRequest: true, Verb: "update", Resource: "pods"}, false},
		{"patch", request.RequestInfo{IsResourceRequest: true, Verb: "patch", Resource: "pods"}, false},
		{"delete", request.RequestInfo{IsResourceRequest: true, Verb: "delete", Resource: "pods"}, false},
		{"deletecollection",
			request.RequestInfo{IsResourceRequest: true, Verb: "deletecollection", Resource: "pods"}, false},

		{"exec is a read only by the verb",
			request.RequestInfo{IsResourceRequest: true, Verb: "create", Resource: "pods", Subresource: "exec"}, false},
		{"attach likewise",
			request.RequestInfo{IsResourceRequest: true, Verb: "get", Resource: "pods", Subresource: "attach"}, false},
		{"portforward likewise",
			request.RequestInfo{IsResourceRequest: true, Verb: "get", Resource: "pods", Subresource: "portforward"}, false},
		{"minting a token is a change however it reads",
			request.RequestInfo{IsResourceRequest: true, Verb: "create", Resource: "serviceaccounts", Subresource: "token"}, false},

		{"access reviews are questions that happen to be posted",
			request.RequestInfo{IsResourceRequest: true, Verb: "create", APIGroup: "authorization.k8s.io",
				Resource: "selfsubjectaccessreviews"}, true},

		{"discovery keeps working", request.RequestInfo{IsResourceRequest: false, Path: "/apis"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := allowedUnderSuspension(&tc.request, tenantv1alpha1.SuspensionReadOnly)
			if got != tc.allow {
				t.Errorf("allowed = %v, want %v", got, tc.allow)
			}
		})
	}
}

// TestFrozenSuspensionRefusesEverythingButDiscovery is the investigation case.
// Discovery survives so that the tenant is told why, rather than watching its
// client fail while building a request.
func TestFrozenSuspensionRefusesEverythingButDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request request.RequestInfo
		allow   bool
	}{
		{"get", request.RequestInfo{IsResourceRequest: true, Verb: "get", Resource: "pods"}, false},
		{"list", request.RequestInfo{IsResourceRequest: true, Verb: "list", Resource: "pods"}, false},
		{"watch", request.RequestInfo{IsResourceRequest: true, Verb: "watch", Resource: "pods"}, false},
		{"create", request.RequestInfo{IsResourceRequest: true, Verb: "create", Resource: "pods"}, false},
		{"even an access review",
			request.RequestInfo{IsResourceRequest: true, Verb: "create", APIGroup: "authorization.k8s.io",
				Resource: "selfsubjectaccessreviews"}, false},
		{"discovery", request.RequestInfo{IsResourceRequest: false, Path: "/apis"}, true},
		{"version", request.RequestInfo{IsResourceRequest: false, Path: "/version"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := allowedUnderSuspension(&tc.request, tenantv1alpha1.SuspensionFrozen)
			if got != tc.allow {
				t.Errorf("allowed = %v, want %v", got, tc.allow)
			}
		})
	}
}

// TestUnrecognisedModeIsTreatedAsFrozen -- validation refuses an unknown mode on
// the way in, so this covers objects stored before that existed. It used to fall
// through to the read-only branch while the controller left upstream RBAC at
// full admin, so the two layers disagreed and a typo produced a tenant that was
// half-suspended.
func TestUnrecognisedModeIsTreatedAsFrozen(t *testing.T) {
	for _, verb := range []string{"get", "list", "watch", "create", "delete"} {
		requestInfo := request.RequestInfo{IsResourceRequest: true, Verb: verb, Resource: "pods"}
		if allowedUnderSuspension(&requestInfo, tenantv1alpha1.TenantSuspensionMode("Revoked")) {
			t.Errorf("%s was allowed under an unrecognised mode; a suspension nobody can "+
				"interpret should hold, not half-open", verb)
		}
	}
}
