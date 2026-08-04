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
	"strings"

	"github.com/fivetime/kubezoo-contract/pkg/util"

	"github.com/pkg/errors"

	"k8s.io/apimachinery/pkg/runtime"
	volumehelpers "k8s.io/component-helpers/storage/volume"
	internal "k8s.io/kubernetes/pkg/apis/core"
)

// PVCTranformer implements the transformation between client and
// upstream server for PersistenceVolumeClaim resource.
type PVCTranformer struct{}

var _ ObjectTransformer = &PVCTranformer{}

// NewPVCTransformer initiates a PVCTranformer which implements
// the ObjectTransformer interfaces.
func NewPVCTransformer() ObjectTransformer {
	return &PVCTranformer{}
}

// Forward transforms tenant object reference to upstream object reference.
func (v *PVCTranformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	pvc, ok := obj.(*internal.PersistentVolumeClaim)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of persistentvolumeclaim")
	}
	if len(pvc.Spec.VolumeName) > 0 && !boundByController(pvc) {
		pvc.Spec.VolumeName = util.AddTenantIDPrefix(tenantID, pvc.Spec.VolumeName)
	}
	// ⚠️ dataSourceRef is the one field on a claim that can name ANOTHER
	// namespace, and it was passing through untouched -- so a tenant writing
	// `namespace: default` meant the PLATFORM's default, not its own. Prefixed
	// here like every other namespace reference kubezoo carries (see
	// rolebinding.go, webhookconfiguration.go, customresourcedefinition.go).
	//
	// ⭐ Not reachable today: CrossNamespaceVolumeDataSource has been alpha and
	// default-off since 1.26, so the apiserver drops the field, and upstream also
	// wants a ReferenceGrant in the target namespace. Done anyway because the
	// cost is three lines and the alternative is depending on someone else's
	// feature gate staying off and someone else's second lock holding.
	//
	// dataSource, beside it, needs nothing: it is a TypedLocalObjectReference and
	// resolves in the claim's own namespace, which is already prefixed.
	if pvc.Spec.DataSourceRef != nil && pvc.Spec.DataSourceRef.Namespace != nil &&
		*pvc.Spec.DataSourceRef.Namespace != "" {
		prefixed := util.UpstreamNamespace(tenantID, *pvc.Spec.DataSourceRef.Namespace)
		pvc.Spec.DataSourceRef.Namespace = &prefixed
	}

	return pvc, nil
}

// Backward transforms upstream object reference to tenant object reference.
func (v *PVCTranformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	pvc, ok := obj.(*internal.PersistentVolumeClaim)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of persistentvolumeclaim")
	}
	// The other direction, so a tenant reads back the name it wrote.
	if pvc.Spec.DataSourceRef != nil && pvc.Spec.DataSourceRef.Namespace != nil &&
		*pvc.Spec.DataSourceRef.Namespace != "" {
		trimmed := util.TrimTenantIDPrefix(tenantID, *pvc.Spec.DataSourceRef.Namespace)
		pvc.Spec.DataSourceRef.Namespace = &trimmed
	}
	if len(pvc.Spec.VolumeName) > 0 && !boundByController(pvc) &&
		strings.HasPrefix(pvc.Spec.VolumeName, tenantID+util.TenantIDSeparator) {
		pvc.Spec.VolumeName = util.TrimTenantIDPrefix(tenantID, pvc.Spec.VolumeName)
	}

	return pvc, nil
}

// boundByController reports whether spec.volumeName was written by the
// PersistentVolume controller rather than by the tenant.
//
// ⛔ THE FIX FOR A CLAIM A TENANT COULD NOT READ, LIST OR DELETE. A dynamically
// provisioned volume is created by the external provisioner DIRECTLY UPSTREAM
// and named pvc-<uid>, so it never passes through kubezoo and never carries the
// tenant prefix. Backward used to refuse any volumeName without one -- and
// refusing the conversion fails the whole object, so `kubectl get pvc` returned
// an error instead of a list and the tenant was stuck with a claim it could not
// even delete. Measured against a real CSI driver; nothing in the lab had ever
// provisioned dynamically before, so this had never run.
//
// ⚠️ Same shape as the RoleBinding bug: a reference written by something other
// than the tenant cannot be attributed, and ONE such object failed an entire
// list. The rule that came out of that one applies here -- what cannot be
// attributed is returned as it is, not turned into an error.
//
// ⭐ The signal is upstream's own, not a guess at the name. component-helpers
// documents it: "The absence of this annotation means the binding was done by
// the user (i.e. pre-bound)", and pv_controller.go only sets it when the
// controller CHOSE the volume (bindClaimToVolume sets it under shouldBind, which
// is false when the claim already names the volume it gets). So:
//
//   - present  -> volumeName is an upstream name; translating it in either
//     direction would corrupt it. A tenant sees the real name, which is a UID
//     it cannot address anyway, and Forward leaves it alone -- required, because
//     spec.volumeName is immutable once bound, so re-prefixing it on any later
//     update would make upstream refuse every write to the claim.
//   - absent   -> the tenant pre-bound its own PersistentVolume, whose name IS
//     prefixed, and both directions translate as before.
func boundByController(pvc *internal.PersistentVolumeClaim) bool {
	_, ok := pvc.Annotations[volumehelpers.AnnBoundByController]
	return ok
}
