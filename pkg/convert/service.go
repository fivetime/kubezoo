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
	core "k8s.io/kubernetes/pkg/apis/core"
)

// ExternalIPsAreNew reports whether a Service is claiming external IPs it was
// not already claiming.
//
// ⛔ A Service carrying spec.externalIPs makes the data plane on EVERY node
// intercept traffic to those addresses and deliver it to that Service's
// endpoints. Nothing checks that the writer has any claim to the address. A
// tenant can therefore take:
//
//   - another tenant's service, given its external IP;
//   - the platform's own -- the apiserver, DNS, a database, a registry;
//   - any address outside the cluster, so that every pod in the cluster talking
//     to it reaches the tenant's pods instead.
//
// This is CVE-2020-8554, and Kubernetes ships a mitigation for it:
// plugin/pkg/admission/network/denyserviceexternalips. ⚠️ That plugin denies the
// field to EVERYONE including the platform, which is exactly why the decision
// belongs here instead, where the writer's tenancy is known.
//
// ⭐ The rule is the upstream plugin's, not one invented here: allow when the new
// set is a SUBSET of the stored one. Keeping or dropping an address is fine;
// adding one is not. So a Service that already carries an address stays writable
// -- an outright refusal would leave it unwritable by its owner, who then cannot
// even remove the address.
//
// old is nil on a create, where every address is new by definition.
func ExternalIPsAreNew(svc, old *core.Service) bool {
	if len(svc.Spec.ExternalIPs) == 0 {
		return false
	}
	stored := map[string]bool{}
	if old != nil {
		for _, ip := range old.Spec.ExternalIPs {
			stored[ip] = true
		}
	}
	for _, ip := range svc.Spec.ExternalIPs {
		if !stored[ip] {
			return true
		}
	}
	return false
}
