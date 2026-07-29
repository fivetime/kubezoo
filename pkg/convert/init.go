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
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubewharf/kubezoo/pkg/common"
	"github.com/kubewharf/kubezoo/pkg/util"
)

// InitConvertors initialize native convertor and custom convertor
func InitConvertors(checkGroupKind util.CheckGroupKindFunc, listTenantCRDs ListTenantCRDsFunc) (nativeConvertor, customConvertor common.ObjectConvertor) {
	ownerReferenceTransformer := NewOwnerReferenceTransformer(checkGroupKind)
	objectReferenceTransformer := NewObjectReferenceTransformer(checkGroupKind)
	defaultConvertor := NewDefaultConvertor(ownerReferenceTransformer)
	nopeConvertor := NewNopeConvertor()

	nativeKindToConvertors := map[schema.GroupKind]common.ObjectConvertor{
		{
			Group: "",
			Kind:  "Namespace",
		}: NewCrossReferenceConverter(defaultConvertor, NewNamespaceTransformer()),
		{
			Group: "",
			Kind:  "Endpoints",
		}: NewCrossReferenceConverter(defaultConvertor, NewEndpointsTransformer(objectReferenceTransformer)),
		{
			Group: "discovery.k8s.io",
			Kind:  "EndpointSlice",
		}: NewCrossReferenceConverter(defaultConvertor, NewEndpointSliceTransformer(objectReferenceTransformer)),
		{
			Group: "",
			Kind:  "Event",
		}: NewCrossReferenceConverter(defaultConvertor, NewEventTransformer(objectReferenceTransformer)),
		{
			Group: "apiextensions.k8s.io",
			Kind:  "CustomResourceDefinition",
		}: NewCRDConvertor(ownerReferenceTransformer),
		{
			// spec.volumeName names a PersistentVolume, which is cluster-scoped
			// and therefore prefixed. Without the transformer the claim named a
			// volume that does not exist under that name, so the whole PV/PVC
			// binding path went unconverted.
			Group: "",
			Kind:  "PersistentVolumeClaim",
		}: NewCrossReferenceConverter(defaultConvertor, NewPVCTransformer()),
		{
			// This was nopeConvertor -- no conversion at all -- while the read
			// path still filtered on the tenant prefix. A tenant's PV landed
			// upstream under a bare name that the tenant could then neither get
			// nor delete, a second tenant creating the same name got
			// AlreadyExists, and the object stayed upstream for good. The
			// default convertor supplies the name prefix; the transformer
			// rewrites spec.claimRef.namespace.
			Group: "",
			Kind:  "PersistentVolume",
		}: NewCrossReferenceConverter(defaultConvertor, NewPVTransformer()),
		{
			Group: "storage.k8s.io",
			Kind:  "VolumeAttachment",
		}: NewCrossReferenceConverter(defaultConvertor, NewVolumeAttachmentTransformer()),
		{
			Group: "rbac.authorization.k8s.io",
			Kind:  "ClusterRole",
		}: NewCrossReferenceConverter(defaultConvertor, NewClusterRoleTransformer(listTenantCRDs)),
		{
			Group: "rbac.authorization.k8s.io",
			Kind:  "ClusterRoleBinding",
		}: NewCrossReferenceConverter(defaultConvertor, NewClusterRoleBindingTransformer()),
		{
			Group: "rbac.authorization.k8s.io",
			Kind:  "Role",
		}: NewCrossReferenceConverter(defaultConvertor, NewRoleTransformer(listTenantCRDs)),
		{
			Group: "rbac.authorization.k8s.io",
			Kind:  "RoleBinding",
		}: NewCrossReferenceConverter(defaultConvertor, NewRoleBindingTransformer()),
		{
			Group: "authentication.k8s.io",
			Kind:  "TokenReview",
		}: NewCrossReferenceConverter(defaultConvertor, NewTokenReviewTransformer()),
		// An access review names a namespace, an API group and a subject, all in
		// the tenant's own namespace of names. Forwarded unconverted the question
		// upstream is a different question, and its answer was confidently wrong.
		{
			Group: "authorization.k8s.io",
			Kind:  "SelfSubjectAccessReview",
		}: NewCrossReferenceConverter(defaultConvertor, NewAccessReviewTransformer()),
		{
			Group: "authorization.k8s.io",
			Kind:  "LocalSubjectAccessReview",
		}: NewCrossReferenceConverter(defaultConvertor, NewAccessReviewTransformer()),
		{
			Group: "authorization.k8s.io",
			Kind:  "SubjectAccessReview",
		}: NewCrossReferenceConverter(defaultConvertor, NewAccessReviewTransformer()),
		{
			Group: "authorization.k8s.io",
			Kind:  "SelfSubjectRulesReview",
		}: NewCrossReferenceConverter(defaultConvertor, NewAccessReviewTransformer()),
		{
			Group: "admissionregistration.k8s.io",
			Kind:  "MutatingWebhookConfiguration",
		}: NewCrossReferenceConverter(defaultConvertor, NewWebhookConfigurationTransformer()),
		{
			Group: "admissionregistration.k8s.io",
			Kind:  "ValidatingWebhookConfiguration",
		}: NewCrossReferenceConverter(defaultConvertor, NewWebhookConfigurationTransformer()),

		// resources with nope convertor:
		{
			Group: "scheduling.k8s.io",
			Kind:  "PriorityClass",
		}: nopeConvertor,
		{
			Group: "policy",
			Kind:  "PodSecurityPolicy",
		}: nopeConvertor,
		{
			Group: "",
			Kind:  "Node",
		}: nopeConvertor,
	}
	nativeConvertor = NewNativeObjectConvertor(defaultConvertor, nativeKindToConvertors)
	customConvertor = NewCrossReferenceConverter(defaultConvertor, NewCustomResourceTransformer())
	return nativeConvertor, customConvertor
}
