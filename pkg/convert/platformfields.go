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

// The fields that name platform-owned cluster-scoped objects are cleared on the
// way in, so that a tenant setting them has no effect.
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
// break a great deal for no gain. So that the drop is not silent, the caller
// turns what is reported here into an admission warning, which kubectl prints on
// apply.
// DropPlatformOwnedFields clears the platform-owned class references on an
// object and reports which ones it actually cleared.
//
// It reports rather than only clearing so the caller can tell the tenant. A
// field dropped without saying so is the shape of defect this repository keeps
// turning up: the write succeeds, the value is not there, and nothing says so.
//
// Both the internal and the versioned form of each kind are handled. Requests
// arrive as internal objects, but the two families are distinct Go types with
// distinct PodSpecs, and a path that passed the versioned one would otherwise
// stop being covered -- which does not fail, it just quietly leaves the field in
// place and the escape open.
func DropPlatformOwnedFields(obj runtime.Object) []string {
	if podSpec := PodSpecOf(obj); podSpec != nil {
		return dropFromPodSpec(&podSpec.RuntimeClassName, &podSpec.PriorityClassName, &podSpec.Priority)
	}
	if podSpec := versionedPodSpecOf(obj); podSpec != nil {
		return dropFromPodSpec(&podSpec.RuntimeClassName, &podSpec.PriorityClassName, &podSpec.Priority)
	}
	switch ingress := obj.(type) {
	case *networking.Ingress:
		return dropFromIngress(&ingress.Spec.IngressClassName, ingress.Annotations)
	case *networkingv1.Ingress:
		return dropFromIngress(&ingress.Spec.IngressClassName, ingress.Annotations)
	}
	return nil
}

func dropFromPodSpec(runtimeClassName **string, priorityClassName *string, priority **int32) []string {
	var dropped []string
	if *runtimeClassName != nil {
		dropped = append(dropped, "spec.runtimeClassName")
		*runtimeClassName = nil
	}
	if *priorityClassName != "" {
		dropped = append(dropped, "spec.priorityClassName")
		*priorityClassName = ""
	}
	// Priority is the resolved number. Admission sets it from the class, but
	// clearing only the name would leave a value written directly.
	if *priority != nil {
		dropped = append(dropped, "spec.priority")
		*priority = nil
	}
	return dropped
}

func dropFromIngress(className **string, annotations map[string]string) []string {
	var dropped []string
	if *className != nil {
		dropped = append(dropped, "spec.ingressClassName")
		*className = nil
	}
	if _, ok := annotations[legacyIngressClassAnnotation]; ok {
		dropped = append(dropped, "the "+legacyIngressClassAnnotation+" annotation")
		delete(annotations, legacyIngressClassAnnotation)
	}
	return dropped
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
