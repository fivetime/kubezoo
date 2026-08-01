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

// TestPVTranformerForward tests the forward method of the PVTranformer.
func TestPVTranformerForward(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		in     internal.PersistentVolume
		want   internal.PersistentVolume
	}{
		{
			name:   "test forward pv",
			tenant: "111111",
			in: internal.PersistentVolume{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolume",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pv",
				},
				Spec: internal.PersistentVolumeSpec{
					Capacity: internal.ResourceList{
						internal.ResourceStorage: resource.MustParse("10Gi"),
					},
					ClaimRef: &internal.ObjectReference{
						Namespace: "default",
						Name:      "pvc-2",
					},
				},
			},
			want: internal.PersistentVolume{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolume",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pv",
				},
				Spec: internal.PersistentVolumeSpec{
					Capacity: internal.ResourceList{
						internal.ResourceStorage: resource.MustParse("10Gi"),
					},
					ClaimRef: &internal.ObjectReference{
						Namespace: "111111-default",
						Name:      "pvc-2",
					},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewPVTransformer()
			if _, err := e.Forward(&c.in, c.tenant); err != nil {
				t.Fatalf("failed to forward pv, err: %+v", err)
			}
			if !reflect.DeepEqual(c.in, c.want) {
				t.Errorf("out: %+v, want: %+v", c.in, c.want)
			}
		})
	}
}

// TestPVTranformerBackward tests the backward method of the PVTranformer.
func TestPVTranformerBackward(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		in     internal.PersistentVolume
		want   internal.PersistentVolume
	}{
		{
			name:   "test backward pv",
			tenant: "111111",
			in: internal.PersistentVolume{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolume",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pv",
				},
				Spec: internal.PersistentVolumeSpec{
					Capacity: internal.ResourceList{
						internal.ResourceStorage: resource.MustParse("10Gi"),
					},
					ClaimRef: &internal.ObjectReference{
						Namespace: "111111-default",
						Name:      "pvc-2",
					},
				},
			},
			want: internal.PersistentVolume{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PersistentVolume",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pv",
				},
				Spec: internal.PersistentVolumeSpec{
					Capacity: internal.ResourceList{
						internal.ResourceStorage: resource.MustParse("10Gi"),
					},
					ClaimRef: &internal.ObjectReference{
						Namespace: "default",
						Name:      "pvc-2",
					},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewPVTransformer()
			if _, err := e.Backward(&c.in, c.tenant); err != nil {
				t.Fatalf("failed to backward pv, err: %+v", err)
			}
			if !reflect.DeepEqual(c.in, c.want) {
				t.Errorf("out: %+v, want: %+v", c.in, c.want)
			}
		})
	}
}

// TestPVSourceAllowlist guards the widest cross-tenant hole found in seven
// rounds.
//
// ⚠️ Only spec.claimRef.namespace was rewritten; the rest of the spec was never
// looked at, while tenants hold full CRUD on persistentvolumes because the name
// is prefixed. Two escapes, both without any driver installed:
//   - hostPath: / bound by the tenant's own PVC and mounted in a pod. Restricted
//     Pod Security skips the volume-source check when the volume is a PVC, so
//     the node's root filesystem -- other tenants' pod volumes, the kubelet's
//     credentials -- mounts into a tenant container.
//   - a CSI secret ref naming another tenant's namespace, which the kubelet
//     resolves with its OWN credentials, so upstream RBAC never applies.
func TestPVSourceAllowlist(t *testing.T) {
	transformer := NewPVTransformer()

	refused := map[string]internal.PersistentVolumeSource{
		"hostPath reaches the node": {
			HostPath: &internal.HostPathVolumeSource{Path: "/"},
		},
		"a CSI secret ref names a namespace": {
			CSI: &internal.CSIPersistentVolumeSource{
				Driver: "d", VolumeHandle: "h",
				NodePublishSecretRef: &internal.SecretReference{Name: "creds", Namespace: "222222-default"},
			},
		},
		"a local volume is a node path too": {
			Local: &internal.LocalVolumeSource{Path: "/"},
		},
		"an ISCSI secret ref names a namespace, unlike the inline form": {
			ISCSI: &internal.ISCSIPersistentVolumeSource{
				TargetPortal: "p", IQN: "q", Lun: 0,
				SecretRef: &internal.SecretReference{Name: "chap", Namespace: "222222-default"},
			},
		},
		"a CSI secret ref in its own namespace is still refused": {
			CSI: &internal.CSIPersistentVolumeSource{
				Driver: "d", VolumeHandle: "h",
				NodeStageSecretRef: &internal.SecretReference{Name: "creds", Namespace: "111111-default"},
			},
		},
	}
	for what, source := range refused {
		t.Run("refused: "+what, func(t *testing.T) {
			pv := &internal.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "vol"},
				Spec:       internal.PersistentVolumeSpec{PersistentVolumeSource: source},
			}
			if _, err := transformer.Forward(pv, "111111"); err == nil {
				t.Error("accepted a volume source that reaches outside the tenant")
			}
		})
	}

	allowed := map[string]internal.PersistentVolumeSource{
		"a plain CSI volume": {
			CSI: &internal.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h"},
		},
		"NFS": {NFS: &internal.NFSVolumeSource{Server: "s", Path: "/x"}},
		"ISCSI without a secret ref": {
			ISCSI: &internal.ISCSIPersistentVolumeSource{TargetPortal: "p", IQN: "q", Lun: 0},
		},
	}
	for what, source := range allowed {
		t.Run("allowed: "+what, func(t *testing.T) {
			pv := &internal.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "vol"},
				Spec: internal.PersistentVolumeSpec{
					PersistentVolumeSource: source,
					ClaimRef:               &internal.ObjectReference{Namespace: "default", Name: "c"},
				},
			}
			out, err := transformer.Forward(pv, "111111")
			if err != nil {
				t.Fatalf("refused a volume source a tenant may safely write: %v", err)
			}
			if got := out.(*internal.PersistentVolume).Spec.ClaimRef.Namespace; got != "111111-default" {
				t.Errorf("claimRef namespace = %q, want 111111-default", got)
			}
		})
	}
}
