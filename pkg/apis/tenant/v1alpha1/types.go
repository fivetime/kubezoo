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
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource/resourcestrategy"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// `spec` is the specification of the desired behavior of a flow-schema.
	// More info: https://git.k8s.io/community/contributors/devel/api-conventions.md#spec-and-status
	// +optional
	Spec TenantSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	// `status` is the current status of a flow-schema.
	// More info: https://git.k8s.io/community/contributors/devel/api-conventions.md#spec-and-status
	// +optional
	Status TenantStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// TenantList is a list of Tenant objects.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	// `metadata` is the standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// `items` is a list of tenant
	// +listType=atomic
	Items []Tenant `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// TenantSpec describes how the proxy-rule's specification looks like.
type TenantSpec struct {
	ID    int32       `json:"id" protobuf:"varint,1,name=id"`
	Quota TenantQuota `json:"quota" protobuf:"bytes,2,name=quota"`
	// Suspension stops a tenant from operating without touching what it is
	// running. Absent means the tenant is operating normally.
	// +optional
	Suspension *TenantSuspension `json:"suspension,omitempty" protobuf:"bytes,3,opt,name=suspension"`
}

// TenantSuspensionMode selects how far a suspension goes.
type TenantSuspensionMode string

const (
	// SuspensionReadOnly leaves the tenant able to see its objects but not to
	// change them. It is the billing case: the point is to prompt payment
	// without manufacturing an incident, and a tenant that cannot see its own
	// objects will reasonably conclude they are gone.
	SuspensionReadOnly TenantSuspensionMode = "ReadOnly"
	// SuspensionFrozen stops the tenant operating at all, while its workloads
	// keep running exactly as they are. It is the investigation case: the
	// tenant must not touch anything, and the evidence must not move.
	//
	// Frozen in the sense an account is frozen: reversible, and what was there
	// is still there. It is deliberately not called revoked -- nothing is
	// revoked. The tenant's certificate still authenticates; what is withdrawn
	// is its authorization, and lifting the suspension gives it straight back.
	//
	// It is the tenant's ability to act that is frozen, not its workloads.
	// Those keep running, which is the point in both the billing and the
	// investigation case. Kubernetes uses "suspend" for the opposite -- a
	// suspended Job stops -- so this is worth being explicit about.
	SuspensionFrozen TenantSuspensionMode = "Frozen"
)

// TenantSuspension describes a suspension in force.
type TenantSuspension struct {
	// Mode is how far the suspension goes.
	Mode TenantSuspensionMode `json:"mode" protobuf:"bytes,1,name=mode"`
	// Reason is shown to the tenant on every refused request, so that it reads
	// as a decision rather than as a malfunction.
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,2,opt,name=reason"`
}

type TenantQuota struct {
	// hard is the set of desired hard limits for each named resource.
	// More info: https://kubernetes.io/docs/concepts/policy/resource-quotas/
	// +optional
	Hard corev1.ResourceList `json:"hard,omitempty" protobuf:"bytes,1,rep,name=hard,casttype=ResourceList,castkey=ResourceName"`
}

// TenantStatus represents the current state of a rule.
type TenantStatus struct {
	// Current state of tenant.
	Online bool `json:"online,omitempty" protobuf:"bytes,1,name=online"`
}

var _ resource.Object = &Tenant{}
var _ resourcestrategy.Validater = &Tenant{}

// GetObjectMeta returns the object metadata of tenant.
func (t *Tenant) GetObjectMeta() *metav1.ObjectMeta {
	return &t.ObjectMeta
}

// NamespaceScoped returns whether the tenant is namespace scoped or not.
func (t *Tenant) NamespaceScoped() bool {
	return false
}

// New returns a tenant object.
func (t *Tenant) New() runtime.Object {
	return &Tenant{}
}

// NewList returns a list of tenant objects.
func (t *Tenant) NewList() runtime.Object {
	return &TenantList{}
}

// GetGroupVersionResource returns the group version resource of tenant.
func (t *Tenant) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "tenant.kubezoo.io",
		Version:  "v1alpha1",
		Resource: "tenants",
	}
}

// IsStorageVersion returns whether tenant is storage version or not.
func (t *Tenant) IsStorageVersion() bool {
	return false
}

// Validate does some validation.
func (t *Tenant) Validate(ctx context.Context) field.ErrorList {
	return nil
}

var _ resource.ObjectList = &TenantList{}

// GetListMeta returns list metadata.
func (in *TenantList) GetListMeta() *metav1.ListMeta {
	return &in.ListMeta
}

// SubResourceName returns subresource name.
func (in TenantStatus) SubResourceName() string {
	return "status"
}

// Xuchen implements ObjectWithStatusSubResource interface.
var _ resource.ObjectWithStatusSubResource = &Tenant{}

// GetStatus returns the sub resource of status.
func (in *Tenant) GetStatus() resource.StatusSubResource {
	return in.Status
}

// XuchenStatus{} implements StatusSubResource interface.
var _ resource.StatusSubResource = &TenantStatus{}

// CopyTo the status.
func (in TenantStatus) CopyTo(parent resource.ObjectWithStatusSubResource) {
	parent.(*Tenant).Status = in
}
