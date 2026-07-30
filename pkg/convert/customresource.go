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
	"strings"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubewharf/kubezoo/pkg/util"
)

// CustomResourceTransformer implements the transformation between
// client and upstream server for custom resource.
type CustomResourceTransformer struct{}

var _ ObjectTransformer = &CustomResourceTransformer{}

// NewCustomResourceTransformer initiates a CustomResourceTransformer
// which implements the ObjectTransformer interfaces.
func NewCustomResourceTransformer() ObjectTransformer {
	return &CustomResourceTransformer{}
}

// Forward transforms tenant object reference to upstream object reference.
func (t *CustomResourceTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, errors.New("fail to assert runtime object to the unstructured object")
	}
	groupVersion := u.GetAPIVersion()
	u.SetAPIVersion(util.AddTenantIDPrefix(tenantID, groupVersion))
	rewriteManagedFieldVersions(u, func(version string) string {
		return util.AddTenantIDPrefix(tenantID, version)
	})
	return u, nil
}

// Backward transforms upstream object reference to tenant object reference.
func (t *CustomResourceTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, errors.New("fail to assert runtime object to the unstructured object")
	}
	groupVersion := u.GetAPIVersion()
	if !strings.HasPrefix(groupVersion, tenantID) {
		return nil, errors.Errorf("invalid apiVersion %s in cr %s, tenant id is %s", groupVersion, u.GetName(), tenantID)
	}
	u.SetAPIVersion(util.TrimTenantIDPrefix(tenantID, groupVersion))
	rewriteManagedFieldVersions(u, func(version string) string {
		if !strings.HasPrefix(version, tenantID) {
			// Written before the versions were rewritten, or by something that
			// never went through here. Reading it back as it is beats refusing
			// to show the tenant its own object.
			return version
		}
		return util.TrimTenantIDPrefix(tenantID, version)
	})
	return u, nil
}

// rewriteManagedFieldVersions carries the group prefix into and out of the
// managed fields.
//
// Each entry records the apiVersion it was written against, and for a custom
// resource that version carries the tenant prefix upstream and must not carry it
// in the tenant's view. Leaving them alone was survivable only while nothing
// read them: once an apply is forwarded upstream as an apply, upstream records
// an entry naming the prefixed group, and the next apply hands that entry to the
// custom resource's own converter, which refuses -- "is unstructured and is not
// suitable for converting to 111111-example.com/v1" -- with the object created
// and the second apply failing, which is the worst shape a failure can have.
func rewriteManagedFieldVersions(u *unstructured.Unstructured, rewrite func(string) string) {
	entries := u.GetManagedFields()
	if len(entries) == 0 {
		return
	}
	rewritten := make([]metav1.ManagedFieldsEntry, len(entries))
	for i := range entries {
		rewritten[i] = entries[i]
		if rewritten[i].APIVersion != "" {
			rewritten[i].APIVersion = rewrite(rewritten[i].APIVersion)
		}
	}
	u.SetManagedFields(rewritten)
}
