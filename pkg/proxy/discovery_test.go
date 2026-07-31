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
