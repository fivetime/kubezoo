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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	restclient "k8s.io/client-go/rest"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// TestNewDiscoveryProxy test some cases of NewDiscoveryProxy.
func TestNewDiscoveryProxy(t *testing.T) {
	client := discovery.NewDiscoveryClientForConfigOrDie(&restclient.Config{})
	crdLister := &util.FakeCRDLister{}
	served := map[string]bool{"": true, "apps": true}
	_, err := NewDiscoveryProxy(nil, crdLister, served)
	assert.Error(t, err)
	_, err = NewDiscoveryProxy(client, nil, served)
	assert.Error(t, err)
	_, err = NewDiscoveryProxy(client, crdLister, served)
	assert.NoError(t, err)
	// An empty served set would advertise nothing at all, which is worse than
	// advertising too much; it has to be an error rather than a quiet blank.
	_, err = NewDiscoveryProxy(client, crdLister, nil)
	assert.Error(t, err)
}

// TestDiscoveryProxy_ServerGroups test some cases of ServerGroups.
func TestDiscoveryProxy_ServerGroups(t *testing.T) {
	tenantID := "demo01"
	upstreamAPIGroupList := &metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroupList",
		},
		Groups: []metav1.APIGroup{
			{
				Name: "extensions",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "extensions/v1beta1",
						Version:      "v1beta1",
					},
				},
			},
			{
				Name: util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: util.AddTenantIDPrefix(tenantID, "kubezoo.io/v1beta1"),
						Version:      "v1beta1",
					},
				},
			},
		},
	}
	tenantAPIGroupList := &metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "APIGroupList",
		},
		Groups: []metav1.APIGroup{
			{
				Name: "extensions",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "extensions/v1beta1",
						Version:      "v1beta1",
					},
				},
			},
			{
				Name: "kubezoo.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{
						GroupVersion: "kubezoo.io/v1beta1",
						Version:      "v1beta1",
					},
				},
			},
		},
	}
	tenantCRDs := []*v1.CustomResourceDefinition{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foos." + tenantID + "-kubezoo.io",
			},
			Spec: v1.CustomResourceDefinitionSpec{
				Group: util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
				Names: v1.CustomResourceDefinitionNames{
					Plural: "foos",
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis" {
			groupList, err := json.Marshal(upstreamAPIGroupList)
			assert.NoError(t, err)
			w.Write(groupList)
		} else if r.URL.Path == "/api" {
			groupList, err := json.Marshal(&metav1.APIGroupList{})
			assert.NoError(t, err)
			w.Write(groupList)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer ts.Close()
	client := discovery.NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: ts.URL})
	crdLister := &util.FakeCRDLister{tenantCRDs}
	// A native group is advertised when this build serves it and dropped when it
	// does not. Asking the scheme instead advertises every group Kubernetes has,
	// and a tenant then finds resources in api-resources that error on every
	// call -- certificatesigningrequests did exactly that.
	proxy, err := NewDiscoveryProxy(client, crdLister, map[string]bool{"extensions": true})
	assert.NoError(t, err)
	actual, err := proxy.ServerGroups(tenantID)
	assert.NoError(t, err)
	assert.Equal(t, tenantAPIGroupList, actual)

	// The same upstream list, with extensions no longer served: the tenant's own
	// CRD group survives, the unserved native group does not.
	proxy, err = NewDiscoveryProxy(client, crdLister, map[string]bool{"apps": true})
	assert.NoError(t, err)
	actual, err = proxy.ServerGroups(tenantID)
	assert.NoError(t, err)
	assert.Len(t, actual.Groups, 1, "an unserved native group is still being advertised")
	assert.Equal(t, "kubezoo.io", actual.Groups[0].Name)
}

// TestDiscoveryProxy_ServerVersionsForGroup test some cases of ServerVersionsForGroup.
func TestDiscoveryProxy_ServerVersionsForGroup(t *testing.T) {
	tenantID := "demo01"
	upstreamAPIGroup := &metav1.APIGroup{
		Name: util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
		Versions: []metav1.GroupVersionForDiscovery{
			{
				GroupVersion: util.AddTenantIDPrefix(tenantID, "kubezoo.io/v1beta1"),
				Version:      "v1beta1",
			},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: util.AddTenantIDPrefix(tenantID, "kubezoo.io/v1beta1"),
			Version:      "v1beta1",
		},
	}
	tenantAPIGroup := &metav1.APIGroup{
		Name: "kubezoo.io",
		Versions: []metav1.GroupVersionForDiscovery{
			{
				GroupVersion: "kubezoo.io/v1beta1",
				Version:      "v1beta1",
			},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: "kubezoo.io/v1beta1",
			Version:      "v1beta1",
		},
	}
	tenantCRDs := []*v1.CustomResourceDefinition{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foos." + tenantID + "-kubezoo.io",
			},
			Spec: v1.CustomResourceDefinitionSpec{
				Group: util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
				Names: v1.CustomResourceDefinitionNames{
					Plural: "foos",
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/demo01-kubezoo.io" {
			group, err := json.Marshal(upstreamAPIGroup)
			assert.NoError(t, err)
			w.Write(group)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer ts.Close()
	client := discovery.NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: ts.URL})
	crdLister := &util.FakeCRDLister{tenantCRDs}
	proxy, err := NewDiscoveryProxy(client, crdLister, map[string]bool{"": true, "apps": true})
	assert.NoError(t, err)
	actual, err := proxy.ServerVersionsForGroup(tenantID, "kubezoo.io")
	assert.NoError(t, err)
	assert.Equal(t, tenantAPIGroup, actual)
}

