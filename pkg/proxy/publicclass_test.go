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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/kubernetes/pkg/apis/storage"

	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
)

// classFake serves a fixed set of upstream objects and records what was asked for.
type classFake struct {
	kubezoodynamic.Interface
	kubezoodynamic.NamespaceableResourceInterface
	present map[string]bool
	asked   []string
}

func (f *classFake) Resource(schema.GroupVersionResource) kubezoodynamic.NamespaceableResourceInterface {
	return f
}

func (f *classFake) Get(_ context.Context, name string, _ metav1.GetOptions,
	_ ...string) (*unstructured.Unstructured, error) {
	f.asked = append(f.asked, name)
	if !f.present[name] {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "storageclasses"}, name)
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{}}
	object.SetAPIVersion("storage.k8s.io/v1")
	object.SetKind("StorageClass")
	object.SetName(name)
	unstructured.SetNestedField(object.Object, "example.com/csi", "provisioner")
	return object, nil
}

func newClassStorage(t *testing.T, present []string, published []string) (rest.Storage, *classFake) {
	t.Helper()
	fake := &classFake{present: map[string]bool{}}
	for _, name := range present {
		fake.present[name] = true
	}
	s, err := NewPublicClassStorage(apiconfig.StorageConfig{
		Kind:          schema.GroupVersion{Group: "storage.k8s.io", Version: "v1"}.WithKind("StorageClass"),
		Resource:      "storageclasses",
		DynamicClient: fake,
		NewFunc:       func() runtime.Object { return &storage.StorageClass{} },
		NewListFunc:   func() runtime.Object { return &storage.StorageClassList{} },
	}, published)
	if err != nil {
		t.Fatalf("building the storage: %v", err)
	}
	return s, fake
}

// TestPublishedClassesAreTheOnlyOnesVisible is the whole point of this storage:
// a tenant sees the platform's classes it may use, under the names that actually
// work in a PersistentVolumeClaim, and nothing else.
//
// ⚠️ The names must NOT be tenant-prefixed. These are the platform's objects, and
// spec.storageClassName is passed upstream untranslated, so a prefixed name here
// would be one the tenant could read and then not use.
func TestPublishedClassesAreTheOnlyOnesVisible(t *testing.T) {
	// Upstream has four; the platform publishes two of them and one that does
	// not exist.
	s, fake := newClassStorage(t,
		[]string{"fast-ssd", "cheap-hdd", "platform-internal", "another-tenants"},
		[]string{"fast-ssd", "cheap-hdd", "typo-never-created"})

	lister := s.(rest.Lister)
	listed, err := lister.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	classes := listed.(*storage.StorageClassList)

	seen := map[string]bool{}
	for i := range classes.Items {
		seen[classes.Items[i].Name] = true
	}
	if !seen["fast-ssd"] || !seen["cheap-hdd"] {
		t.Errorf("a published class was not visible: %v", seen)
	}
	if seen["platform-internal"] || seen["another-tenants"] {
		t.Errorf("an unpublished class was visible: %v -- the tenant is not told what it may not use", seen)
	}
	if seen["typo-never-created"] {
		t.Error("a published name that does not exist upstream was invented")
	}
	if len(classes.Items) != 2 {
		t.Errorf("listed %d classes, want 2", len(classes.Items))
	}
	// The list must not read anything it was not published.
	for _, name := range fake.asked {
		if name == "platform-internal" || name == "another-tenants" {
			t.Errorf("the list read %q upstream even though it is not published", name)
		}
	}

	getter := s.(rest.Getter)
	got, err := getter.Get(context.Background(), "fast-ssd", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting a published class: %v", err)
	}
	if name := got.(*storage.StorageClass).Name; name != "fast-ssd" {
		t.Errorf("class came back as %q; the name must be the one that works in a PVC", name)
	}

	// An unpublished class that exists is NotFound, not Forbidden: a tenant is
	// not told which classes it may not use.
	if _, err := getter.Get(context.Background(), "platform-internal", &metav1.GetOptions{}); err == nil {
		t.Error("an unpublished class was readable by name")
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("refused, but not as NotFound: %v", err)
	}
}

// TestPublishingNothingShowsNothing pins the default. An operator who has not
// chosen classes has not offered any, and the resource must still be served --
// otherwise kubectl fails while building the request rather than showing an
// empty list, and pv.go's advice to run "kubectl get storageclass" is a lie again.
func TestPublishingNothingShowsNothing(t *testing.T) {
	s, fake := newClassStorage(t, []string{"fast-ssd"}, []string{})

	listed, err := s.(rest.Lister).List(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if items := listed.(*storage.StorageClassList).Items; len(items) != 0 {
		t.Errorf("published nothing but listed %d classes", len(items))
	}
	if len(fake.asked) != 0 {
		t.Errorf("published nothing but still read upstream: %v", fake.asked)
	}
	if _, err := s.(rest.Getter).Get(context.Background(), "fast-ssd", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("published nothing but a class was readable: %v", err)
	}
}

// TestPublishedClassesAreReadOnly -- a StorageClass is never a tenant's.
func TestPublishedClassesAreReadOnly(t *testing.T) {
	s, _ := newClassStorage(t, []string{"fast-ssd"}, []string{"fast-ssd"})

	if _, ok := s.(rest.Creater); ok {
		t.Error("the published-class storage implements Creater; a tenant must not write the platform's classes")
	}
	if _, ok := s.(rest.Updater); ok {
		t.Error("the published-class storage implements Updater")
	}
	if _, ok := s.(rest.GracefulDeleter); ok {
		t.Error("the published-class storage implements GracefulDeleter")
	}
	if _, ok := s.(rest.CollectionDeleter); ok {
		t.Error("the published-class storage implements CollectionDeleter")
	}
	if s.(rest.Scoper).NamespaceScoped() {
		t.Error("a StorageClass is cluster-scoped")
	}
}
