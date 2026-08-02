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
	"strings"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/storage"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"github.com/fivetime/kubezoo-gateway/pkg/publishedclass"
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
	}, publishedclass.Static("storageclass", published))
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

// TestRefuseUnpublishedStorageClass pins the answers the guard has to give.
//
// ⭐ An unpublished class is REFUSED. That is a deliberate reversal: publication
// used to be discovery only, so a tenant that learned a platform-internal class
// name out of band could still provision on it. The cost, accepted knowingly, is
// that removing a label now stops new claims at once -- see the comment on
// refuseUnpublishedStorageClass and the inventory step in docs/operations-cn.md.
func TestRefuseUnpublishedStorageClass(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for name, value := range map[string]string{
		"fast-ssd":        common.PublishedTrue,
		"legacy-spinning": common.PublishedDeprecated,
	} {
		if err := store.Add(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{common.StorageClassPublishedLabelKey: value},
		}}); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
	}
	proxy := &tenantProxy{publishedStorageClasses: publishedclass.New("storageclass",
		common.StorageClassPublishedLabelKey, store, func() bool { return true }, nil)}

	claim := func(class *string) *core.PersistentVolumeClaim {
		return &core.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team"},
			Spec:       core.PersistentVolumeClaimSpec{StorageClassName: class},
		}
	}
	name := func(s string) *string { return &s }

	for _, tc := range []struct {
		what    string
		object  runtime.Object
		refused bool
	}{
		{"a published class", claim(name("fast-ssd")), false},
		{"a retired class", claim(name("legacy-spinning")), true},
		{"a class the platform never published", claim(name("platform-internal")), true},
		// Empty is not "names no class": it is a request for the default class,
		// which the setdefault admission plugin fills in upstream. Refusing it
		// would refuse most PVCs ever written.
		//
		// ⚠️ These two now guard the single line that keeps the check from
		// refusing MOST PVCs ever written: Visible("") is false, so without the
		// explicit empty short-circuit every claim leaving the class to the
		// default would be turned away. They were vacuous while the check only
		// looked at Retired; deleting that line now turns them red.
		{"the default class, named by omission", claim(name("")), false},
		{"the default class, named by a nil pointer", claim(nil), false},
		// The guard runs on every create the proxy serves, so it has to be inert
		// for everything that is not a claim.
		{"something that is not a claim at all", &core.ConfigMap{}, false},
	} {
		err := proxy.refuseUnpublishedStorageClass(tc.object)
		if tc.refused && err == nil {
			t.Errorf("%s: accepted, want refused", tc.what)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s: refused with %v, want accepted", tc.what, err)
		}
		if tc.refused && err != nil {
			if !apierrors.IsInvalid(err) {
				t.Errorf("%s: refused with %T, want an Invalid so kubectl points at the field", tc.what, err)
			}
			// The tenant cannot list what it may use unless the message says how.
			if !strings.Contains(err.Error(), "kubectl get storageclass") {
				t.Errorf("%s: the refusal does not tell the tenant where to look: %v", tc.what, err)
			}
		}
	}
}

// TestUnsyncedCacheAsksForARetry pins the startup window this reversal opened.
//
// ⚠️ Before the informer fills, the store is legitimately empty, and empty reads
// exactly like "the platform published nothing" -- so a naive check refuses every
// claim for the first seconds after each restart. The readiness gate keeps
// /readyz red until the informers sync, but a single-replica StatefulSet has
// nothing draining traffic away from it in the meantime.
//
// Unavailable rather than Invalid, and rather than letting the claim through: the
// tenant retries, which clients do; an operator can tell it from a real refusal;
// and the boundary does not open for the one window an attacker can arrange.
func TestUnsyncedCacheAsksForARetry(t *testing.T) {
	proxy := &tenantProxy{publishedStorageClasses: publishedclass.New("storageclass",
		common.StorageClassPublishedLabelKey, cache.NewStore(cache.MetaNamespaceKeyFunc),
		func() bool { return false }, nil)}

	class := "fast-ssd"
	err := proxy.refuseUnpublishedStorageClass(&core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec:       core.PersistentVolumeClaimSpec{StorageClassName: &class},
	})
	if err == nil {
		t.Fatal("a claim was accepted while the published set was still unknown")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Errorf("refused with %v, want ServiceUnavailable so the client retries", err)
	}

	// A claim that names no class does not depend on the cache and must not be
	// held up by it: it asks for the default, which the platform chose.
	if err := proxy.refuseUnpublishedStorageClass(&core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
	}); err != nil {
		t.Errorf("a claim asking for the default class was held up by the cache: %v", err)
	}
}

// TestRetiredCheckIsInertWithoutASet -- every other resource's proxy leaves the
// field nil, and must not pay for or be affected by this.
func TestRetiredCheckIsInertWithoutASet(t *testing.T) {
	proxy := &tenantProxy{}
	class := "legacy-spinning"
	if err := proxy.refuseUnpublishedStorageClass(&core.PersistentVolumeClaim{
		Spec: core.PersistentVolumeClaimSpec{StorageClassName: &class},
	}); err != nil {
		t.Errorf("a proxy with no published set refused a claim: %v", err)
	}
}

