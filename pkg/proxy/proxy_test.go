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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"

	"github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/util"

	"github.com/stretchr/testify/assert"

	appsapiv1 "k8s.io/api/apps/v1"
	coreapiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/printers"
	printersinternal "k8s.io/kubernetes/pkg/printers/internalversion"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"
)

// TestNewTenantProxy tests the NewTenantProxy method.
func TestNewTenantProxy(t *testing.T) {
	invalidConfig := apiconfig.StorageConfig{}
	_, err := NewTenantProxy(invalidConfig)
	assert.Error(t, err)

	config := apiconfig.StorageConfig{
		NewFunc: func() runtime.Object {
			return nil
		},
	}
	_, err = NewTenantProxy(config)
	assert.NoError(t, err)
}

type fakeConvertor struct{}

func (f *fakeConvertor) ConvertTenantObjectToUpstreamObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if isNamespaceScoped && accessor.GetNamespace() != "" {
		prefixed := util.AddTenantIDPrefix(tenantID, accessor.GetNamespace())
		accessor.SetNamespace(prefixed)
	} else if !isNamespaceScoped && accessor.GetName() != "" {
		prefixed := util.AddTenantIDPrefix(tenantID, accessor.GetName())
		accessor.SetName(prefixed)
	}
	return nil
}

func (f *fakeConvertor) ConvertUpstreamObjectToTenantObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if isNamespaceScoped {
		namespace := accessor.GetNamespace()
		trimmed := util.TrimTenantIDPrefix(tenantID, namespace)
		accessor.SetNamespace(trimmed)
	} else {
		name := accessor.GetName()
		trimmed := util.TrimTenantIDPrefix(tenantID, name)
		accessor.SetName(trimmed)
	}
	return nil
}

func tenantContext(tenantID string, requestInfo *request.RequestInfo) context.Context {
	userInfo := util.AddTenantIDToUserInfo(tenantID, &user.DefaultInfo{})
	ctx := request.WithUser(context.Background(), userInfo)
	return request.WithRequestInfo(ctx, requestInfo)
}

// TestTenantProxy_Get tests the Get method for TenantProxy.
func TestTenantProxy_Get(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName := "foo"
	upstreamDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName,
		},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", upstreamNamespace, deploymentName) {
			deployment, err := json.Marshal(upstreamDeployment)
			assert.NoError(t, err)
			w.Write(deployment)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	getter, ok := proxy.(rest.Getter)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Getter")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "get",
		Namespace: tenantNamespace,
	})
	obj, err := getter.Get(ctx, deploymentName, &metav1.GetOptions{})
	assert.NoError(t, err)
	accessor, err := meta.Accessor(obj)
	assert.NoError(t, err)
	assert.Equal(t, tenantNamespace, accessor.GetNamespace())
}

func TestTenantProxyWithListerList(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName1 := "foo1"
	deploymentName2 := "foo2"
	upstreamDeployment1 := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName1,
		},
	}
	upstreamDeployment2 := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName2,
		},
	}
	upstreamDeploymentList := appsapiv1.DeploymentList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "List",
		},
		ListMeta: metav1.ListMeta{},
		Items:    []appsapiv1.Deployment{upstreamDeployment1, upstreamDeployment2},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", upstreamNamespace) {
			deployment, err := json.Marshal(upstreamDeploymentList)
			assert.NoError(t, err)
			w.Write(deployment)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	lister, ok := proxy.(rest.Lister)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Lister")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "list",
		Namespace: tenantNamespace,
	})
	obj, err := lister.List(ctx, &metainternalversion.ListOptions{})
	assert.NoError(t, err)
	deploymentList := obj.(*apps.DeploymentList)
	for _, d := range deploymentList.Items {
		assert.Equal(t, tenantNamespace, d.Namespace)
	}
}

