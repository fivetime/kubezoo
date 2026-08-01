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

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
)

// publicClassStorage serves a fixed set of the platform's own cluster-scoped
// objects to every tenant, read-only and under their real names.
//
// It exists because a tenant has to be able to NAME these objects for them to be
// any use -- a PersistentVolumeClaim's spec.storageClassName is passed upstream
// untranslated, so the reference already works today -- while having no way to
// discover which names exist. storage.k8s.io is not served at all, so
// `kubectl get storageclass` answers "the server doesn't have a resource type",
// and the tenant has to be told out of band. Worse, pkg/convert/pv.go refuses a
// volume source and tells the tenant to "Use a StorageClass", naming a resource
// it cannot enumerate.
//
// Deliberately NOT a tenantProxy with an extra flag. Everything the tenant proxy
// does here would be wrong:
//
//   - No name translation, in either direction. These are the platform's
//     objects; the tenant must see the name that actually works in a PVC.
//   - Visibility is an allowlist, not the tenant prefix. util.UpstreamObjectBelongsToTenant
//     would hide exactly the objects this is for.
//   - Read-only. There is no such thing as a tenant's StorageClass here; write
//     verbs are not installed, so they are 405 rather than a runtime refusal.
//
// Threading those three exceptions through tenantProxy as conditionals is how
// this would become a defect later -- the audits keep finding fields that one
// branch handled and another did not. A separate storage cannot leak into any
// other resource.
type publicClassStorage struct {
	kind           schema.GroupVersionKind
	resource       string
	shortNames     []string
	newFunc        func() runtime.Object
	newListFunc    func() runtime.Object
	tableConvertor rest.TableConvertor
	convertor      apiconfig.GroupVersionKindFunc

	dynamicClient kubezoodynamic.Interface
	// allowed is the set of names the platform publishes. Empty means the
	// resource is served but nothing is visible, which is the safe default: an
	// operator who has not chosen classes has not published any.
	allowed map[string]bool
}

var (
	_ rest.Storage              = &publicClassStorage{}
	_ rest.Getter               = &publicClassStorage{}
	_ rest.Lister               = &publicClassStorage{}
	_ rest.Watcher              = &publicClassStorage{}
	_ rest.Scoper               = &publicClassStorage{}
	_ rest.ShortNamesProvider   = &publicClassStorage{}
	_ rest.SingularNameProvider = &publicClassStorage{}
)

// NewPublicClassStorage builds the read-only view of a published cluster-scoped
// resource.
func NewPublicClassStorage(config apiconfig.StorageConfig, allowed []string) (rest.Storage, error) {
	if config.NewFunc == nil || config.NewListFunc == nil {
		return nil, fmt.Errorf("a published class needs both NewFunc and NewListFunc")
	}
	names := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		if name != "" {
			names[name] = true
		}
	}
	return &publicClassStorage{
		kind:           config.Kind,
		resource:       config.Resource,
		shortNames:     config.ShortNames,
		newFunc:        config.NewFunc,
		newListFunc:    config.NewListFunc,
		tableConvertor: config.TableConvertor,
		convertor:      config.GroupVersionKindFunc,
		dynamicClient:  config.DynamicClient,
		allowed:        names,
	}, nil
}

func (s *publicClassStorage) New() runtime.Object     { return s.newFunc() }
func (s *publicClassStorage) NewList() runtime.Object { return s.newListFunc() }
func (s *publicClassStorage) Destroy()                {}
func (s *publicClassStorage) NamespaceScoped() bool   { return false }
func (s *publicClassStorage) ShortNames() []string    { return s.shortNames }

func (s *publicClassStorage) GetSingularName() string {
	return strings.ToLower(s.kind.Kind)
}

func (s *publicClassStorage) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	if s.convertor == nil {
		return s.kind
	}
	return s.convertor(containingGV)
}

func (s *publicClassStorage) ConvertToTable(ctx context.Context, object runtime.Object,
	tableOptions runtime.Object) (*metav1.Table, error) {
	if s.tableConvertor == nil {
		return rest.NewDefaultTableConvertor(s.groupResource()).ConvertToTable(ctx, object, tableOptions)
	}
	return s.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}

