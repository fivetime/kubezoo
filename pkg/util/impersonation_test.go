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

package util

import (
	"reflect"
	"testing"
)

// TestImpersonationGroups covers which requests carry a tenant's cluster-scoped
// permissions upstream. Each case is a way the isolation would come apart, so
// the names say what goes wrong rather than what the input is.
func TestImpersonationGroups(t *testing.T) {
	tests := []struct {
		what     string
		tenantID string
		userName string
		groups   []string
		want     []string
	}{
		{
			what:     "the tenant's own credential carries the group, or nothing it does works",
			tenantID: "111111",
			userName: "111111-admin",
			groups:   []string{"system:authenticated"},
			want:     []string{"system:authenticated", "kubezoo:proxied:111111"},
		},
		{
			what:     "a ServiceAccount does not, or every workload becomes cluster-scoped",
			tenantID: "111111",
			userName: "system:serviceaccount:111111-default:app",
			groups:   []string{"system:serviceaccounts"},
			want:     []string{"system:serviceaccounts"},
		},
		{
			what:     "a mis-issued credential carrying the group does not get it forwarded",
			tenantID: "111111",
			userName: "system:serviceaccount:111111-default:app",
			groups:   []string{"system:serviceaccounts", "kubezoo:proxied:222222"},
			want:     []string{"system:serviceaccounts"},
		},
		{
			what:     "and one carrying its own tenant's group gets exactly one copy",
			tenantID: "111111",
			userName: "111111-admin",
			groups:   []string{"kubezoo:proxied:111111"},
			want:     []string{"kubezoo:proxied:111111"},
		},
		{
			what:     "a request with no tenant carries no group",
			tenantID: "",
			userName: "111111-admin",
			groups:   []string{"system:authenticated"},
			want:     []string{"system:authenticated"},
		},
		{
			what:     "a user merely named like the admin of another tenant gets nothing",
			tenantID: "111111",
			userName: "222222-admin",
			groups:   nil,
			want:     []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.what, func(t *testing.T) {
			got := ImpersonationGroups(test.tenantID, test.userName, test.groups)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}
