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
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/networking"
)

// legacyIngressClassAnnotation selects an ingress controller the old way. It is
// deprecated and still honoured by most controllers, so clearing only the field
// would leave the choice open through the annotation.
const legacyIngressClassAnnotation = "kubernetes.io/ingress.class"

// PlatformFieldsTransformer clears the fields that name platform-owned
// cluster-scoped objects, so that a tenant setting them has no effect.
//
// runtimeClassName, ingressClassName and priorityClassName each name an object
// the platform owns and the tenant cannot see. Left alone they were wrong in
// both directions at once: a tenant's own class was unreachable, because
// cluster-scoped names carry the tenant prefix and these references were not
// rewritten, while the platform's classes were perfectly reachable by name. A
// tenant could ask for any runtime handler the platform defines -- including one
// outside whatever sandbox the platform relies on -- and could set
// system-cluster-critical, which resolves to the highest priority in the
// cluster and preempts every other tenant.
//
// Prefixing the references was considered and rejected. It would close the
// escape and also make the platform's shared classes, which are the only ones it
// makes sense to reference, permanently unreachable.
//
// So these are not the tenant's decision at all. The platform decides which
// runtime, which ingress controller and which priority a tenant's workloads get,
// by whatever means it likes -- a default class, an admission policy -- and what
// the tenant writes here is dropped on the way in.
//
// Dropping rather than rejecting is deliberate: ingressClassName in particular
// appears in almost every published example, and refusing those manifests would
// break a great deal for no gain. The cost is that the removal is quiet -- the
// tenant reads the object back without the field. Returning an admission warning
// would say so in band, and needs a warning recorder in the handler chain and a
// context on the transformer interface, neither of which exists yet.
type PlatformFieldsTransformer struct{}

var _ ObjectTransformer = &PlatformFieldsTransformer{}

// NewPlatformFieldsTransformer initiates a transformer that drops the fields
// naming platform-owned classes.
func NewPlatformFieldsTransformer() ObjectTransformer {
	return &PlatformFieldsTransformer{}
}

// Forward transforms tenant object reference to upstream object reference.
//
// Both the internal and the versioned form of each kind are handled. Requests
// arrive as internal objects, but the two families are distinct Go types with
// distinct PodSpecs, and a path that passed the versioned one would otherwise
// stop being covered -- which does not fail, it just quietly leaves the field in
// place and the escape open.
func (t *PlatformFieldsTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	if podSpec := PodSpecOf(obj); podSpec != nil {
		podSpec.RuntimeClassName = nil
		podSpec.PriorityClassName = ""
		// Priority is the resolved number. Admission sets it from the class, but
		// clearing only the name would leave a value a tenant had written
		// directly.
		podSpec.Priority = nil
		return obj, nil
	}
	if podSpec := versionedPodSpecOf(obj); podSpec != nil {
		podSpec.RuntimeClassName = nil
		podSpec.PriorityClassName = ""
		podSpec.Priority = nil
		return obj, nil
	}
	if ingress, ok := obj.(*networking.Ingress); ok {
		ingress.Spec.IngressClassName = nil
		delete(ingress.Annotations, legacyIngressClassAnnotation)
		return obj, nil
	}
	if ingress, ok := obj.(*networkingv1.Ingress); ok {
		ingress.Spec.IngressClassName = nil
		delete(ingress.Annotations, legacyIngressClassAnnotation)
		return obj, nil
	}
	return nil, errors.Errorf("fail to assert the runtime object to a kind carrying platform-owned class references")
}

// versionedPodSpecOf is PodSpecOf for the versioned types. They are a separate
// set of Go types with their own PodSpec, so one switch cannot serve both.
func versionedPodSpecOf(obj runtime.Object) *corev1.PodSpec {
	switch typed := obj.(type) {
	case *corev1.Pod:
		return &typed.Spec
	case *corev1.PodTemplate:
		return &typed.Template.Spec
	case *corev1.ReplicationController:
		if typed.Spec.Template == nil {
			return nil
		}
		return &typed.Spec.Template.Spec
	case *appsv1.Deployment:
		return &typed.Spec.Template.Spec
	case *appsv1.ReplicaSet:
		return &typed.Spec.Template.Spec
	case *appsv1.StatefulSet:
		return &typed.Spec.Template.Spec
	case *appsv1.DaemonSet:
		return &typed.Spec.Template.Spec
	case *batchv1.Job:
		return &typed.Spec.Template.Spec
	case *batchv1.CronJob:
		return &typed.Spec.JobTemplate.Spec.Template.Spec
	default:
		return nil
	}
}

// Backward transforms upstream object reference to tenant object reference.
//
// Nothing to undo: the tenant's value was dropped, and whatever the platform put
// there instead is what is really in effect, so it is what the tenant should
// see.
func (t *PlatformFieldsTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	return obj, nil
}

// PodSpecOf returns the pod spec an object carries, or nil if it carries none.
//
// The point of having this in one place is that runtimeClassName and
// priorityClassName live in PodSpec, and PodSpec is embedded in nine served
// kinds. Handling Pod alone would leave the common path -- a Deployment --
// untouched while looking complete, which is how the Node exemption came to be
// in three places with only one of them ever found.
func PodSpecOf(obj runtime.Object) *core.PodSpec {
	switch typed := obj.(type) {
	case *core.Pod:
		return &typed.Spec
	case *core.PodTemplate:
		return &typed.Template.Spec
	case *core.ReplicationController:
		if typed.Spec.Template == nil {
			return nil
		}
		return &typed.Spec.Template.Spec
	case *apps.Deployment:
		return &typed.Spec.Template.Spec
	case *apps.ReplicaSet:
		return &typed.Spec.Template.Spec
	case *apps.StatefulSet:
		return &typed.Spec.Template.Spec
	case *apps.DaemonSet:
		return &typed.Spec.Template.Spec
	case *batch.Job:
		return &typed.Spec.Template.Spec
	case *batch.CronJob:
		return &typed.Spec.JobTemplate.Spec.Template.Spec
	default:
		return nil
	}
}

// PlatformFieldGroupKinds lists the kinds whose platform-owned class references
// are dropped.
//
// Exported so that cmd/kubezoo/app can check it against the resources
// apigroups.go actually serves; that comparison cannot live here, since pkg
// cannot import cmd.
func PlatformFieldGroupKinds() []schema.GroupKind {
	return []schema.GroupKind{
		{Group: "", Kind: "Pod"},
		{Group: "", Kind: "PodTemplate"},
		{Group: "", Kind: "ReplicationController"},
		{Group: "apps", Kind: "Deployment"},
		{Group: "apps", Kind: "ReplicaSet"},
		{Group: "apps", Kind: "StatefulSet"},
		{Group: "apps", Kind: "DaemonSet"},
		{Group: "batch", Kind: "Job"},
		{Group: "batch", Kind: "CronJob"},
		{Group: "networking.k8s.io", Kind: "Ingress"},
	}
}
