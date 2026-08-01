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
	"net/http"
	"net/url"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"k8s.io/apimachinery/pkg/util/managedfields"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/fivetime/kubezoo-contract/pkg/dynamic"
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

	// PublishedNames turns this into a read-only view of a fixed set of the
	// platform's OWN cluster-scoped objects, served to every tenant under their
	// real names. Nil means the ordinary tenant proxy. See
	// pkg/proxy/publicclass.go.
	PublishedNames []string

	ProxyTransport       http.RoundTripper
	UpstreamMaster       *url.URL
	GroupVersionKindFunc GroupVersionKindFunc
}

type GroupVersionKindFunc func(containingGV schema.GroupVersion) schema.GroupVersionKind