func TestTenantProxyCreate(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName := "foo"
	tenantDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantNamespace,
			Name:      deploymentName,
		},
	}
	upstreamDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName,
		},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", upstreamNamespace) {
			deployment, err := json.Marshal(upstreamDeployment)
			assert.NoError(t, err)
			w.Write(deployment)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	creater, ok := proxy.(rest.Creater)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Creater")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "create",
		Namespace: tenantNamespace,
	})
	fakeFunc := func(ctx context.Context, obj runtime.Object) error {
		return nil
	}
	obj, err := creater.Create(ctx, &tenantDeployment, fakeFunc, &metav1.CreateOptions{})
	assert.NoError(t, err)
	accessor, err := meta.Accessor(obj)
	assert.NoError(t, err)
	assert.Equal(t, tenantNamespace, accessor.GetNamespace())
}

func TestTenantProxyUpdate(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName := "foo"
	tenantDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantNamespace,
			Name:      deploymentName,
		},
	}
	upstreamDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName,
		},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", upstreamNamespace, deploymentName) {
			deployment, err := json.Marshal(upstreamDeployment)
			assert.NoError(t, err)
			w.Write(deployment)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	updater, ok := proxy.(rest.Updater)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Updater")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "update",
		Namespace: tenantNamespace,
	})
	fakeValidateObjectFunc := func(ctx context.Context, obj runtime.Object) error {
		return nil
	}
	fakeValidateObjectUpdateFunc := func(ctx context.Context, obj, old runtime.Object) error {
		return nil
	}

	obj, created, err := updater.Update(ctx, deploymentName, rest.DefaultUpdatedObjectInfo(&tenantDeployment), fakeValidateObjectFunc, fakeValidateObjectUpdateFunc, false, &metav1.UpdateOptions{})
	assert.NoError(t, err)
	assert.Equal(t, false, created)
	accessor, err := meta.Accessor(obj)
	assert.NoError(t, err)
	assert.Equal(t, tenantNamespace, accessor.GetNamespace())
}

func TestTenantProxyDelete(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName := "foo"
	upstreamDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName,
		},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", upstreamNamespace, deploymentName) {
			deployment, err := json.Marshal(upstreamDeployment)
			assert.NoError(t, err)
			w.Write(deployment)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	deleter, ok := proxy.(rest.GracefulDeleter)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Getter")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "delete",
		Namespace: tenantNamespace,
	})

	fakeFunc := func(ctx context.Context, obj runtime.Object) error {
		return nil
	}

	obj, deleted, err := deleter.Delete(ctx, deploymentName, fakeFunc, &metav1.DeleteOptions{})
	assert.NoError(t, err)
	accessor, err := meta.Accessor(obj)
	assert.NoError(t, err)
	assert.Equal(t, deleted, true)
	assert.Equal(t, tenantNamespace, accessor.GetNamespace())
}

func TestTenantProxyDeleteCollection(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName1 := "foo1"
	deploymentName2 := "foo2"
	upstreamDeployment1 := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName1,
		},
	}

	upstreamDeployment2 := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName2,
		},
	}

	upstreamDeploymentList := appsapiv1.DeploymentList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "List",
		},
		ListMeta: metav1.ListMeta{},
		Items:    []appsapiv1.Deployment{upstreamDeployment1, upstreamDeployment2},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", upstreamNamespace, deploymentName1) {
			deployment, err := json.Marshal(upstreamDeployment1)
			assert.NoError(t, err)
			w.Write(deployment)
		} else if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", upstreamNamespace, deploymentName2) {
			deployment, err := json.Marshal(upstreamDeployment2)
			assert.NoError(t, err)
			w.Write(deployment)
		} else if r.URL.Path == fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", upstreamNamespace) {
			deploymentList, err := json.Marshal(upstreamDeploymentList)
			assert.NoError(t, err)
			w.Write(deploymentList)
		} else {
			t.Errorf("unexpected url: %v", r.URL.Path)
		}
	}))
	defer fakeUpstream.Close()
	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL})
	config := apiconfig.StorageConfig{
		Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		Resource:        "deployments",
		ShortNames:      []string{"deploy"},
		NamespaceScoped: true,
		NewFunc:         func() runtime.Object { return &apps.Deployment{} },
		NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
		DynamicClient:   client,
		Convertor:       &fakeConvertor{},
	}
	proxy, err := NewTenantProxy(config)
	assert.NoError(t, err)
	collectionDeleter, ok := proxy.(rest.CollectionDeleter)
	if !ok {
		t.Errorf("tenant proxy should implement rest.Getter")
	}

	ctx := tenantContext(tenantID, &request.RequestInfo{
		Verb:      "delete",
		Namespace: tenantNamespace,
	})

	fakeFunc := func(ctx context.Context, obj runtime.Object) error {
		return nil
	}

	listOptions := metainternalversion.ListOptions{
		LabelSelector: labels.Everything(),
	}

	obj, err := collectionDeleter.DeleteCollection(ctx, fakeFunc, &metav1.DeleteOptions{}, &listOptions)

	assert.NoError(t, err)
	deploymentList := obj.(*apps.DeploymentList)
	for _, d := range deploymentList.Items {
		assert.Equal(t, tenantNamespace, d.Namespace)
	}
}

