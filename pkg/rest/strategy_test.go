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

package test_rest

import (
	"testing"

	tenantv1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/tenant/v1alpha1"
)

// TestValidateSuspension: an unvalidated mode is stored as written, and the two
// enforcement layers then read it differently -- the front door refused writes
// while upstream RBAC stayed at full admin. Refusing the write is where this is
// caught once instead of in every reader.
func TestValidateSuspension(t *testing.T) {
	for _, tc := range []struct {
		name       string
		suspension *tenantv1alpha1.TenantSuspension
		valid      bool
	}{
		{"not suspended", nil, true},
		{"read-only", &tenantv1alpha1.TenantSuspension{Mode: tenantv1alpha1.SuspensionReadOnly}, true},
		{"frozen", &tenantv1alpha1.TenantSuspension{Mode: tenantv1alpha1.SuspensionFrozen}, true},
		{"an earlier spelling of this API", &tenantv1alpha1.TenantSuspension{Mode: "Revoked"}, false},
		{"a typo", &tenantv1alpha1.TenantSuspension{Mode: "readonly"}, false},
		{"empty", &tenantv1alpha1.TenantSuspension{Mode: ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateSuspension(tc.suspension)
			if tc.valid && len(errs) != 0 {
				t.Errorf("rejected a valid suspension: %v", errs)
			}
			if !tc.valid && len(errs) == 0 {
				t.Errorf("accepted mode %q, which no reader knows how to interpret",
					tc.suspension.Mode)
			}
		})
	}
}
