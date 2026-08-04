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

package convert

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apps "k8s.io/kubernetes/pkg/apis/apps"
	core "k8s.io/kubernetes/pkg/apis/core"
	saadmission "k8s.io/kubernetes/plugin/pkg/admission/serviceaccount"
	"k8s.io/utils/ptr"
)

func deploymentIn(ns string) *apps.Deployment {
	return &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
	}
}

// namespaceFieldPath returns what the pod's namespace file will be filled from,
// or "" if nothing fills it.
func namespaceFieldPath(spec *core.PodSpec) string {
	v := saVolume(spec)
	if v == nil || v.Projected == nil {
		return ""
	}
	for _, s := range v.Projected.Sources {
		if s.DownwardAPI == nil {
			continue
		}
		for _, item := range s.DownwardAPI.Items {
			if item.Path == "namespace" && item.FieldRef != nil {
				return item.FieldRef.FieldPath
			}
		}
	}
	return ""
}

// TestVolumeNameSuppressesUpstreamInjection pins the one string this whole
// mechanism rests on.
//
// If the volume kubezoo injects does not match what upstream's ServiceAccount
// admission plugin searches for, upstream adds a SECOND volume, and the first
// one in slice order wins the mount. Upstream appends, so upstream's would lose
// today -- but the ordering is not a contract, and the failure is silent either
// way: pods come up, tokens work, and only the namespace file is wrong again.
func TestVolumeNameSuppressesUpstreamInjection(t *testing.T) {
	if upstreamSAVolumePrefix != saadmission.ServiceAccountVolumeName {
		t.Fatalf("upstream renamed the service account volume: kubezoo looks for %q, "+
			"upstream searches for %q -- the injected volume no longer suppresses "+
			"upstream's and the namespace file goes back to the upstream name",
			upstreamSAVolumePrefix, saadmission.ServiceAccountVolumeName)
	}
	if !strings.HasPrefix(kubezooSAVolumeName, saadmission.ServiceAccountVolumeName+"-") {
		t.Fatalf("kubezoo injects %q, which upstream's HasPrefix test does not match", kubezooSAVolumeName)
	}
}

func TestTemplateGetsTheTenantsNameForItsNamespace(t *testing.T) {
	d := deploymentIn(testTenant + "-default")
	if _, err := NewSATokenNamespaceTransformer().Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}

	tmpl := &d.Spec.Template
	if got := tmpl.Annotations[TenantNamespaceAnnotation]; got != "default" {
		t.Errorf("the pod carries %q as its namespace, want the tenant's name %q", got, "default")
	}
	// ⚠️ On the TEMPLATE's metadata, not the Deployment's -- a downward API
	// selector inside a pod reads the pod's own annotations, and a Deployment's
	// never reach it.
	if _, onWorkload := d.Annotations[TenantNamespaceAnnotation]; onWorkload {
		t.Error("stamped on the Deployment instead of the pod template; the pod would never see it")
	}
	if got := namespaceFieldPath(&tmpl.Spec); got != "metadata.annotations['"+TenantNamespaceAnnotation+"']" {
		t.Errorf("the namespace file is filled from %q, so the pod still learns the upstream name", got)
	}
}

// TestForwardLeavesALivePodsVolumesAlone is the trap placement.go already paid for, with
// a sharper edge: spec.volumes is immutable once a pod is stored.
func TestForwardLeavesALivePodsVolumesAlone(t *testing.T) {
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-default"}}
	if _, err := NewSATokenNamespaceTransformer().Forward(pod, testTenant); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 0 {
		t.Fatal("a volume was added to a live pod: every update to a pod created before " +
			"this existed would be refused upstream, and the tenant could not touch it again")
	}
}

// TestAPodReadBackAndWrittenAgainKeepsItsNamespace is the hole Backward opens if
// an update does not re-stamp the annotation.
//
// Backward hides it, so a tenant doing kubectl edit -- or applying a manifest it
// rendered from a get -- sends a pod without it. The volume is immutable and
// stays, now pointing at an annotation that is gone, and a downward API selector
// for a missing key resolves to the EMPTY STRING with no error
// (fieldpath.ExtractFieldPathAsString). The pod's namespace file goes blank,
// which is worse than the upstream name this exists to replace.
func TestAPodReadBackAndWrittenAgainKeepsItsNamespace(t *testing.T) {
	tr := NewSATokenNamespaceTransformer()
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-default"}}
	ProjectPodNamespace(pod, testTenant) // as created

	if _, err := tr.Backward(pod, testTenant); err != nil { // as the tenant reads it
		t.Fatal(err)
	}
	if _, err := tr.Forward(pod, testTenant); err != nil { // and writes it back
		t.Fatal(err)
	}

	if got := pod.Annotations[TenantNamespaceAnnotation]; got != "default" {
		t.Fatalf("after a read-modify-write the pod carries %q; its namespace file is filled "+
			"from that annotation, and an absent key resolves to the empty string, so the pod "+
			"would come to believe it is in no namespace at all", got)
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Errorf("%d volumes after the write-back: adding one on an update is refused upstream, "+
			"because spec.volumes is immutable", len(pod.Spec.Volumes))
	}
}