func TestTenantProxyWatch(t *testing.T) {
	tenantID := "test01"
	tenantNamespace := "default"
	upstreamNamespace := util.AddTenantIDPrefix(tenantID, tenantNamespace)
	deploymentName := "foo1"

	upstreamDeployment := appsapiv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: upstreamNamespace,
			Name:      deploymentName,
		},
	}

	client := dynamic.NewForConfigOrDie(&restclient.Config{Host: ""})
	tc := printerstorage.TableConvertor{TableGenerator: printers.NewTableGenerator().With(printersinternal.AddHandlers)}

	proxy := &tenantProxy{
		kind:             appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
		namespaceScoped:  true,
		isCustomResource: false,
		resource:         "deployments",
		shortNames:       []string{"deploy"},
		newFunc:          func() runtime.Object { return &apps.Deployment{} },
		newListFunc:      func() runtime.Object { return &apps.DeploymentList{} },
		dynamicClient:    client,
		convertor:        &fakeConvertor{},
		tableConvertor:   tc,
	}

	fakeWatcher := watch.NewFake()
	w, err := newProxyWatch(fakeWatcher, proxy, tenantID, "")
	if err != nil {
		t.Errorf("failed to new proxy watch")
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&upstreamDeployment)
	if err != nil {
		t.Errorf("failed to convert to unstructured")
	}
	un := &unstructured.Unstructured{Object: obj}

	fakeWatcher.Add(un)
	event := <-w.ResultChan()
	if event.Type != watch.Added {
		t.Errorf("unexpected event type.")
	}

	accessor, err := meta.Accessor(event.Object)
	assert.NoError(t, err)
	assert.Equal(t, tenantNamespace, accessor.GetNamespace())
}

