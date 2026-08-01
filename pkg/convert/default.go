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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// DefaultConvertor implements the transformation between
// client and upstream server for generic default resource.
type DefaultConvertor struct {
	ownerRefTransformer OwnerReferenceTransformer
}

var _ common.ObjectConvertor = &DefaultConvertor{}

// NewDefaultConvertor initiates a DefaultConvertor which implements the
// ObjectConvertor interfaces.
func NewDefaultConvertor(ort OwnerReferenceTransformer) common.ObjectConvertor {
	return &DefaultConvertor{
		ownerRefTransformer: ort,
	}
}

// ConvertTenantObjectToUpstreamObject convert the tenant object to
// upstream object.
func (c *DefaultConvertor) ConvertTenantObjectToUpstreamObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if isNamespaceScoped && accessor.GetNamespace() != "" {
		prefixed := util.UpstreamNamespace(tenantID, accessor.GetNamespace())
		accessor.SetNamespace(prefixed)
	} else if !isNamespaceScoped {
		if accessor.GetName() != "" {
			accessor.SetName(util.AddTenantIDPrefix(tenantID, accessor.GetName()))
		}
		// ⚠️ generateName needs the prefix too, and used to get nothing. kubezoo
		// is not a genericregistry.Store, so rest.BeforeCreate never runs here and
		// the name is resolved by the *upstream* apiserver -- which is why
		// generateName works at all for a proxied resource. Unprefixed, upstream
		// named the object foo-abcde, outside the tenant's namespace of names
		// entirely: the tenant was handed that name back and could never address
		// it again, because every later request prefixes it and 404s; LIST dropped
		// it, because ownership is decided by the prefix; and teardown never
		// deleted it, for the same reason. It also let a tenant plant a name
		// inside another tenant's space -- generateName "111111-" sent by tenant
		// 222222 -- which refuseReservedName catches on the explicit-name path and
		// could not see here.
		if accessor.GetGenerateName() != "" {
			accessor.SetGenerateName(util.AddTenantIDPrefix(tenantID, accessor.GetGenerateName()))
		}
	}
	ownerReferences := accessor.GetOwnerReferences()
	for i := range ownerReferences {
		target, err := c.ownerRefTransformer.Forward(&ownerReferences[i], tenantID)
		if err != nil {
			return err
		}
		ownerReferences[i] = *target
	}
	accessor.SetOwnerReferences(ownerReferences)
	return nil
}

// ConvertUpstreamObjectToTenantObject convert the upstream object to
// tenant object.
func (c *DefaultConvertor) ConvertUpstreamObjectToTenantObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	// some object may be valid to have empty name and namespace, such as TokenReview
	if isNamespaceScoped {
		namespace := accessor.GetNamespace()
		trimmed := util.TrimTenantIDPrefix(tenantID, namespace)
		accessor.SetNamespace(trimmed)
	} else {
		name := accessor.GetName()
		trimmed := util.TrimTenantIDPrefix(tenantID, name)
		accessor.SetName(trimmed)
		// ⚠️ generateName is trimmed too, or the prefix accumulates. Forward adds
		// it; nothing used to take it off, so the tenant was handed back
		// generateName="111111-vol-" and every later write re-prefixed whatever it
		// sent. That is not only read-modify-write: a patch goes through
		// guaranteedUpdate, which re-fetches the tenant view -- already prefixed
		// -- applies the patch and forwards it, so kubectl annotate, label, edit
		// or a Helm upgrade each add seven bytes. Measured against a real 1.36
		// apiserver: forty updates grew it to 291 characters, all accepted,
		// because ValidateObjectMetaAccessorUpdate never looks at generateName.
		accessor.SetGenerateName(util.TrimTenantIDPrefix(tenantID, accessor.GetGenerateName()))
	}
	ownerReferences := accessor.GetOwnerReferences()
	for i := range ownerReferences {
		target, err := c.ownerRefTransformer.Backward(&ownerReferences[i], tenantID)
		if err != nil {
			return err
		}
		ownerReferences[i] = *target
	}
	accessor.SetOwnerReferences(ownerReferences)
	return nil
}
