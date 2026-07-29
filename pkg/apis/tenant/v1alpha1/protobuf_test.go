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

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestProtobufRoundTripKeepsEveryField guards the quietest failure this API can
// have.
//
// KubeZoo stores its own objects with protobuf as the media type, and the
// generated marshaller only knows the fields it was generated from. Add a field
// to the Go type, run the repository's codegen, and -- until the protobuf target
// existed -- the field would be accepted by the API server, reported as created,
// and silently absent when read back. Nothing errors: the write succeeds and the
// data is not there.
//
// Round-tripping a fully populated object through the type's own marshaller
// catches it, because a field the generated code does not know about does not
// survive.
func TestProtobufRoundTripKeepsEveryField(t *testing.T) {
	in := &Tenant{
		Spec: TenantSpec{
			ID: 111111,
			Quota: TenantQuota{
				Hard: corev1.ResourceList{"cpu": resource.MustParse("4")},
			},
			Suspension: &TenantSuspension{
				Mode:   SuspensionReadOnly,
				Reason: "unpaid invoice",
			},
		},
	}

	data, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &Tenant{}
	if err := out.Unmarshal(data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if out.Spec.ID != in.Spec.ID {
		t.Errorf("id = %d, want %d", out.Spec.ID, in.Spec.ID)
	}
	if len(out.Spec.Quota.Hard) != 1 {
		t.Errorf("quota did not survive: %+v", out.Spec.Quota)
	}
	if out.Spec.Suspension == nil {
		t.Fatal("suspension did not survive the round trip; generated.pb.go does not know the " +
			"field, so a suspended tenant would be stored as an operating one -- run " +
			"'make codegen' after changing this package")
	}
	if out.Spec.Suspension.Mode != in.Spec.Suspension.Mode ||
		out.Spec.Suspension.Reason != in.Spec.Suspension.Reason {
		t.Errorf("suspension = %+v, want %+v", out.Spec.Suspension, in.Spec.Suspension)
	}
}