// TestRefuseUnpublishedVolumeAttributesClass pins the rule that differs from the
// storage class one.
//
// ⚠️ spec.volumeAttributesClassName is MUTABLE on a bound claim -- it is how a
// tenant asks for a different IOPS tier -- so the check cannot be create-only the
// way the storage class check is. But it must fire only when the value CHANGES,
// or every later write to a claim already naming the class fails, a GitOps
// controller reapplying an unchanged manifest among them.
func TestRefuseUnpublishedVolumeAttributesClass(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for name, value := range map[string]string{
		"gold":   common.PublishedTrue,
		"silver": common.PublishedDeprecated,
	} {
		if err := store.Add(&storagev1.VolumeAttributesClass{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{common.VolumeAttributesClassPublishedLabelKey: value},
		}}); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
	}
	proxy := &tenantProxy{publishedVolumeAttributesClasses: publishedclass.New(
		"volumeattributesclass", common.VolumeAttributesClassPublishedLabelKey,
		store, func() bool { return true }, nil)}

	claim := func(class *string) *core.PersistentVolumeClaim {
		return &core.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team"},
			Spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: class},
		}
	}
	name := func(s string) *string { return &s }

	for _, tc := range []struct {
		what    string
		new     *core.PersistentVolumeClaim
		old     runtime.Object // nil = create
		refused bool
	}{
		{"creating with a published class", claim(name("gold")), nil, false},
		{"creating with an unpublished class", claim(name("platform-nvme")), nil, true},
		{"creating with a retired class", claim(name("silver")), nil, true},
		// Empty means no class applied. Unlike storageClassName nothing upstream
		// fills it in, so empty really is "none".
		{"creating with none", claim(nil), nil, false},
		{"creating with the empty string", claim(name("")), nil, false},

		// ⭐ The mutable half. Raising your own tier on an existing claim is the
		// escape a create-only check would miss entirely.
		{"raising the tier on an existing claim", claim(name("platform-nvme")),
			claim(name("gold")), true},
		{"switching to a published tier", claim(name("gold")),
			claim(name("silver")), false},

		// ⚠️ And the half that must NOT fire. These are what make withdrawing a
		// class survivable for the tenants already on it.
		{"rewriting a claim without touching the class", claim(name("platform-nvme")),
			claim(name("platform-nvme")), false},
		{"reapplying an unchanged manifest on a retired class", claim(name("silver")),
			claim(name("silver")), false},
		{"dropping the class from a claim that had an unpublished one", claim(nil),
			claim(name("platform-nvme")), false},
	} {
		err := proxy.refuseUnpublishedVolumeAttributesClass(tc.new, tc.old)
		if tc.refused && err == nil {
			t.Errorf("%s: accepted, want refused", tc.what)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s: refused with %v, want accepted", tc.what, err)
		}
		if tc.refused && err != nil && !apierrors.IsInvalid(err) {
			t.Errorf("%s: refused with %T, want an Invalid naming the field", tc.what, err)
		}
	}
}

// TestVolumeAttributesClassUnsyncedAsksForARetry -- same startup window as the
// storage class check, same answer, for the same reason.
func TestVolumeAttributesClassUnsyncedAsksForARetry(t *testing.T) {
	proxy := &tenantProxy{publishedVolumeAttributesClasses: publishedclass.New(
		"volumeattributesclass", common.VolumeAttributesClassPublishedLabelKey,
		cache.NewStore(cache.MetaNamespaceKeyFunc), func() bool { return false }, nil)}

	class := "gold"
	err := proxy.refuseUnpublishedVolumeAttributesClass(&core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: &class},
	}, nil)
	if !apierrors.IsServiceUnavailable(err) {
		t.Errorf("refused with %v, want ServiceUnavailable so the client retries", err)
	}

	// An unchanged value must not wait on the cache either -- it is not this
	// write that put the tenant on the class.
	same := &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec:       core.PersistentVolumeClaimSpec{VolumeAttributesClassName: &class},
	}
	if err := proxy.refuseUnpublishedVolumeAttributesClass(same, same.DeepCopy()); err != nil {
		t.Errorf("an unchanged class was held up by the cache: %v", err)
	}
}

// TestRefuseTenantChosenNode covers the field that goes around the scheduler.
//
// ⚠️ CREATE only, and here that is the only workable rule rather than a
// preference: the scheduler writes spec.nodeName onto every pod it binds, so from
// then on every update to that pod carries it. Refusing on update would fail
// every later write to every running pod. This guard is wired into Create alone,
// which is the one operation where the field can only have come from the tenant.
func TestRefuseTenantChosenNode(t *testing.T) {
	proxy := &tenantProxy{}

	err := proxy.refuseTenantChosenNode(&core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "escape"},
		Spec:       core.PodSpec{NodeName: "another-tenants-node"},
	})
	if err == nil {
		t.Fatal("a pod naming its own node was accepted; it would skip the scheduler entirely")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("refused with %T, want an Invalid naming the field", err)
	}
	if !strings.Contains(err.Error(), "nodeName") {
		t.Errorf("the refusal does not name the field: %v", err)
	}

	if err := proxy.refuseTenantChosenNode(&core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ordinary"},
	}); err != nil {
		t.Errorf("a pod leaving the node to the scheduler was refused: %v", err)
	}
	// The guard runs on every create the proxy serves and must be inert for
	// everything that is not a pod.
	if err := proxy.refuseTenantChosenNode(&core.ConfigMap{}); err != nil {
		t.Errorf("a ConfigMap was refused: %v", err)
	}
}
