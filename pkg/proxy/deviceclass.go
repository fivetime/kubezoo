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

package proxy

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	resourceapi "k8s.io/kubernetes/pkg/apis/resource"
)

// refuseUnpublishedDeviceClass stops a tenant asking for hardware the platform
// has not offered it.
//
// ⛔ A DeviceClass selects DEVICES -- which GPUs, which accelerators, at which
// tier. It is cluster-scoped and belongs to the platform, and a ResourceClaim
// reaches it BY NAME from inside a tenant's namespace. That is the same shape as
// storageClassName, ingressClassName and volumeAttributesClassName, each of
// which arrived as "the field is passed through untranslated, so a tenant naming
// the platform's own object simply gets it".
//
// ⭐ The difference is when. Those three were found and fixed after the fact;
// this one is wired at the moment the API group is opened, so there is never a
// version in which naming an unpublished class works.
//
// ⚠️ Refused on every write, not only on create. Unlike a storage class, a
// claim's device requests are the whole point of the object, and a platform that
// withdraws a tier must not have it walk back in through an update.
func (tp *tenantProxy) refuseUnpublishedDeviceClass(obj runtime.Object) error {
	if tp.publishedDeviceClasses == nil {
		return nil
	}
	for _, name := range deviceClassNamesOf(obj) {
		if name == "" {
			continue
		}
		if tp.publishedDeviceClasses.Visible(name) {
			continue
		}
		// ⚠️ The message names what the tenant CAN do. A refusal over a
		// cluster-scoped object it cannot list would otherwise be unactionable --
		// which is the complaint that made storage classes discoverable in the
		// first place, and why device classes are readable.
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "resource.k8s.io", Kind: "ResourceClaim"}, "",
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "devices", "requests", "deviceClassName"),
				fmt.Errorf("device class %q is not offered to this tenant: it selects hardware the "+
					"platform allocates, so only the classes it publishes may be named. "+
					"`kubectl get deviceclasses` lists the ones you may use", name).Error(),
			)})
	}
	return nil
}

// deviceClassNamesOf collects every device class an object names.
//
// ⚠️ Both kinds, and both matter: a ResourceClaimTemplate is how a Deployment
// asks for a device, so guarding only the claim would leave the ordinary way of
// using DRA unguarded.
func deviceClassNamesOf(obj runtime.Object) []string {
	switch v := obj.(type) {
	case *resourceapi.ResourceClaim:
		return requestedDeviceClasses(&v.Spec)
	case *resourceapi.ResourceClaimTemplate:
		return requestedDeviceClasses(&v.Spec.Spec)
	default:
		return nil
	}
}

func requestedDeviceClasses(spec *resourceapi.ResourceClaimSpec) []string {
	var names []string
	for i := range spec.Devices.Requests {
		request := &spec.Devices.Requests[i]
		if request.Exactly != nil {
			names = append(names, request.Exactly.DeviceClassName)
		}
		// ⚠️ FirstAvailable is a list of alternatives, and EVERY one of them is a
		// class the claim may end up using. Checking only the first would let an
		// unpublished class through as the fallback.
		for j := range request.FirstAvailable {
			names = append(names, request.FirstAvailable[j].DeviceClassName)
		}
	}
	return names
}
