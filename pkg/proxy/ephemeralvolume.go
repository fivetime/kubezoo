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
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

// refuseUnpublishedEphemeralClasses applies the storage class and volume
// attributes class rules to a generic ephemeral volume's claim template.
//
// ⛔ THE SECOND WAY OUT OF THE SAME ROOM. #105 refused an inline csi volume
// because it names a driver and skips classes entirely. This is the other path
// through the same pod spec: an ephemeral volume embeds a whole PVC template,
// and the claim is created by the EPHEMERAL CONTROLLER IN
// kube-controller-manager, straight upstream --
// pkg/controller/volume/ephemeral/controller.go calls
// PersistentVolumeClaims(pod.Namespace).Create. It never passes through kubezoo,
// so refuseUnpublishedStorageClass -- which matches *core.PersistentVolumeClaim
// -- never sees it, and neither does the volume attributes class rule. A tenant
// names a class the platform withdrew, or never published, and gets it.
//
// ⭐ The root cause was already written down: kubezoo cannot see what
// kube-controller-manager produces, so a pod-level rule has to be applied to the
// TEMPLATE. That is why placement works on templates. Applying it to the inline
// csi volume and not to the ephemeral one beside it is the recurring shape --
// a rule that does not spread by itself.
//
// ⭐ The decision is made by CALLING the existing guards on a synthesised claim,
// not by a second copy of their logic. Two copies of one rule diverge; this
// repository has spent today proving it -- rolebinding.go against
// clusterrolebinding.go, three copies of the write-path guard list, the same
// prefix arithmetic written out in three files. What this adds is the field path,
// because an error naming spec.storageClassName on a Deployment helps nobody.
func (tp *tenantProxy) refuseUnpublishedEphemeralClasses(obj, old runtime.Object) error {
	spec, err := convert.PodSpecOf(obj)
	if err != nil || spec == nil {
		return nil
	}
	oldSpec, _ := convert.PodSpecOf(old)

	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Ephemeral == nil || v.Ephemeral.VolumeClaimTemplate == nil {
			continue
		}
		claim := &core.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: v.Name},
			Spec:       v.Ephemeral.VolumeClaimTemplate.Spec,
		}
		// ⚠️ The matching OLD template, paired by volume name, so that an
		// unchanged reapply is not refused. Without it a GitOps controller
		// reapplying a Deployment whose template names a since-retired class would
		// fail forever -- the reconcile loop refuseUnpublishedVolumeAttributesClass
		// documents at length and exists to avoid.
		var oldClaim runtime.Object
		if oldSpec != nil {
			for j := range oldSpec.Volumes {
				ov := &oldSpec.Volumes[j]
				if ov.Name == v.Name && ov.Ephemeral != nil && ov.Ephemeral.VolumeClaimTemplate != nil {
					oldClaim = &core.PersistentVolumeClaim{
						ObjectMeta: metav1.ObjectMeta{Name: ov.Name},
						Spec:       ov.Ephemeral.VolumeClaimTemplate.Spec,
					}
					break
				}
			}
		}

		base := field.NewPath("spec", "volumes").Index(i).Child("ephemeral", "volumeClaimTemplate", "spec")
		if err := tp.refuseUnpublishedStorageClass(claim); err != nil {
			return retargetToEphemeral(err, obj, base.Child("storageClassName"))
		}
		if err := tp.refuseUnpublishedVolumeAttributesClass(claim, oldClaim); err != nil {
			return retargetToEphemeral(err, obj, base.Child("volumeAttributesClassName"))
		}
	}
	return nil
}

// retargetToEphemeral keeps the guard's own words and puts them on the field the
// tenant actually wrote.
//
// ⚠️ A ServiceUnavailable is passed through untouched: it is about the cache not
// being ready, not about this object, and rewriting it into an Invalid would
// tell a client to fix something instead of to retry.
func retargetToEphemeral(err error, obj runtime.Object, path *field.Path) error {
	if apierrors.IsServiceUnavailable(err) {
		return err
	}
	name := ""
	if accessor, ok := obj.(metav1.Object); ok {
		name = accessor.GetName()
	}
	gk := schema.GroupKind{Kind: obj.GetObjectKind().GroupVersionKind().Kind}
	detail := err.Error()
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if d := status.Status().Details; d != nil && len(d.Causes) > 0 {
			detail = d.Causes[0].Message
		}
	}
	return apierrors.NewInvalid(gk, name, field.ErrorList{field.Forbidden(path, detail)})
}
