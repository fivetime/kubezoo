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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/networking"
)

func podSpecNamingPlatformClasses() core.PodSpec {
	runtimeClass := "kata"
	priority := int32(2000000000)
	return core.PodSpec{
		RuntimeClassName:  &runtimeClass,
		PriorityClassName: "system-cluster-critical",
		Priority:          &priority,
	}
}

func templateNamingPlatformClasses() core.PodTemplateSpec {
	return core.PodTemplateSpec{Spec: podSpecNamingPlatformClasses()}
}

// TestPlatformFieldsAreDroppedFromEveryWorkload -- runtimeClassName and
// priorityClassName live in PodSpec, and a Deployment is the path almost
// everything really takes. Covering Pod alone would look done and leave the
// common case open.
func TestPlatformFieldsAreDroppedFromEveryWorkload(t *testing.T) {
	rcTemplate := templateNamingPlatformClasses()
	workloads := map[string]runtime.Object{
		"Pod":                   &core.Pod{Spec: podSpecNamingPlatformClasses()},
		"PodTemplate":           &core.PodTemplate{Template: templateNamingPlatformClasses()},
		"ReplicationController": &core.ReplicationController{Spec: core.ReplicationControllerSpec{Template: &rcTemplate}},
		"Deployment":            &apps.Deployment{Spec: apps.DeploymentSpec{Template: templateNamingPlatformClasses()}},
		"ReplicaSet":            &apps.ReplicaSet{Spec: apps.ReplicaSetSpec{Template: templateNamingPlatformClasses()}},
		"StatefulSet":           &apps.StatefulSet{Spec: apps.StatefulSetSpec{Template: templateNamingPlatformClasses()}},
		"DaemonSet":             &apps.DaemonSet{Spec: apps.DaemonSetSpec{Template: templateNamingPlatformClasses()}},
		"Job":                   &batch.Job{Spec: batch.JobSpec{Template: templateNamingPlatformClasses()}},
		"CronJob": &batch.CronJob{Spec: batch.CronJobSpec{
			JobTemplate: batch.JobTemplateSpec{Spec: batch.JobSpec{Template: templateNamingPlatformClasses()}},
		}},
	}

	for name, obj := range workloads {
		t.Run(name, func(t *testing.T) {
			out, err := NewPlatformFieldsTransformer().Forward(obj, testTenant)
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			podSpec := PodSpecOf(out)
			if podSpec == nil {
				t.Fatalf("PodSpecOf does not reach the pod spec of a %s", name)
			}
			if podSpec.RuntimeClassName != nil {
				t.Errorf("runtimeClassName = %q survived; a tenant can then name any runtime "+
					"handler the platform defines, including one outside the sandbox",
					*podSpec.RuntimeClassName)
			}
			if podSpec.PriorityClassName != "" {
				t.Errorf("priorityClassName = %q survived; system-cluster-critical resolves to "+
					"the highest priority in the cluster and preempts other tenants",
					podSpec.PriorityClassName)
			}
			if podSpec.Priority != nil {
				t.Errorf("priority = %d survived; clearing only the class name would leave a "+
					"number written directly", *podSpec.Priority)
			}
		})
	}
}

// TestIngressClassIsDroppedBothWays -- the field is the modern spelling and the
// annotation is the old one, still honoured by most controllers. Clearing one
// leaves the choice open through the other.
func TestIngressClassIsDroppedBothWays(t *testing.T) {
	className := "platform-nginx"
	ingress := &networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				legacyIngressClassAnnotation: "platform-nginx",
				"keep-me":                    "yes",
			},
		},
		Spec: networking.IngressSpec{IngressClassName: &className},
	}

	out, err := NewPlatformFieldsTransformer().Forward(ingress, testTenant)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	converted := out.(*networking.Ingress)
	if converted.Spec.IngressClassName != nil {
		t.Errorf("ingressClassName = %q survived", *converted.Spec.IngressClassName)
	}
	if _, ok := converted.Annotations[legacyIngressClassAnnotation]; ok {
		t.Error("the deprecated ingress.class annotation survived, so the controller can still " +
			"be chosen through it")
	}
	if converted.Annotations["keep-me"] != "yes" {
		t.Error("an unrelated annotation was removed")
	}
}

// TestPlatformFieldsRefusesWhatItCannotHandle: the transformer is registered per
// kind, so being handed something else means the registration is wrong, and
// quietly returning it unchanged would hide that.
func TestPlatformFieldsRefusesWhatItCannotHandle(t *testing.T) {
	if _, err := NewPlatformFieldsTransformer().Forward(&core.ConfigMap{}, testTenant); err == nil {
		t.Error("Forward accepted a ConfigMap, which carries none of these fields")
	}
}
