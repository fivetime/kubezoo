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

package apiconfig

import (
	tenantlister "github.com/fivetime/kubezoo-contract/pkg/generated/listers/tenant/v1alpha1"
	"net/http"
	"net/url"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"k8s.io/apimachinery/pkg/util/managedfields"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/fivetime/kubezoo-contract/pkg/dynamic"

	"github.com/fivetime/kubezoo-gateway/pkg/publishedclass"
)

type APIGroupConfig struct {
	Group string
	// the map is version to resource to storage config
	StorageConfigs map[string]map[string]*StorageConfig
}

type StorageConfig struct {
	Kind        schema.GroupVersionKind
	Resource    string
	Subresource string
	ShortNames  []string

	NamespaceScoped bool

	IsCustomResource bool

	IsConnecter bool

	// TypeConverter reads an object against its schema, which is what lets a
	// server-side apply be forwarded upstream as an apply rather than resolved
	// here and written as an update. Nil disables that, and applies fall back to
	// being written as updates.
	TypeConverter managedfields.TypeConverter

	// NewFunc returns a new instance of the type this registry returns for a
	// GET of a single object, e.g.:
	//
	// curl GET /apis/group/version/namespaces/my-ns/myresource/name-of-object
	NewFunc func() runtime.Object

	// NewListFunc returns a new list of the type this registry; it is the
	// type returned when the resource is listed, e.g.:
	//
	// curl GET /apis/group/version/namespaces/my-ns/myresource
	NewListFunc func() runtime.Object

	// dynamic client is used to communicate with upstream cluster
	DynamicClient dynamic.Interface

	Convertor common.ObjectConvertor

	TableConvertor rest.TableConvertor

	// PublishedClasses turns this into a read-only view of the platform's OWN
	// cluster-scoped objects, served to every tenant under their real names and
	// narrowed to the ones the platform publishes. Nil means the ordinary tenant
	// proxy. See pkg/proxy/publicclass.go and pkg/publishedclass.
	PublishedClasses publishedclass.Set

	// PublishedStorageClasses lets an ordinary tenant proxy refuse a CREATE that
	// would newly reference a storage class the platform has marked as going
	// away. Nil disables the check.
	//
	// ⚠️ NOT the same field as PublishedClasses above, and setting the wrong one
	// is not a compile error. PublishedClasses CHANGES WHAT THIS STORAGE IS --
	// NewTenantProxy branches on it and returns the read-only published view
	// instead. Setting it on persistentvolumeclaims would turn the PVC endpoint
	// into a read-only list of storage classes.
	PublishedStorageClasses publishedclass.Set

	// PublishedVolumeAttributesClasses does the same for
	// spec.volumeAttributesClassName. Nil disables the check.
	//
	// ⚠️ That field is MUTABLE after the claim is bound, so unlike the storage
	// class the refusal cannot be create-only -- it fires whenever the value
	// changes. See tenantProxy.refuseUnpublishedVolumeAttributesClass.
	PublishedVolumeAttributesClasses publishedclass.Set

	// PublishedDeviceClasses lets a guard refuse a ResourceClaim that names a
	// DeviceClass the platform has not published.
	//
	// ⚠️ NOT PublishedClasses. That field CHANGES WHAT THE STORAGE IS -- a
	// read-only view of the platform's own objects -- and setting it here would
	// replace the ResourceClaim endpoint with a list of device classes. It
	// compiles.
	PublishedDeviceClasses publishedclass.Set

	// Tenants reads the Tenant objects, so that a per-tenant capacity can be
	// raised by editing one object rather than by restarting the gateway.
	//
	// ⛔ The limits below are only DEFAULTS for a tenant that names none. A flag
	// cannot express "this tenant may have more", and reaching for one means a
	// restart of a single-replica StatefulSet -- every tenant's API interrupted
	// to change a number for one of them.
	Tenants tenantlister.TenantLister

	// MaxNamespaces is the default cap when a tenant names none. Zero means no
	// cap. Set only on the namespaces resource.
	MaxNamespaces int

	// MaxCRDs is the default cap when a tenant names none. Zero
	// means no cap.
	MaxCRDs int

	// MaxClusterRoleBindings is the default cap when a tenant names none. Zero
	// means no cap.
	// Set only on the clusterrolebindings resource.
	MaxClusterRoleBindings int

	ProxyTransport       http.RoundTripper
	UpstreamMaster       *url.URL
	GroupVersionKindFunc GroupVersionKindFunc
}

type GroupVersionKindFunc func(containingGV schema.GroupVersion) schema.GroupVersionKind
