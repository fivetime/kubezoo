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

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	internal "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// PVTranformer implements the transformation between client and
// upstream server for PersistenceVolume resource.
type PVTranformer struct{}

var _ ObjectTransformer = &PVTranformer{}

// NewPVTransformer initiates a PVTranformer which implements
// the ObjectTransformer interfaces.
func NewPVTransformer() ObjectTransformer {
	return &PVTranformer{}
}

// Forward transforms tenant object reference to upstream object reference.
func (v *PVTranformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	// convert logic
	pv, ok := obj.(*internal.PersistentVolume)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of persistentvolume")
	}

	if err := refuseUnsafePVSource(&pv.Spec, pv.Name); err != nil {
		return nil, err
	}

	if err := refuseUnreservedPV(pv); err != nil {
		return nil, err
	}
	if pv.Spec.ClaimRef != nil && len(pv.Spec.ClaimRef.Namespace) > 0 {
		pv.Spec.ClaimRef.Namespace = util.UpstreamNamespace(tenantID, pv.Spec.ClaimRef.Namespace)
	}
	return pv, nil
}

// refuseUnreservedPV stops a tenant offering storage that another tenant's claim
// can bind to.
//
// ⛔ A PersistentVolume is CLUSTER-SCOPED and the binder does not care whose it
// is. findByClaim matches on access modes, class, size and topology and never
// looks at tenancy, and pv_controller only provisions dynamically when no
// existing volume matched -- so a static volume PRE-EMPTS the provisioner. Every
// link checked in the Kubernetes source rather than assumed.
//
// ⛔ Which makes this the shape of the attack: tenant A creates a PersistentVolume
// with a published class name, a common size, and an NFS server A controls.
// Tenant B's claim for that class binds to it, and B's pods mount A's storage. A
// then reads everything B writes and serves B whatever it likes.
//
// ⚠️ Refusing the class name instead does not work, and the reason is worth
// keeping: a tenant's own claim may only name a PUBLISHED class -- that is
// tenantProxy.refuseUnpublishedStorageClass -- so a tenant's own static volume
// has to carry a published class to be usable by its owner at all. The
// legitimate use and the attack are the same write.
//
// ⭐ What separates them is the claimRef. FindMatchingVolume skips any volume
// whose claimRef is set and does not name the claim being bound, so a volume
// reserved for one claim is invisible to every other. Requiring one leaves
// static provisioning working -- you name the claim you are providing for, which
// is what reserving a volume means -- and closes the cross-tenant grab entirely.
// The namespace is prefixed just below, and Backward refuses a claimRef pointing
// outside the tenant, so the reservation cannot be aimed anywhere else.
//
// ⚠️ I had this wrong earlier and the comment that said so has been corrected:
// "refusing it would block a tenant from statically providing storage without
// protecting anything". The last clause was false.
func refuseUnreservedPV(pv *internal.PersistentVolume) error {
	if pv.Spec.ClaimRef != nil &&
		len(pv.Spec.ClaimRef.Namespace) > 0 && len(pv.Spec.ClaimRef.Name) > 0 {
		return nil
	}
	return errors.Errorf("persistentvolume %s must reserve itself for one of your own claims: "+
		"set spec.claimRef to the namespace and name of the PersistentVolumeClaim it is for. "+
		"Without a claimRef the volume is offered to every claim in the cluster, including "+
		"other tenants', and whoever binds it first mounts your storage", pv.Name)
}

