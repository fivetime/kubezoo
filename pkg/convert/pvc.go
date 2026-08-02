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
	if len(pvc.Spec.VolumeName) > 0 {
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
	if len(pvc.Spec.VolumeName) > 0 {
		if !strings.HasPrefix(pvc.Spec.VolumeName, tenantID) {
			return nil, errors.Errorf("invalid pv name %s in pvc %s, tenant id is %s", pvc.Spec.VolumeName, pvc.Spec.VolumeName, tenantID)
		}
		pvc.Spec.VolumeName = util.TrimTenantIDPrefix(tenantID, pvc.Spec.VolumeName)
	}

	return pvc, nil
}
