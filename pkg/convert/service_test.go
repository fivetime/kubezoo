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

package convert

import (
	"testing"

	core "k8s.io/kubernetes/pkg/apis/core"
)

// TestExternalIPsAreNew pins the rule that decides CVE-2020-8554 here.
//
// ⛔ A Service carrying spec.externalIPs makes the data plane on every node
// intercept traffic to those addresses, with no check that the writer has any
// claim to them -- another tenant's service, the platform's DNS, anything
// outside the cluster.
//
// ⭐ Subset, not refusal, and the rule is the upstream plugin's rather than one
// invented here: keeping or dropping an address is allowed, adding one is not. A
// Service that already carries an address has to stay writable by its owner, who
// would otherwise be unable even to remove it.
func TestExternalIPsAreNew(t *testing.T) {
	svc := func(ips ...string) *core.Service {
		return &core.Service{Spec: core.ServiceSpec{ExternalIPs: ips}}
	}

	for _, tc := range []struct {
		what string
		new  *core.Service
		old  *core.Service // nil = create
		want bool
	}{
		{"creating with none", svc(), nil, false},
		{"creating with one", svc("10.0.0.1"), nil, true},
		{"creating with several", svc("10.0.0.1", "10.0.0.2"), nil, true},

		// ⭐ The half that keeps existing Services writable.
		{"rewriting without touching them", svc("10.0.0.1"), svc("10.0.0.1"), false},
		{"dropping one", svc("10.0.0.1"), svc("10.0.0.1", "10.0.0.2"), false},
		{"dropping all", svc(), svc("10.0.0.1"), false},
		// Order is not identity.
		{"reordering", svc("10.0.0.2", "10.0.0.1"), svc("10.0.0.1", "10.0.0.2"), false},

		// ⚠️ And the half that closes it: anything added is new, even beside one
		// that was already there.
		{"adding one beside an existing", svc("10.0.0.1", "10.0.0.9"),
			svc("10.0.0.1"), true},
		{"swapping one for another", svc("10.0.0.9"), svc("10.0.0.1"), true},
	} {
		if got := ExternalIPsAreNew(tc.new, tc.old); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.what, got, tc.want)
		}
	}
}