// TestDiscoveryProxy_ServerResourcesForGroupVersion test some cases of for method ServerResourcesForGroupVersion.
func TestDiscoveryProxy_ServerResourcesForGroupVersion(t *testing.T) {
	tenantID := "demo01"
	upstreamResourceList := &metav1.APIResourceList{
		GroupVersion: util.AddTenantIDPrefix(tenantID, "kubezoo.io/v1beta1"),
		APIResources: []metav1.APIResource{
			{
				Name:    "foo",
				Group:   util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
				Version: "v1beta1",
			},
		},
	}
	tenantResourceList := &metav1.APIResourceList{
		GroupVersion: "kubezoo.io/v1beta1",
		APIResources: []metav1.APIResource{
			{
				Name:    "foo",
				Group:   "kubezoo.io",
				Version: "v1beta1",
			},
		},
	}
	tenantCRDs := []*v1.CustomResourceDefinition{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foos." + tenantID + "-kubezoo.io",
			},
			Spec: v1.CustomResourceDefinitionSpec{
				Group: util.AddTenantIDPrefix(tenantID, "kubezoo.io"),
				Names: v1.CustomResourceDefinitionNames{
					Plural: "foos",
				},
				Versions: []v1.CustomResourceDefinitionVersion{
					{
						Name: "v1beta1",
					},
				},
			},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/demo01-kubezoo.io/v1beta1" {
			resourceList, err := json.Marshal(upstreamResourceList)
			assert.NoError(t, err)
			w.Write(resourceList)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer ts.Close()
	client := discovery.NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: ts.URL})
	crdLister := &util.FakeCRDLister{tenantCRDs}
	proxy, err := NewDiscoveryProxy(client, crdLister, map[string]bool{"": true, "apps": true})
	assert.NoError(t, err)
	actual, err := proxy.ServerResourcesForGroupVersion(tenantID, "kubezoo.io", "v1beta1")
	assert.NoError(t, err)
	assert.Equal(t, tenantResourceList, actual)
}

// TestPerGroupDiscoveryRefusesAnotherTenantsGroup guards the leak that survived
// six rounds and a fix that was never wired.
//
// ⚠️ /apis is filtered, but /apis/{group} and /apis/{group}/{version} are
// addressed directly and used to forward whatever group the client typed --
// using kubezoo's OWN upstream credential, so upstream RBAC did not apply
// either. Tenant A asking for /apis/<B's group>/v1 was handed tenant B's whole
// APIResourceList: every plural, singular, kind and short name of every CRD B
// had installed, plus a 200-vs-404 oracle for guessing the pairs.
//
// A previous round added the refusing helper and never called it. The commit
// message and docs/isolation-audit-cn.md both said it was fixed; an unused
// package-level function compiles, so nothing went red. This test calls the
// endpoints.
func TestPerGroupDiscoveryRefusesAnotherTenantsGroup(t *testing.T) {
	const tenantID = "111111"

	// The tenant owns one CRD group; another tenant owns the one it will ask for.
	tenantCRDs := []*v1.CustomResourceDefinition{{
		ObjectMeta: metav1.ObjectMeta{Name: "foos." + tenantID + "-mine.example.com"},
		Spec: v1.CustomResourceDefinitionSpec{
			Group:    util.AddTenantIDPrefix(tenantID, "mine.example.com"),
			Names:    v1.CustomResourceDefinitionNames{Plural: "foos"},
			Versions: []v1.CustomResourceDefinitionVersion{{Name: "v1", Served: true}},
		},
	}}

	var reached []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.Write([]byte(`{"kind":"APIGroup","apiVersion":"v1","name":"x"}`))
	}))
	defer ts.Close()

	client := discovery.NewDiscoveryClientForConfigOrDie(&restclient.Config{Host: ts.URL})
	proxy, err := NewDiscoveryProxy(client, &util.FakeCRDLister{tenantCRDs},
		map[string]bool{"": true, "apps": true})
	assert.NoError(t, err)

	// Another tenant's CRD group, by both endpoints.
	if _, err := proxy.ServerVersionsForGroup(tenantID, "222222-acme.io"); err == nil {
		t.Error("ServerVersionsForGroup handed a tenant another tenant's API group")
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("refused, but not as NotFound: %v", err)
	}
	if _, err := proxy.ServerResourcesForGroupVersion(tenantID, "222222-acme.io", "v1"); err == nil {
		t.Error("ServerResourcesForGroupVersion handed a tenant another tenant's resource list")
	}
	// A native group this build installs no storage for: /apis drops it, so these
	// must not advertise it either.
	if _, err := proxy.ServerResourcesForGroupVersion(tenantID, "certificates.k8s.io", "v1"); err == nil {
		t.Error("a group this build serves no storage for was advertised")
	}
	if len(reached) != 0 {
		t.Errorf("the refused requests still went upstream: %v", reached)
	}

	// And what the tenant IS entitled to still works, by both spellings of the
	// name it uses.
	if _, err := proxy.ServerVersionsForGroup(tenantID, "mine.example.com"); err != nil {
		t.Errorf("the tenant's own CRD group was refused: %v", err)
	}
	if _, err := proxy.ServerVersionsForGroup(tenantID, "apps"); err != nil {
		t.Errorf("a native served group was refused: %v", err)
	}
}
