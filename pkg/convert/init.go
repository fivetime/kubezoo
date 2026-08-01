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

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// InitConvertors initialize native convertor and custom convertor.
//
// publicIngressClasses are the IngressClass names that reach the platform's own
// controller, and so the public internet; every other class a tenant names is
// prefixed and can only be served by something the tenant runs itself.
func InitConvertors(checkGroupKind util.CheckGroupKindFunc, listTenantCRDs ListTenantCRDsFunc,
	publicIngressClasses []string) (nativeConvertor, customConvertor common.ObjectConvertor) {
	ownerReferenceTransformer := NewOwnerReferenceTransformer(checkGroupKind)
	objectReferenceTransformer := NewObjectReferenceTransformer(checkGroupKind)
	defaultConvertor := NewDefaultConvertor(ownerReferenceTransformer)

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
			// spec.ingressClassName names an IngressClass, which is
			// cluster-scoped and therefore prefixed -- the same shape as
			// spec.volumeName below. Left alone, a tenant's own class was
			// unreachable and the platform's was.
			Group: "networking.k8s.io",
			Kind:  "Ingress",
		}: NewCrossReferenceConverter(defaultConvertor, NewIngressTransformer(publicIngressClasses)),
		{
			Group: "discovery.k8s.io",
			Kind:  "EndpointSlice",
		}: NewCrossReferenceConverter(defaultConvertor, NewEndpointSliceTransformer(objectReferenceTransformer)),
		{
			Group: "",
			Kind:  "Event",
		}: NewCrossReferenceConverter(defaultConvertor, NewEventTransformer(objectReferenceTransformer)),
		{
			Group: "events.k8s.io",
			Kind:  "Event",
		}: NewCrossReferenceConverter(defaultConvertor, NewEventsV1Transformer(objectReferenceTransformer)),
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

		// There is deliberately no entry mapping a kind to a convertor that does
		// nothing. Three of the isolation audit's findings were that shape --
		// PersistentVolume was mapped to one while the read path filtered on the
		// prefix, so a tenant's volume landed upstream under a bare name that
		// nobody could then see or delete, and Node was exempted in three
		// separate places at once. An unconverted cluster-scoped kind is a name
		// every tenant shares. The default convertor, which prefixes, is the
		// safe thing to fall through to.
		//
		// The two entries removed from here were scheduling.k8s.io/PriorityClass,
		// which kubezoo does not serve, and policy/PodSecurityPolicy, a kind
		// Kubernetes deleted in 1.25. Neither could ever match; had PriorityClass
		// later been served, the entry would have reproduced the volume bug
		// silently.
	}

	nativeConvertor = NewNativeObjectConvertor(defaultConvertor, nativeKindToConvertors)
	customConvertor = NewCrossReferenceConverter(defaultConvertor, NewCustomResourceTransformer())
	return nativeConvertor, customConvertor
}
