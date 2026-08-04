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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	volumehelpers "k8s.io/component-helpers/storage/volume"
	core "k8s.io/kubernetes/pkg/apis/core"
)

const dynamicVolume = "pvc-b956aeb5-6674-4da4-8339-16377b60e91e"

func claim(volumeName string, boundByCtrl bool) *core.PersistentVolumeClaim {
	c := &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: testTenant + "-default"},
		Spec:       core.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}
	if boundByCtrl {
		c.Annotations = map[string]string{volumehelpers.AnnBoundByController: "yes"}
	}
	return c
}

// TestADynamicallyProvisionedClaimIsReadable is the bug a real CSI driver found.
//
// The provisioner creates the volume directly upstream as pvc-<uid>, so it never
// passes through kubezoo and never carries the tenant prefix. Refusing to convert
// it failed the whole object: `kubectl get pvc` returned an error rather than a
// list, and the tenant could not even delete the claim.
func TestADynamicallyProvisionedClaimIsReadable(t *testing.T) {
	c := claim(dynamicVolume, true)
	out, err := NewPVCTransformer().Backward(c, testTenant)
	if err != nil {
		t.Fatalf("a claim bound to a dynamically provisioned volume could not be read back: %v\n"+
			"one such object fails the entire list, and the tenant cannot delete what it cannot read", err)
	}
	if got := out.(*core.PersistentVolumeClaim).Spec.VolumeName; got != dynamicVolume {
		t.Errorf("volumeName came back as %q, want the upstream name %q -- the tenant has no other name for it", got, dynamicVolume)
	}
}

// TestForwardLeavesAControllerBoundVolumeAlone -- spec.volumeName is immutable
// once bound, so prefixing it on an update makes upstream refuse every later
// write to the claim: a label, an annotation, a finalizer removal.
func TestForwardLeavesAControllerBoundVolumeAlone(t *testing.T) {
	c := claim(dynamicVolume, true)
	out, err := NewPVCTransformer().Forward(c, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*core.PersistentVolumeClaim).Spec.VolumeName; got != dynamicVolume {
		t.Errorf("volumeName went upstream as %q; it is immutable and upstream would refuse the write", got)
	}
}

// TestATenantPreBoundVolumeStillTranslates is the case that must NOT regress:
// a volume the tenant created itself is cluster-scoped and therefore prefixed,
// and the claim naming it has to reach that name.
func TestATenantPreBoundVolumeStillTranslates(t *testing.T) {
	tr := NewPVCTransformer()

	up, err := tr.Forward(claim("myvol", false), testTenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := up.(*core.PersistentVolumeClaim).Spec.VolumeName; got != testTenant+"-myvol" {
		t.Fatalf("a tenant's own volume went upstream as %q, want %q -- it would bind to nothing",
			got, testTenant+"-myvol")
	}

	back, err := tr.Backward(claim(testTenant+"-myvol", false), testTenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.(*core.PersistentVolumeClaim).Spec.VolumeName; got != "myvol" {
		t.Errorf("the tenant reads its own volume back as %q, want the name it wrote", got)
	}
}

// TestTheTwoDirectionsRoundTrip is the property the whole scheme rests on:
// whatever Backward hands a tenant, Forward has to turn back into the same
// upstream string, or the next update to that claim is refused.
func TestTheTwoDirectionsRoundTrip(t *testing.T) {
	tr := NewPVCTransformer()
	for _, tc := range []struct {
		name     string
		upstream string
		byCtrl   bool
	}{
		{"dynamically provisioned", dynamicVolume, true},
		{"pre-bound by the tenant", testTenant + "-myvol", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back, err := tr.Backward(claim(tc.upstream, tc.byCtrl), testTenant)
			if err != nil {
				t.Fatal(err)
			}
			seen := back.(*core.PersistentVolumeClaim)
			// What the tenant sends back is what it was given.
			fwd, err := tr.Forward(claim(seen.Spec.VolumeName, tc.byCtrl), testTenant)
			if err != nil {
				t.Fatal(err)
			}
			if got := fwd.(*core.PersistentVolumeClaim).Spec.VolumeName; got != tc.upstream {
				t.Errorf("round trip gave %q, want %q -- an update to this claim would be refused", got, tc.upstream)
			}
		})
	}
}

// TestAnUnattributableVolumeNameDoesNotFailTheObject covers the state that
// should not exist and sometimes does: no annotation, no prefix -- a claim
// written before this fix, or one an operator edited upstream.
//
// ⚠️ The rule here came out of the RoleBinding bug: what cannot be attributed is
// returned as it is. Erroring would fail the whole list for the sake of one
// object, which is how a tenant loses sight of everything it owns.
func TestAnUnattributableVolumeNameDoesNotFailTheObject(t *testing.T) {
	out, err := NewPVCTransformer().Backward(claim("someone-elses-volume", false), testTenant)
	if err != nil {
		t.Fatalf("an unattributable volumeName failed the conversion: %v\n"+
			"one object like this makes `kubectl get pvc` return an error instead of a list", err)
	}
	if got := out.(*core.PersistentVolumeClaim).Spec.VolumeName; got != "someone-elses-volume" {
		t.Errorf("got %q, want it returned unchanged", got)
	}
}