// TestTenantProxyCreateSubresourceAddressesTheParent covers the two ways the
// parent's name can be got wrong on a subresource create.
//
// It used to be read out of the request body, which works only for the
// subresources whose body is the parent or carries its name by convention.
// Eviction and Binding do; TokenRequest does not, and `kubectl create token`
// failed with "name is required". Taking it from the request path fixes that but
// loses something the body gave for free: the body had already been converted,
// so a cluster-scoped parent arrived prefixed. The path has the tenant's
// spelling, so the conversion has to be applied explicitly.
func TestTenantProxyCreateSubresourceAddressesTheParent(t *testing.T) {
	tenantID := "test01"

	for _, tc := range []struct {
		name            string
		namespaceScoped bool
		parentName      string
		wantPath        string
	}{
		{
			// The body of a token request carries no name at all.
			name:            "namespaced parent keeps the tenant's name",
			namespaceScoped: true,
			parentName:      "robot",
			wantPath:        "/api/v1/namespaces/test01-default/serviceaccounts/robot/token",
		},
		{
			// Cluster-scoped names are prefixed, exactly as Get does it.
			name:            "cluster-scoped parent is prefixed",
			namespaceScoped: false,
			parentName:      "volume",
			wantPath:        "/api/v1/persistentvolumes/test01-volume/status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Write([]byte(`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{}}`))
			}))
			defer fakeUpstream.Close()

			resource, subresource := "serviceaccounts", "token"
			if !tc.namespaceScoped {
				resource, subresource = "persistentvolumes", "status"
			}
			config := apiconfig.StorageConfig{
				Kind:            coreapiv1.SchemeGroupVersion.WithKind("ServiceAccount"),
				Resource:        resource,
				Subresource:     subresource,
				NamespaceScoped: tc.namespaceScoped,
				NewFunc:         func() runtime.Object { return &core.ServiceAccount{} },
				DynamicClient:   dynamic.NewForConfigOrDie(&restclient.Config{Host: fakeUpstream.URL}),
				Convertor:       &fakeConvertor{},
			}
			proxy, err := NewTenantProxy(config)
			assert.NoError(t, err)

			requestInfo := &request.RequestInfo{Verb: "create", Name: tc.parentName}
			if tc.namespaceScoped {
				requestInfo.Namespace = "default"
			}
			ctx := tenantContext(tenantID, requestInfo)
			_, err = proxy.(rest.Creater).Create(ctx, &core.ServiceAccount{},
				func(context.Context, runtime.Object) error { return nil }, &metav1.CreateOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tc.wantPath, gotPath,
				"the subresource request went to the wrong object")
		})
	}
}

