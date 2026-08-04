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
	"k8s.io/apimachinery/pkg/runtime/schema"
	internal "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// ObjectReferenceTransformer transforms tenant object reference to/from upstream object reference
type ObjectReferenceTransformer interface {
	// Forward transforms tenant object reference to upstream object reference
	Forward(or *internal.ObjectReference, tenantID string) (*internal.ObjectReference, error)
	// Backward transforms upstream object reference to tenant object reference
	Backward(or *internal.ObjectReference, tenantID string) (*internal.ObjectReference, error)
}

type objectReferenceTransformer struct {
	checkGroupKind util.CheckGroupKindFunc
}

// NewObjectReferenceTransformer returns a new ObjectReferenceTransformer
func NewObjectReferenceTransformer(checkGroupKind util.CheckGroupKindFunc) ObjectReferenceTransformer {
	return &objectReferenceTransformer{checkGroupKind: checkGroupKind}
}

// Forward transforms tenant object reference to upstream object reference
func (t *objectReferenceTransformer) Forward(or *internal.ObjectReference, tenantID string) (*internal.ObjectReference, error) {
	if or == nil {
		return nil, nil
	}
	gv, err := schema.ParseGroupVersion(or.APIVersion)
	if err != nil {
		return nil, err
	}

	namespaced, customResourceGroup, err := t.checkGroupKind(gv.Group, or.Kind, tenantID, true)
	if err != nil {
		// APIVersion and Kind of objectReference may be empty, in such cases we can't tell whether it is namespaced,
		// so just add tenantID prefix if namespace or name is not empty.
		if len(or.Namespace) != 0 {
			or.Namespace = util.UpstreamNamespace(tenantID, or.Namespace)
		} else if len(or.Name) != 0 {
			or.Name = util.AddTenantIDPrefix(tenantID, or.Name)
		}
		return or, nil
	}
	if !namespaced && len(or.Name) != 0 {
		or.Name = util.AddTenantIDPrefix(tenantID, or.Name)
	}
	if namespaced && len(or.Namespace) != 0 {
		or.Namespace = util.UpstreamNamespace(tenantID, or.Namespace)
	}
	if customResourceGroup && len(or.APIVersion) != 0 {
		or.APIVersion = util.AddTenantIDPrefix(tenantID, or.APIVersion)
	}
	return or, nil
}

// Backward transforms upstream object reference to tenant object reference.
func (t *objectReferenceTransformer) Backward(or *internal.ObjectReference, tenantID string) (*internal.ObjectReference, error) {
	if or == nil {
		return nil, nil
	}
	gv, err := schema.ParseGroupVersion(or.APIVersion)
	if err != nil {
		return nil, err
	}

	namespaced, customResourceGroup, err := t.checkGroupKind(gv.Group, or.Kind, tenantID, false)
	if err != nil {
		// APIVersion and Kind of objectReference may be empty, in such cases we can't tell whether it is namespaced,
		// so just trim tenantID prefix if namespace or name is not empty.
		if len(or.Namespace) != 0 {
			or.Namespace = util.TrimTenantIDPrefix(tenantID, or.Namespace)
		} else if len(or.Name) != 0 {
			or.Name = util.TrimTenantIDPrefix(tenantID, or.Name)
		}
		return or, nil
	}
	// ⚠️ An Event names whatever it is about, and controllers write events about
	// objects a tenant never touched. Failing the conversion took out the whole
	// event list, which is the first thing anyone reads when something is wrong.
	// See trimIfAttributable.
	if namespaced && len(or.Namespace) != 0 {
		or.Namespace = trimIfAttributable(or.Namespace, tenantID)
	}

	if !namespaced && len(or.Name) != 0 {
		or.Name = trimIfAttributable(or.Name, tenantID)
	}

	// ⚠️ Missed by hand and caught by the guard. An event about an object in a
	// SHARED platform CRD group -- a snapshot -- carries an apiVersion with no
	// tenant prefix by design, and this would have failed every event that
	// mentioned one.
	if customResourceGroup && len(or.APIVersion) != 0 {
		or.APIVersion = trimIfAttributable(or.APIVersion, tenantID)
	}
	return or, nil
}