func TestCreatingAPodProjectsIt(t *testing.T) {
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-team-a"}}
	ProjectPodNamespace(pod, testTenant)

	if got := pod.Annotations[TenantNamespaceAnnotation]; got != "team-a" {
		t.Errorf("pod carries %q, want %q", got, "team-a")
	}
	if got := namespaceFieldPath(&pod.Spec); !strings.Contains(got, TenantNamespaceAnnotation) {
		t.Errorf("the namespace file is filled from %q", got)
	}
}

// TestAnExistingServiceAccountVolumeIsRewrittenNotDuplicated covers the
// read-modify-write a tenant does with kubectl edit, and a hand-written volume.
func TestAnExistingServiceAccountVolumeIsRewrittenNotDuplicated(t *testing.T) {
	d := deploymentIn(testTenant + "-default")
	d.Spec.Template.Spec.Volumes = []core.Volume{{
		Name: "kube-api-access-abcde",
		VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{
			Sources: []core.VolumeProjection{{
				DownwardAPI: &core.DownwardAPIProjection{Items: []core.DownwardAPIVolumeFile{{
					Path:     "namespace",
					FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
				}}},
			}},
		}},
	}}

	if _, err := NewSATokenNamespaceTransformer().Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}

	vols := d.Spec.Template.Spec.Volumes
	if len(vols) != 1 {
		t.Fatalf("%d service account volumes; upstream mounts the first one it finds, so a "+
			"second is a coin flip over which namespace the pod believes", len(vols))
	}
	if got := namespaceFieldPath(&d.Spec.Template.Spec); !strings.Contains(got, TenantNamespaceAnnotation) {
		t.Errorf("the tenant's own volume still reads %q", got)
	}
}

// TestForwardIsIdempotent is what keeps a Deployment from rolling every time it
// is written: a changed spec.template is a new ReplicaSet.
func TestForwardIsIdempotent(t *testing.T) {
	tr := NewSATokenNamespaceTransformer()
	d := deploymentIn(testTenant + "-default")
	if _, err := tr.Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}
	first := d.DeepCopy()
	for i := 0; i < 3; i++ {
		if _, err := tr.Forward(d, testTenant); err != nil {
			t.Fatal(err)
		}
	}
	if !equality(first, d) {
		t.Error("writing the same Deployment twice produces a different pod template, " +
			"which rolls every pod the tenant runs on every no-op write")
	}
}

func equality(a, b *apps.Deployment) bool {
	return len(a.Spec.Template.Spec.Volumes) == len(b.Spec.Template.Spec.Volumes) &&
		a.Spec.Template.Annotations[TenantNamespaceAnnotation] == b.Spec.Template.Annotations[TenantNamespaceAnnotation]
}

// TestATenantCannotChooseWhatItsPodsBelieve -- the same rule as every other
// platform decision: a tenant-supplied value is overwritten, not honoured.
func TestATenantCannotChooseWhatItsPodsBelieve(t *testing.T) {
	d := deploymentIn(testTenant + "-default")
	d.Spec.Template.Annotations = map[string]string{TenantNamespaceAnnotation: "kube-system"}
	if _, err := NewSATokenNamespaceTransformer().Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Annotations[TenantNamespaceAnnotation]; got != "default" {
		t.Errorf("a tenant-set value survived: %q", got)
	}
}

// TestNoTokenAskedForMeansNoVolume -- injecting one anyway would make kubelet
// mint a token for a pod that asked not to have one.
func TestNoTokenAskedForMeansNoVolume(t *testing.T) {
	d := deploymentIn(testTenant + "-default")
	d.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(false)
	if _, err := NewSATokenNamespaceTransformer().Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 0 {
		t.Error("a token volume was injected into a pod that set automountServiceAccountToken: false")
	}
}

// TestBackwardHidesTheAnnotationAndKeepsTheVolume.
func TestBackwardHidesTheAnnotationAndKeepsTheVolume(t *testing.T) {
	tr := NewSATokenNamespaceTransformer()
	d := deploymentIn(testTenant + "-default")
	if _, err := tr.Forward(d, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Backward(d, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, present := d.Spec.Template.Annotations[TenantNamespaceAnnotation]; present {
		t.Error("a platform-internal annotation is in what the tenant reads back")
	}
	if saVolume(&d.Spec.Template.Spec) == nil {
		t.Error("the volume was hidden too: the tenant reads back a pod that looks like it " +
			"has no service account, which is not the pod that is running")
	}
}

// TestSubresourcesCarryNoTemplate -- a scale or a status reaches this convertor
// because the map is keyed by GroupKind.
func TestSubresourcesCarryNoTemplate(t *testing.T) {
	tr := NewSATokenNamespaceTransformer()
	binding := &core.Binding{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-default"}}
	if _, err := tr.Forward(binding, testTenant); err != nil {
		t.Fatalf("a pods/binding was refused by the projection: %v", err)
	}
	if _, err := tr.Backward(binding, testTenant); err != nil {
		t.Fatalf("a pods/binding was refused on the way back: %v", err)
	}
}
