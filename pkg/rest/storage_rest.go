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

package test_rest

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
)

type RESTStorageProvider struct{}

var SchemeGroupVersion = schema.GroupVersion{Group: "tenant.kubezoo.io", Version: "v1alpha1"}

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// NewRESTStorage provide the rest storage for tenant.
func (p RESTStorageProvider) NewRESTStorage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (genericapiserver.APIGroupInfo, error) {
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo("tenant.kubezoo.io", legacyscheme.Scheme, legacyscheme.ParameterCodec, legacyscheme.Codecs)
	// If you add a version here, be sure to add an entry in `k8s.io/kubernetes/cmd/kube-apiserver/app/aggregator.go with specific priorities.
	// TODO refactor the plumbing to provide the information in the APIGroupInfo

	if apiResourceConfigSource.ResourceEnabled(SchemeGroupVersion.WithResource("tenants")) {
		storage, err := p.v1alpha1Storage(restOptionsGetter)
		if err != nil {
			return genericapiserver.APIGroupInfo{}, err
		}
		apiGroupInfo.VersionedResourcesStorageMap[SchemeGroupVersion.Version] = storage
	}

	return apiGroupInfo, nil
}

// v1alpha1Storage returns the storage for tenants.
//
// The error from NewREST used to be discarded here. When NewREST started failing
// -- 1.26 made SingularQualifiedResource mandatory and this store did not set it
// -- that put a typed-nil *REST into the map, and the server segfaulted on the
// first method call against it rather than reporting why it could not start.
func (p RESTStorageProvider) v1alpha1Storage(restOptionsGetter generic.RESTOptionsGetter) (map[string]rest.Storage, error) {
	tenantStorage, err := NewREST(legacyscheme.Scheme, restOptionsGetter)
	if err != nil {
		return nil, fmt.Errorf("building tenant storage: %w", err)
	}
	return map[string]rest.Storage{"tenants": tenantStorage}, nil
}

// GroupName returns the group name.
func (p RESTStorageProvider) GroupName() string {
	return SchemeGroupVersion.Group
}
