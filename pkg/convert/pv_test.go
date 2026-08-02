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
	"strings"
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

// TestUnreservedPVIsRefused closes a cross-tenant data path.
//
// ⛔ A PersistentVolume is cluster-scoped and the binder never looks at tenancy:
// findByClaim matches on access modes, class, size and topology, and
// pv_controller only provisions dynamically when nothing matched -- so a static
// volume PRE-EMPTS the provisioner. Tenant A offers an NFS server it controls
// under a published class; tenant B's claim binds to it; B's pods mount A's
// storage.
//
// ⭐ The claimRef is what separates the legitimate use from the attack.
// FindMatchingVolume skips a volume whose claimRef names a different claim, so a
// reserved volume is invisible to everyone else. Refusing the class name would
// not work: a tenant's own claim may only name a published class too, so the
// legitimate use and the attack are the same write.
func TestUnreservedPVIsRefused(t *testing.T) {
	pv := func(ref *internal.ObjectReference) *internal.PersistentVolume {
		return &internal.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "offered"},
			Spec: internal.PersistentVolumeSpec{
				ClaimRef:         ref,
				StorageClassName: "standard",
				PersistentVolumeSource: internal.PersistentVolumeSource{
					NFS: &internal.NFSVolumeSource{Server: "192.0.2.1", Path: "/exports/x"},
				},
			},
		}
	}

	for _, tc := range []struct {
		what    string
		ref     *internal.ObjectReference
		refused bool
	}{
		{"no claimRef at all -- offered to every claim in the cluster", nil, true},
		{"a claimRef with no namespace", &internal.ObjectReference{Name: "data"}, true},
		{"a claimRef with no name -- matches nothing, reserves nothing",
			&internal.ObjectReference{Namespace: "team"}, true},
		{"reserved for one of the tenant's own claims",
			&internal.ObjectReference{Namespace: "team", Name: "data"}, false},
	} {
		_, err := NewPVTransformer().Forward(pv(tc.ref), "111111")
		if tc.refused && err == nil {
			t.Errorf("%s: accepted", tc.what)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s: refused with %v", tc.what, err)
		}
		if tc.refused && err != nil && !strings.Contains(err.Error(), "claimRef") {
			t.Errorf("%s: the refusal does not name the field to set: %v", tc.what, err)
		}
	}
}

// TestReservedPVNamespaceIsStillPrefixed -- the reservation has to land in the
// tenant's own namespace, or it reserves the volume for somebody else's claim.
func TestReservedPVNamespaceIsStillPrefixed(t *testing.T) {
	pv := &internal.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "offered"},
		Spec: internal.PersistentVolumeSpec{
			ClaimRef: &internal.ObjectReference{Namespace: "team", Name: "data"},
			PersistentVolumeSource: internal.PersistentVolumeSource{
				NFS: &internal.NFSVolumeSource{Server: "192.0.2.1", Path: "/exports/x"},
			},
		},
	}
	got, err := NewPVTransformer().Forward(pv, "111111")
	if err != nil {
		t.Fatalf("converting: %v", err)
	}
	if ns := got.(*internal.PersistentVolume).Spec.ClaimRef.Namespace; ns != "111111-team" {
		t.Errorf("the reservation names namespace %q, want the tenant's own", ns)
	}
}