// TestEveryWritePathRunsTheSameGuards pins that the three write paths refuse the
// same things.
//
// ⛔ The guard list is written out three times -- Create, Update for a PUT, and
// guaranteedUpdate for a PATCH -- and nothing makes the copies agree. Leaving a
// guard out of one of them compiles, passes every unit test that calls the guard
// directly, and leaves an escape reachable by writing twice: create the object
// without the field, then patch it in.
//
// ⭐ That is not hypothetical. refuseNodePorts was added to Create and to Update
// and missed in guaranteedUpdate, which meant `kubectl patch` converted a
// Service to NodePort with nothing in the way. Only the lab's update assertion
// caught it -- reading the code had produced the confident and wrong conclusion
// that Update was the PATCH path.
//
// Compares the source rather than behaviour on purpose: behaviour needs a live
// proxy per guard, and what goes wrong here is an omission, which is a property
// of the text.
func TestEveryWritePathRunsTheSameGuards(t *testing.T) {
	// ⛔ THE WHOLE PACKAGE, not proxy.go. This read only proxy.go, and four
	// guards had since been written into files of their own --
	// refuseUnpublishedDeviceClass, refusePreProvisionedSnapshot,
	// refuseUnpublishedSnapshotClass, refuseInlineCSIVolume. None of them was
	// checked, and the createOnly reasons written for two of them were dead
	// entries nobody was reading. The test reported success about guards it could
	// not see, which is the failure mode it exists to prevent, turned on itself.
	fset := token.NewFileSet()
	var files []*ast.File
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		files = append(files, f)
	}
	var decls []ast.Decl
	for _, f := range files {
		decls = append(decls, f.Decls...)
	}
	file := &ast.File{Name: files[0].Name, Decls: decls}

	// Every method whose name says it refuses something is a guard, so a guard
	// added later is covered without anyone remembering to list it here.
	guards := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "refuse") {
			continue
		}
		guards[fn.Name.Name] = true
	}
	if len(guards) < 10 {
		t.Fatalf("found %d guards, expected the several that exist; the detection is wrong", len(guards))
	}

	// ⚠️ Direct calls are not enough, and a version of this test that stopped
	// there reported two guards missing that were not: refuseProjectedName and
	// refuseProjectedLabel are called from tp.update, which BOTH update paths end
	// in. A test that cries wolf gets an allowlist entry written for it and stops
	// meaning anything, so follow the calls one method to the next.
	directCalls := map[string]map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		directCalls[fn.Name.Name] = map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				directCalls[fn.Name.Name][sel.Sel.Name] = true
			}
			return true
		})
	}
	// ⚠️ Helpers are followed, other ENTRY POINTS are not, and that distinction is
	// the whole difficulty. guaranteedUpdate has a branch that delegates to
	// Create when the object turns out not to exist, so a plain transitive walk
	// credits guaranteedUpdate with every guard Create runs -- and the version of
	// this test that did that passed happily with the guard deleted from
	// guaranteedUpdate. Following nothing produces the opposite error. Follow
	// helpers only.
	entryPoint := map[string]bool{"Create": true, "Update": true, "guaranteedUpdate": true}
	var reachable func(from string, seen map[string]bool) map[string]bool
	reachable = func(from string, seen map[string]bool) map[string]bool {
		out := map[string]bool{}
		if seen[from] {
			return out
		}
		seen[from] = true
		for callee := range directCalls[from] {
			if guards[callee] {
				out[callee] = true
				continue
			}
			// Only methods defined in this file are followed; anything else is a
			// leaf as far as this test is concerned.
			if _, isLocal := directCalls[callee]; isLocal && !entryPoint[callee] {
				for g := range reachable(callee, seen) {
					out[g] = true
				}
			}
		}
		return out
	}
	called := map[string]map[string]bool{}
	for _, path := range []string{"Create", "Update", "guaranteedUpdate"} {
		if _, ok := directCalls[path]; !ok {
			continue
		}
		called[path] = reachable(path, map[string]bool{})
	}
	for _, path := range []string{"Create", "Update", "guaranteedUpdate"} {
		if called[path] == nil {
			t.Fatalf("%s not found in proxy.go; this test no longer knows where the write paths are", path)
		}
	}

	// ⭐ Create-only is a legitimate answer for some rules, so the test asks for a
	// REASON rather than for symmetry. Adding a guard therefore forces the
	// decision to be made and written down; it does not force the guard onto
	// paths where it would do harm.
	createOnly := map[string]string{
		"refuseTenantChosenNode": "placement runs on CREATE and never again: nodeSelector and " +
			"schedulerName are immutable on a pod update and every existing toleration must " +
			"survive one, so re-running it would make a tenant's own running pods unwritable",
		"refuseReservedName": "a name cannot change after creation",
		"refuseUnpublishedStorageClass": "a retired class refuses NEW references only -- existing " +
			"claims have to stay writable by their owner, which is the whole point of retiring " +
			"rather than deleting",
		"refuseUnpublishedSnapshotClass": "same as the storage class: a retired class refuses NEW " +
			"snapshots only, and spec.volumeSnapshotClassName is immutable, so refusing on update " +
			"would fail every later write to a snapshot the tenant cannot repair",
		"refusePreProvisionedSnapshot": "spec.source is immutable upstream, so a tenant cannot " +
			"repair a snapshot it already has; refusing on update would fail every later write -- " +
			"a label, a finalizer -- to an object that is not going to change",
		"refuseTooManyNamespaces": "a ceiling on how many exist, and only a create adds one",
		"refuseTooManyCRDs": "same: a ceiling on how many exist. A tenant over the limit must " +
			"still be able to write and delete the CRDs it has, since deleting one is its only " +
			"way back under",
	}

	// A guard that appears on ANY write path has to appear on all of them, unless
	// it is create-only on purpose. Guards nothing calls are a separate problem
	// and not this test's.
	for name := range guards {
		var missing []string
		var present bool
		for _, path := range []string{"Create", "Update", "guaranteedUpdate"} {
			if called[path][name] {
				present = true
			} else {
				missing = append(missing, path)
			}
		}
		if !present {
			continue
		}
		if _, deliberate := createOnly[name]; deliberate {
			// Still checked: a guard excused from the update paths had better be
			// on the create path, or it runs nowhere at all.
			if !called["Create"][name] {
				t.Errorf("%s is listed as create-only but does not run on Create, so it runs on "+
					"no write path at all", name)
			}
			continue
		}
		if len(missing) > 0 {
			t.Errorf("%s runs on some write paths but not %v -- a tenant reaches the unguarded one "+
				"by writing twice: create the object without the field, then patch it in. If that "+
				"is deliberate, say why in createOnly above", name, missing)
		}
	}
}