func (s *publicClassStorage) groupResource() schema.GroupResource {
	return schema.GroupResource{Group: s.kind.Group, Resource: s.resource}
}

func (s *publicClassStorage) client() kubezoodynamic.ResourceInterface {
	return s.dynamicClient.Resource(s.kind.GroupVersion().WithResource(s.resource))
}

// Get returns one published object, and NotFound for anything else -- including
// an object that exists upstream but was not published. A tenant is not told
// that a class it may not use exists.
func (s *publicClassStorage) Get(ctx context.Context, name string,
	options *metav1.GetOptions) (runtime.Object, error) {

	if !s.allowed[name] {
		return nil, apierrors.NewNotFound(s.groupResource(), name)
	}
	// Still impersonated as the tenant, so upstream RBAC applies as well as the
	// allowlist. That is deliberate and it is the difference from a pure
	// filtering proxy: the tenant is granted get/list/watch on this resource
	// upstream (kubezoo-contract's ClusterScopedRules), and the allowlist narrows
	// what kubezoo will serve on top of that. Reading with kubezoo's own
	// credential instead would be simpler and would make this allowlist the only
	// boundary -- one filter with nothing behind it.
	got, err := s.client().Get(ctx, name, *options)
	if err != nil {
		return nil, err
	}
	return s.toOutput(got)
}

func (s *publicClassStorage) List(ctx context.Context,
	options *metainternalversion.ListOptions) (runtime.Object, error) {

	// No paging upstream: the published set is small and fixed, and a continue
	// token over the whole cluster's range is exactly the leak
	// clusterscopedcursor.go exists to stop. Each name is read individually.
	out := s.newListFunc()
	items := make([]unstructured.Unstructured, 0, len(s.allowed))
	for name := range s.allowed {
		got, err := s.client().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Published but not present. The operator named a class that does
				// not exist upstream; the tenant simply does not see it.
				continue
			}
			return nil, err
		}
		items = append(items, *got)
	}
	list := &unstructured.UnstructuredList{Items: items}
	list.SetAPIVersion(s.kind.GroupVersion().String())
	list.SetKind(s.kind.Kind + "List")
	// Through the scheme, not straight into the internal type: internal types
	// carry no json tags, so DefaultUnstructuredConverter silently produces an
	// empty object. Same route tenantProxy takes.
	origin, err := nativeScheme.New(s.kind.GroupVersion().WithKind(s.kind.Kind + "List"))
	if err != nil {
		return nil, err
	}
	raw, err := list.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &origin); err != nil {
		return nil, err
	}
	if err := nativeScheme.Convert(origin, out, context.TODO()); err != nil {
		return nil, err
	}
	return out, nil
}

// Watch is served as a single synthetic burst rather than a real upstream watch.
//
// A real watch would have to be filtered, and a filtered watch hands the client
// a revision it cannot safely resume from -- the problem watchmux.go documents
// at length for the namespace fan-out. The published set changes when an
// operator edits a flag and restarts, not while a client is watching, so a
// client that lists and then watches sees the truth and simply never sees an
// event. An informer over this stays correct; it just never has anything to do.
func (s *publicClassStorage) Watch(ctx context.Context,
	options *metainternalversion.ListOptions) (watch.Interface, error) {

	listed, err := s.List(ctx, options)
	if err != nil {
		return nil, err
	}
	events := make(chan watch.Event, len(s.allowed)+1)
	if options == nil || options.ResourceVersion == "" || options.ResourceVersion == "0" {
		// Only a watch that did not come from a list replays the set; one that
		// resumes from a revision has already been told.
		if err := meta.EachListItem(listed, func(object runtime.Object) error {
			events <- watch.Event{Type: watch.Added, Object: object}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return watch.NewProxyWatcher(events), nil
}

func (s *publicClassStorage) toOutput(utd *unstructured.Unstructured) (runtime.Object, error) {
	out := s.newFunc()
	original, err := nativeScheme.New(s.kind)
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(utd.Object, original); err != nil {
		return nil, err
	}
	if err := nativeScheme.Convert(original, out, context.TODO()); err != nil {
		return nil, err
	}
	return out, nil
}
