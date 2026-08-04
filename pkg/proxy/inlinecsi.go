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

package proxy

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

// inlineCSIMessage says the same thing wherever the refusal comes from.
const inlineCSIMessage = "a volume provided directly by a CSI driver is the platform's to " +
	"offer: it names a driver rather than a storage class, so nothing decides whether this " +
	"tenant was sold it. Use a PersistentVolumeClaim on a published storage class"

// refuseInlineCSIVolume stops a tenant naming a CSI driver in a pod template.
//
// ⛔ MEASURED, not reasoned. With the storage class UNPUBLISHED, a tenant pod
// carrying `volumes: [{csi: {driver: hostpath.csi.k8s.io}}]` was accepted,
// reached Running, and had the driver's volume mounted at its path. Publication
// -- the mechanism that makes a storage class a tier the platform sells, and the
// thing #91 and #93 exist to enforce -- was bypassed completely, because an
// inline volume names a DRIVER and there is no class for anything to check.
//
// ⚠️ Pod Security does not help and should not be expected to. `csi` is on the
// restricted profile's ALLOWED volume list, spelled out in
// check_restrictedVolumes.go: an inline CSI volume is a compliant pod.
//
// ⭐ Refused rather than allowlisted, and that is the decision rather than a
// placeholder. A tenant that wants ordinary storage has claims and classes;
// inline volumes exist for drivers built to hand something to a pod directly --
// secrets-store being the obvious one -- which is precisely the category with
// the largest blast radius. Offering one is a new decision about one driver,
// and should cost a review of that driver, the way an entry in
// sharedCRDResources does. A permanently-empty allowlist would only move the
// decision somewhere nobody looks.
func (tp *tenantProxy) refuseInlineCSIVolume(obj runtime.Object) error {
	// ⛔ A live Pod is handled separately, on CREATE only -- see
	// RefusePodInlineCSIVolume. spec.volumes is immutable once a pod is stored,
	// so a pod that predates this cannot be repaired by its tenant, and refusing
	// its updates would strand it.
	if _, isPod := obj.(*core.Pod); isPod {
		return nil
	}
	spec, err := convert.PodSpecOf(obj)
	if err != nil || spec == nil {
		// Not something carrying a pod template. Convertor registration decides
		// that; this is not the place to complain about it.
		return nil
	}
	return inlineCSIError(spec)
}

// RefusePodInlineCSIVolume is the same check for a live Pod.
//
// ⚠️ Called from tenantProxy.Create, the one caller that knows a write is a
// create. On a template the check runs on every write, because a template's
// volumes are mutable and a tenant could otherwise add one afterwards.
func RefusePodInlineCSIVolume(pod *core.Pod) error {
	return inlineCSIError(&pod.Spec)
}

func inlineCSIError(spec *core.PodSpec) error {
	for i := range spec.Volumes {
		if spec.Volumes[i].CSI == nil {
			continue
		}
		return apierrors.NewInvalid(
			core.Kind("Pod"), "",
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "volumes").Index(i).Child("csi"),
				inlineCSIMessage,
			)},
		)
	}
	return nil
}
