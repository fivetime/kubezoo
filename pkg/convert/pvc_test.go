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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	internal "k8s.io/kubernetes/pkg/apis/core"
)

// TestPVCTranformerForward tests the forward method of the PVCTranformer.
func TestPVCTranformerForward(t *testing.T) {
	scName := "my-sc"
	volumeMode := internal.PersistentVolumeFilesystem

	cases := []struct {
		name   string
		tenant string
		in     internal.PersistentVolumeClaim
		want   internal.PersistentVolumeClaim
	}{
		{
			name:   "test forward pvc",
			tenant: "111111",
			in: internal.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolumeClaim",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pvc",
				},
				Spec: internal.PersistentVolumeClaimSpec{
					AccessModes: []internal.PersistentVolumeAccessMode{
						internal.ReadWriteOnce,
					},
					Resources: internal.VolumeResourceRequirements{
						Requests: internal.ResourceList{
							internal.ResourceStorage: resource.MustParse("20Gi"),
						},
					},
					StorageClassName: &scName,
					VolumeMode:       &volumeMode,
					VolumeName:       "pv-1",
				},
			},
			want: internal.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolumeClaim",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pvc",
				},
				Spec: internal.PersistentVolumeClaimSpec{
					AccessModes: []internal.PersistentVolumeAccessMode{
						internal.ReadWriteOnce,
					},
					Resources: internal.VolumeResourceRequirements{
						Requests: internal.ResourceList{
							internal.ResourceStorage: resource.MustParse("20Gi"),
						},
					},
					StorageClassName: &scName,
					VolumeMode:       &volumeMode,
					VolumeName:       "111111-pv-1",
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewPVCTransformer()
			if _, err := e.Forward(&c.in, c.tenant); err != nil {
				t.Fatalf("failed to forward pvc, err: %+v", err)
			}
			if !reflect.DeepEqual(c.in, c.want) {
				t.Errorf("out: %+v, want: %+v", c.in, c.want)
			}
		})
	}
}

// TestPVCTranformerBackward tests the backward method of the PVCTranformer.
func TestPVCTranformerBackward(t *testing.T) {
	scName := "my-sc"
	volumeMode := internal.PersistentVolumeFilesystem

	cases := []struct {
		name   string
		tenant string
		in     internal.PersistentVolumeClaim
		want   internal.PersistentVolumeClaim
	}{
		{
			name:   "test forward pvc",
			tenant: "111111",
			in: internal.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolumeClaim",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pvc",
				},
				Spec: internal.PersistentVolumeClaimSpec{
					AccessModes: []internal.PersistentVolumeAccessMode{
						internal.ReadWriteOnce,
					},
					Resources: internal.VolumeResourceRequirements{
						Requests: internal.ResourceList{
							internal.ResourceStorage: resource.MustParse("20Gi"),
						},
					},
					StorageClassName: &scName,
					VolumeMode:       &volumeMode,
					VolumeName:       "111111-pv-1",
				},
			},
			want: internal.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolumeClaim",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pvc",
				},
				Spec: internal.PersistentVolumeClaimSpec{
					AccessModes: []internal.PersistentVolumeAccessMode{
						internal.ReadWriteOnce,
					},
					Resources: internal.VolumeResourceRequirements{
						Requests: internal.ResourceList{
							internal.ResourceStorage: resource.MustParse("20Gi"),
						},
					},
					StorageClassName: &scName,
					VolumeMode:       &volumeMode,
					VolumeName:       "pv-1",
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewPVCTransformer()
			if _, err := e.Backward(&c.in, c.tenant); err != nil {
				t.Fatalf("failed to backward pvc, err: %+v", err)
			}
			if !reflect.DeepEqual(c.in, c.want) {
				t.Errorf("out: %+v, want: %+v", c.in, c.want)
			}
		})
	}
}

// TestDataSourceRefNamespaceIsPrefixed covers the one field on a claim that can
// name another namespace.
//
// ⚠️ It was passing through untouched, so a tenant writing `namespace: default`
// meant the PLATFORM's default rather than its own. Not reachable today --
// CrossNamespaceVolumeDataSource has been alpha and default-off since 1.26, so
// the apiserver drops the field -- which is exactly why nothing would have
// noticed until someone turned the gate on.
func TestDataSourceRefNamespaceIsPrefixed(t *testing.T) {
	const tenantID = "111111"
	name := func(s string) *string { return &s }

	claim := &internal.PersistentVolumeClaim{
		Spec: internal.PersistentVolumeClaimSpec{
			DataSourceRef: &internal.TypedObjectReference{
				APIGroup:  name("snapshot.storage.k8s.io"),
				Kind:      "VolumeSnapshot",
				Name:      "snap",
				Namespace: name("default"),
			},
		},
	}
	got, err := NewPVCTransformer().Forward(claim, tenantID)
	if err != nil {
		t.Fatalf("converting a claim with a cross-namespace source: %v", err)
	}
	if ns := *got.(*internal.PersistentVolumeClaim).Spec.DataSourceRef.Namespace; ns != "111111-default" {
		t.Errorf("the source namespace is %q; unprefixed it names the platform's own", ns)
	}

	// And back, so the tenant reads what it wrote.
	back, err := NewPVCTransformer().Backward(got, tenantID)
	if err != nil {
		t.Fatalf("converting back: %v", err)
	}
	if ns := *back.(*internal.PersistentVolumeClaim).Spec.DataSourceRef.Namespace; ns != "default" {
		t.Errorf("the tenant reads back %q, want the name it wrote", ns)
	}
}

// TestDataSourceRefWithoutANamespaceIsLeftAlone -- the field is optional, and the
// ordinary case is a source in the claim's own namespace, which already rides the
// namespace prefix.
func TestDataSourceRefWithoutANamespaceIsLeftAlone(t *testing.T) {
	claim := &internal.PersistentVolumeClaim{
		Spec: internal.PersistentVolumeClaimSpec{
			DataSourceRef: &internal.TypedObjectReference{Kind: "PersistentVolumeClaim", Name: "src"},
		},
	}
	got, err := NewPVCTransformer().Forward(claim, "111111")
	if err != nil {
		t.Fatalf("converting: %v", err)
	}
	if ref := got.(*internal.PersistentVolumeClaim).Spec.DataSourceRef; ref.Namespace != nil {
		t.Errorf("a namespace was invented: %q", *ref.Namespace)
	}
}