// refuseUnsafePVSource is an allowlist over the volume source of a
// tenant-written PersistentVolume.
//
// ⚠️ Only spec.claimRef.namespace was ever rewritten here, and nothing else in
// the spec was looked at -- while kubezoo-contract's ClusterScopedRules grants
// tenants full CRUD on persistentvolumes on the stated grounds that they are
// "Prefixed by the convertor, so tenants cannot collide or reach each other's".
// That is true of the name and false of everything under spec.
//
// Two ways it was an escape, both reproduced:
//
//   - hostPath. A tenant creates a PV with hostPath: {path: /}, binds it with
//     its own PVC and mounts it in a pod. Restricted Pod Security does NOT stop
//     this: check_restrictedVolumes short-circuits on
//     volume.PersistentVolumeClaim != nil and moves on, because the
//     volume-source check is meant for inline volumes. The node's root
//     filesystem -- other tenants' pod volumes and the kubelet's own
//     credentials -- mounts into a tenant container. No driver needed.
//   - Namespace-qualified secret references. CSIPersistentVolumeSource alone
//     carries five *SecretReference fields, and CephFS, RBD, ISCSI, ScaleIO,
//     StorageOS, FlexVolume and AzureFile carry more. Their namespaces were
//     passed through byte for byte, so a tenant could name any other tenant's
//     namespace, and the kubelet reads them with ITS OWN credentials -- upstream
//     RBAC, the documented second line of defence, is not on that path at all.
//
// An allowlist rather than more namespace rewriting, because the failure mode of
// missing one field is silent and cross-tenant, and the list of fields grows
// with every Kubernetes release. Kubernetes itself treats a namespace-qualified
// secret reference as an admin-only construct: csi_mounter.go forces
// ns := c.pod.Namespace on the inline-ephemeral path for this same reason.
//
// What is allowed is what a tenant can be given safely: the dynamic-provisioning
// sources, which name nothing outside the volume itself. A tenant that needs one
// of the others is asking for a platform-level grant, and should be told so
// rather than silently handed the cluster.
func refuseUnsafePVSource(spec *internal.PersistentVolumeSpec, name string) error {
	source := spec.PersistentVolumeSource
	switch {
	case source.CSI != nil:
		// The driver itself is fine; the secret references are not. They name a
		// namespace, and the kubelet resolves them with its own credentials.
		for field, ref := range map[string]*internal.SecretReference{
			"controllerPublishSecretRef": source.CSI.ControllerPublishSecretRef,
			"nodeStageSecretRef":         source.CSI.NodeStageSecretRef,
			"nodePublishSecretRef":       source.CSI.NodePublishSecretRef,
			"controllerExpandSecretRef":  source.CSI.ControllerExpandSecretRef,
			"nodeExpandSecretRef":        source.CSI.NodeExpandSecretRef,
		} {
			if ref != nil {
				return errors.Errorf("persistentvolume %s: spec.csi.%s names a namespace, which the "+
					"kubelet resolves with its own credentials rather than yours. A StorageClass "+
					"carries those credentials on the platform's behalf: see "+
					"kubectl get storageclass", name, field)
			}
		}
		return nil
	case source.ISCSI != nil:
		// ⚠️ ISCSIPersistentVolumeSource DOES carry a SecretReference, and it has
		// a namespace -- unlike the inline ISCSIVolumeSource a pod may use. An
		// earlier version of this comment claimed the opposite and let it
		// through; the field is right there in the type.
		if source.ISCSI.SecretRef != nil {
			return errors.Errorf("persistentvolume %s: spec.iscsi.secretRef names a namespace, "+
				"which the kubelet resolves with its own credentials rather than yours", name)
		}
		return nil
	case source.NFS != nil, source.FC != nil, source.PortworxVolume != nil:
		// Named by address or device, with no reference to a namespaced object
		// and no path on the node.
		return nil
	case source.HostPath != nil, source.Local != nil:
		// ⚠️ Local belongs here with hostPath, and putting it in the allowlist was
		// the same mistake in a quieter spelling: spec.local.path is a path on the
		// node, so `local` with path "/" is exactly the hostPath escape. What
		// makes both reachable is that restricted Pod Security short-circuits on
		// volume.PersistentVolumeClaim != nil -- the volume-source checks are
		// written for inline volumes -- so binding either through a PVC walks
		// straight past them and mounts the node's filesystem, other tenants' pod
		// volumes and the kubelet's credentials included.
		return errors.Errorf("persistentvolume %s: a hostPath or local volume exposes the node's "+
			"filesystem to whoever mounts it, and a PersistentVolumeClaim is not subject to the "+
			"Pod Security volume checks", name)
	case (source == internal.PersistentVolumeSource{}):
		// No source at all. Left to upstream, which requires exactly one and says
		// so clearly; refusing here would only replace a good error with a worse
		// one, and there is nothing to reach with.
		return nil
	}
	return errors.Errorf("persistentvolume %s: this volume source is not one tenants may write; "+
		"it either names an object in another namespace or reaches the node directly. "+
		"Use one of the storage classes the platform publishes -- kubectl get storageclass -- "+
		"or ask the platform operator", name)
}

// Backward transforms upstream object reference to tenant object reference.
func (v *PVTranformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	pv, ok := obj.(*internal.PersistentVolume)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of persistentvolume")
	}

	if pv.Spec.ClaimRef != nil && len(pv.Spec.ClaimRef.Namespace) > 0 {
		if !strings.HasPrefix(pv.Spec.ClaimRef.Namespace, tenantID) {
			return nil, errors.Errorf("invalid namespace %s in pv %s claim ref, tenant id is %s", pv.Spec.ClaimRef.Namespace, pv.Name, tenantID)
		}
		pv.Spec.ClaimRef.Namespace = util.TrimTenantIDPrefix(tenantID, pv.Spec.ClaimRef.Namespace)
	}

	return pv, nil
}
