/*
Copyright 2024 The KubeZoo Authors.

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
	"strings"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// trimIfAttributable takes the tenant's prefix off a reference that carries it,
// and returns anything else unchanged.
//
// ⛔ THE RULE THIS ENCODES, AND WHY IT IS A FUNCTION RATHER THAN A HABIT.
//
// A reference kubezoo did not write cannot be attributed to a tenant. Refusing
// to convert it does not hide one field -- a convertor returning an error fails
// the WHOLE OBJECT, and one failed object fails the whole LIST. The tenant then
// cannot see, or delete, anything of that kind at all.
//
// Measured four times:
//
//   - A RoleBinding referencing kubezoo's own unprefixed ClusterRole made
//     `kubectl get rolebinding` return an error instead of a list. Fixed once,
//     in rolebinding.go, and the rule was written into that comment -- but its
//     twin in clusterrolebinding.go kept erroring, because a fix does not
//     spread by itself.
//   - A dynamically provisioned PersistentVolume is created by the external
//     provisioner as pvc-<uid>, straight upstream, with no prefix. The claim
//     naming it became unreadable and UNDELETABLE (pvc.go).
//   - The dead VolumeAttachment convertor had the same check written into it
//     already, waiting for the day the resource was served.
//   - The custom resource convertor refused any group without the prefix, which
//     is every shared platform CRD.
//
// ⚠️ The cost is real and is accepted knowingly: the tenant reads back a name
// that is not in its own namespace of names -- a platform ClusterRole, a
// provisioner's volume. That is information, and it is less than the object
// being unreadable. Where a reference must NOT be shown, hide the object; do
// not fail its conversion.
func trimIfAttributable(value, tenantID string) string {
	if !strings.HasPrefix(value, tenantID+util.TenantIDSeparator) {
		return value
	}
	return util.TrimTenantIDPrefix(tenantID, value)
}
