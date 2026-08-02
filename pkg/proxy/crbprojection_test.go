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
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/apis/rbac"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
)

// TestProjectedFieldSelectorNamesTheRecord guards the one call site the
// ListOptionScope fix structurally could not reach.
//
// ⚠️ The field selector went through untouched. util.ConvertInternalListOptions
// is handed the INNER storage's scope -- namespaced RoleBinding -- so it
// correctly leaves metadata.name alone, while the object the client asked about
// is a cluster-scoped ClusterRoleBinding whose record is named
// ProjectedBindingName(name). Both failures were silent:
//
//   - `metadata.name=my-crb` matched nothing, so a single-object informer watched
//     an empty world forever while `kubectl get clusterrolebinding my-crb` worked.
//   - `metadata.name!=keepme` matched EVERYTHING including keepme's own record,
//     because no record is literally named keepme -- so a DeleteCollection
//     carrying it deleted the object it was told to spare.
func TestProjectedFieldSelectorNamesTheRecord(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		want     string
		wantErr  bool
	}{
		{
			name:     "the single-object informer's selector",
			selector: "metadata.name=my-crb",
			want:     "metadata.name=kubezoo:clusterrolebinding:my-crb",
		},
		{
			name:     "the negative form, which used to match the object it excludes",
			selector: "metadata.name!=keepme",
			want:     "metadata.name!=kubezoo:clusterrolebinding:keepme",
		},
		{
			name:     "a namespace on a cluster-scoped object means nothing",
			selector: "metadata.namespace=default",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := fields.ParseSelector(tc.selector)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.selector, err)
			}
			scoped, err := withProjectedSelector(&metainternalversion.ListOptions{FieldSelector: selector})
			if tc.wantErr {
				if err == nil {
					t.Errorf("accepted %q, which cannot be true of any record", tc.selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("rewriting %q: %v", tc.selector, err)
			}
			if got := scoped.FieldSelector.String(); got != tc.want {
				t.Errorf("field selector = %q, want %q", got, tc.want)
			}
			// The label narrowing must survive the rewrite.
			if scoped.LabelSelector == nil || scoped.LabelSelector.Empty() {
				t.Error("the record label selector was lost")
			}
		})
	}
}

// TestTheProjectionDeclaresWhatItInjects guards the DECLARATION, not the
// mechanism.
//
// ⚠️ forwardApply honouring tenantProxy.injectedPaths is guarded in apply_test.go,
// but that test builds the proxy by hand and sets the field itself. Deleting the
// one production line that sets it -- crbprojection.go's
// `lister.injectedPaths = projectionLabelPath()` -- left every package green
// while the defect it fixes came all the way back: a server-side-applied
// ClusterRoleBinding created with no projection label, invisible to the
// tenant's own list and to the controller. One line, no compiler signal, silent
// at runtime. This is the guard on that line.
func TestTheProjectionDeclaresWhatItInjects(t *testing.T) {
	storage, err := NewTenantProxy(apiconfig.StorageConfig{
		Kind:     schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
		Resource: "clusterrolebindings",
		NewFunc:  func() runtime.Object { return &rbac.ClusterRoleBinding{} },
	})
	if err != nil {
		t.Fatalf("building the projection: %v", err)
	}
	projection, ok := storage.(*clusterRoleBindingProjection)
	if !ok {
		t.Fatalf("expected the ClusterRoleBinding projection, got %T", storage)
	}

	declared := projection.inner.injectedPaths
	if declared == nil || declared.Empty() {
		t.Fatal("the projection injects its label above the inner storage and does not declare it; " +
			"a forwarded server-side apply will create the record unlabelled, which makes it " +
			"invisible to the tenant's own list and to every controller sync")
	}
	if !declared.Equals(projectionLabelPath()) {
		t.Errorf("the projection declares %v, not the label it actually injects", declared)
	}
}

// crbCountFake stands in for the inner RoleBinding proxy's List.
type crbCountFake struct {
	*tenantProxyWithLister
	count  int
	err    error
	listed bool
}

func (f *crbCountFake) List(context.Context, *metainternalversion.ListOptions) (runtime.Object, error) {
	f.listed = true
	if f.err != nil {
		return nil, f.err
	}
	list := &rbac.RoleBindingList{}
	for i := 0; i < f.count; i++ {
		list.Items = append(list.Items, rbac.RoleBinding{})
	}
	return list, nil
}

// TestRefuseTooManyClusterRoleBindings caps the second multiplier.
//
// ⚠️ A tenant's ClusterRoleBinding is stored as one RoleBinding in EVERY
// namespace it owns, so the object count is bindings times namespaces -- and the
// namespace cap alone leaves this dimension unbounded. RBAC authorization walks
// every binding in a namespace, so a large count slows down every authorization
// decision made there, not only the tenant's own.
func TestRefuseTooManyClusterRoleBindings(t *testing.T) {
	withCount := func(max, owned int) (*clusterRoleBindingProjection, *crbCountFake) {
		fake := &crbCountFake{count: owned}
		return &clusterRoleBindingProjection{maxBindings: max, counter: fake.List}, fake
	}

	proxy, _ := withCount(3, 2)
	if err := proxy.refuseTooManyBindings(context.TODO()); err != nil {
		t.Errorf("a tenant below the limit was refused: %v", err)
	}

	proxy, _ = withCount(3, 3)
	err := proxy.refuseTooManyBindings(context.TODO())
	if err == nil {
		t.Fatal("a tenant at the limit was allowed another binding")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("refused with %T, want a Forbidden", err)
	}
	if !strings.Contains(err.Error(), "times") {
		t.Errorf("the refusal does not explain the multiplier: %v", err)
	}

	// ⭐ Zero is no cap, and must not even count -- the count is a LIST.
	//
	// ⚠️ Asserted by whether the counter RAN, not by making it fail: this guard
	// fails open on a count error, so an erroring counter and one that was never
	// called are indistinguishable. That mistake made two assertions vacuous in
	// the namespace cap before a mutation check caught it.
	proxy, fake := withCount(0, 99)
	if err := proxy.refuseTooManyBindings(context.TODO()); err != nil {
		t.Errorf("no cap configured, yet: %v", err)
	}
	if fake.listed {
		t.Error("with no cap configured the bindings were counted anyway")
	}
}

// TestClusterRoleBindingCountFailureDoesNotRefuse -- an upstream blip must not
// look like a quota to a tenant, which cannot tell the two apart.
func TestClusterRoleBindingCountFailureDoesNotRefuse(t *testing.T) {
	fake := &crbCountFake{err: fmt.Errorf("upstream is having a moment")}
	proxy := &clusterRoleBindingProjection{maxBindings: 1, counter: fake.List}
	if err := proxy.refuseTooManyBindings(context.TODO()); err != nil {
		t.Errorf("a failed count refused the binding: %v", err)
	}
	if !fake.listed {
		t.Error("the counter was never called, so this proves nothing about failure")
	}
}
