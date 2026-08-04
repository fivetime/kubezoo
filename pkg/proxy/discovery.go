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
	"net/url"
	"strings"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	v1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

type DiscoveryProxy interface {
	// ServerGroups returns the supported groups for tenant, with information like supported versions and the
	// preferred version.
	ServerGroups(tenantID string) (*metav1.APIGroupList, error)
	// ServerVersionsForGroup returns the supported versions and the preferred version of a group for tenant.
	ServerVersionsForGroup(tenantID, group string) (*metav1.APIGroup, error)
	// ServerResourcesForGroupVersion returns the supported resources for a group and version for tenant.
	ServerResourcesForGroupVersion(tenantID, group, version string) (*metav1.APIResourceList, error)
	// ServerVersion retrieves and parses the server's version (git version).
	ServerVersion() (*version.Info, error)
	// OpenAPISchema fetches the open api schema using a rest client and parses the proto.
	OpenAPISchema() (*openapi_v2.Document, error)
	// GetSwagger fetches swagger API specification
	GetSwagger() (*spec.Swagger, error)
	// OpenAPIV3 fetches an OpenAPI v3 document from upstream by its path below
	// /openapi/v3 -- the empty string for the group-version index itself, or
	// something like "apis/111111-acme.io/v1" for one group version.
	//
	// It returns raw bytes rather than a parsed document because the caller
	// rewrites names textually and hands the result straight on; parsing and
	// re-serialising it would only risk dropping fields kubezoo does not model.
	OpenAPIV3(path string, query url.Values) ([]byte, error)
}

// discoveryProxy implements the DiscoveryProxy interface
type discoveryProxy struct {
	// discoveryClient discover server-supported API groups,
	// versions and resources from upstream cluster.
	discoveryClient *discovery.DiscoveryClient
	// crdLister helps list CustomResourceDefinitions from upstream cluster.
	crdLister v1.CustomResourceDefinitionLister
	// servedGroups are the API groups this build installs storage for, which is
	// what may be advertised. The scheme knows many more.
	servedGroups map[string]bool
	// sharedResources are the platform's own CRD groups a tenant addresses under
	// their real names, and -- per group -- exactly which resources of them.
	//
	// ⚠️ Advertising a group advertises everything upstream reports in it, which
	// for snapshots would include volumesnapshotcontents: the one resource the
	// design refuses, because its snapshotHandle is another tenant's data. So the
	// resource list is filtered against this map rather than passed through.
	sharedResources map[string]map[string]bool
}

func NewDiscoveryProxy(discoveryClient *discovery.DiscoveryClient,
	crdLister v1.CustomResourceDefinitionLister, servedGroups map[string]bool,
	sharedResources map[string]map[string]bool) (DiscoveryProxy, error) {
	if discoveryClient == nil {
		return nil, fmt.Errorf("discoveryClient is nil")
	}
	if crdLister == nil {
		return nil, fmt.Errorf("crdLister is nil")
	}
	if len(servedGroups) == 0 {
		return nil, fmt.Errorf("servedGroups is empty; discovery would advertise nothing")
	}
	return &discoveryProxy{discoveryClient: discoveryClient, crdLister: crdLister,
		servedGroups: servedGroups, sharedResources: sharedResources}, nil
}

// ServerGroups returns the supported groups for tenant, with information like supported versions and the
// preferred version.
func (dp *discoveryProxy) ServerGroups(tenantID string) (*metav1.APIGroupList, error) {
	crds, err := util.ListCRDsForTenant(tenantID, dp.crdLister)
	if err != nil {
		return nil, err
	}
	grm := util.NewCustomGroupResourcesMap(crds)
	groupList, err := dp.discoveryClient.ServerGroups()
	if err != nil {
		return nil, err
	}
	return filterAPIGroupList(groupList, grm, tenantID, dp.servedGroups, dp.sharedResources), nil
}

// filterAPIGroupList filter the apigroup according to the tenantId prefix.
func filterAPIGroupList(apiGroupList *metav1.APIGroupList, grm util.CustomGroupResourcesMap,
	tenantID string, servedGroups map[string]bool,
	sharedResources map[string]map[string]bool) *metav1.APIGroupList {
	if apiGroupList == nil {
		return nil
	}
	// Set the TypeMeta rather than copying the upstream one. client-go used to
	// hand back the decoded APIGroupList with kind/apiVersion intact, but since
	// the aggregated-discovery rewrite ServerGroups builds a fresh object and
	// leaves TypeMeta empty. Copying that through would strip kind/apiVersion
	// from what tenants see at /apis, while our /apis/{group} and
	// /apis/{group}/{version} responses still carry theirs.
	filtered := &metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
		Groups:   make([]metav1.APIGroup, 0, len(apiGroupList.Groups)),
	}

	for i := range apiGroupList.Groups {
		groupName := apiGroupList.Groups[i].Name
		// exclude the groupVersions exposed at /api
		if groupName == "" {
			continue
		}
		// Native groups this build actually serves. Asking the scheme instead --
		// which is what this did -- advertises every group Kubernetes has,
		// including ones with no storage installed here, and a tenant then finds
		// them in `api-resources` and gets an error from every call.
		if servedGroups[groupName] {
			filtered.Groups = append(filtered.Groups, apiGroupList.Groups[i])
			continue
		}
		// ⭐ A platform CRD group shared with every tenant, under its real name.
		// Not converted: there is no tenant prefix to take off, which is the
		// whole point -- the tenant addresses snapshot.storage.k8s.io, the same
		// string the platform's controller watches.
		if len(sharedResources[groupName]) > 0 {
			filtered.Groups = append(filtered.Groups, apiGroupList.Groups[i])
			continue
		}
		// custom group for tenant
		if grm.HasGroup(groupName) {
			util.ConvertUpstreamApiGroupToTenant(tenantID, &apiGroupList.Groups[i])
			filtered.Groups = append(filtered.Groups, apiGroupList.Groups[i])
			continue
		}
	}
	return filtered
}

// notATenantsGroup refuses a group that is neither one this build serves nor one
// of the tenant's own CRD groups.
//
// ⚠️⚠️ This helper was added once before and NEVER CALLED. The commit message
// said both endpoints now refuse, docs/isolation-audit-cn.md listed the leak as
// fixed, and neither was true of the shipped code: an unused package-level
// function compiles, so build, vet and every test stayed green while the leak
// below was wide open. Three independent audit dimensions found it. If you are
// changing this file, check the call sites, not the helper.
//
// ⚠️ These two endpoints used to fall through to upstream with whatever group the
// client typed, and with kubezoo's own credential rather than the tenant's
// impersonated identity -- so upstream RBAC did not apply either. The group list
// at /apis is filtered (see filterAPIGroupList), but /apis/{group} and
// /apis/{group}/{version} are addressed directly: tenant 111111 asking for
// /apis/222222-acme.io/v1 was handed tenant 222222's whole APIResourceList --
// every plural, singular, kind and short name of every CRD that tenant had
// installed. It also advertised native groups kubezoo installs no storage for,
// so a tenant found resources in api-resources that error on every call.
func notATenantsGroup(group string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: group, Resource: "apigroup"}, group)
}

// ServerVersionsForGroup returns the supported versions and the preferred version of a group for tenant.
func (dp *discoveryProxy) ServerVersionsForGroup(tenantID, group string) (*metav1.APIGroup, error) {
	crds, err := util.ListCRDsForTenant(tenantID, dp.crdLister)
	if err != nil {
		return nil, err
	}
	grm := util.NewCustomGroupResourcesMap(crds)
	customResourceUpstreamGroup := util.AddTenantIDPrefix(tenantID, group)
	switch {
	case grm.HasGroup(customResourceUpstreamGroup):
		group = customResourceUpstreamGroup
	case dp.servedGroups[group]:
		// A native group this build installs storage for.
	case len(dp.sharedResources[group]) > 0:
		// A shared platform CRD group, addressed under its real name.
	default:
		return nil, notATenantsGroup(group)
	}

	g := &metav1.APIGroup{}
	if err := dp.discoveryClient.RESTClient().Get().AbsPath("/apis/" + group).Do(context.TODO()).Into(g); err != nil {
		return nil, err
	}
	util.ConvertUpstreamApiGroupToTenant(tenantID, g)
	return g, nil
}

// ServerResourcesForGroupVersion returns the supported resources for a group and version for tenant.
func (dp *discoveryProxy) ServerResourcesForGroupVersion(tenantID, group, version string) (*metav1.APIResourceList, error) {
	crds, err := util.ListCRDsForTenant(tenantID, dp.crdLister)
	if err != nil {
		return nil, err
	}
	grm := util.NewCustomGroupResourcesMap(crds)
	customResourceUpstreamGroup := util.AddTenantIDPrefix(tenantID, group)
	switch {
	case grm.HasGroupVersion(customResourceUpstreamGroup, version):
		group = customResourceUpstreamGroup
	case dp.servedGroups[group]:
		// A native group this build installs storage for.
	case len(dp.sharedResources[group]) > 0:
		// A shared platform CRD group, addressed under its real name.
	default:
		return nil, notATenantsGroup(group)
	}
	resourceList, err := dp.discoveryClient.ServerResourcesForGroupVersion(group + "/" + version)
	if err != nil {
		return nil, err
	}
	util.ConvertUpstreamResourceListToTenant(tenantID, resourceList)
	// ⛔ A shared group is advertised resource by resource, not wholesale.
	// Upstream reports everything the CRDs define, and for snapshots that
	// includes volumesnapshotcontents -- the one this design refuses, because a
	// tenant able to create one could import another tenant's data by naming its
	// snapshotHandle. Discovery is not authorization, and the write guards refuse
	// it anyway; but advertising a resource a tenant must not use invites the
	// call and makes the refusal look like a bug.
	if allowed := dp.sharedResources[group]; len(allowed) > 0 {
		kept := resourceList.APIResources[:0]
		for _, r := range resourceList.APIResources {
			// A subresource is written parent/sub; it lives or dies with its parent.
			parent, _, _ := strings.Cut(r.Name, "/")
			if allowed[parent] {
				kept = append(kept, r)
			}
		}
		resourceList.APIResources = kept
	}
	return resourceList, nil
}

// ServerVersion retrieves and parses the server's version (git version).
func (dp *discoveryProxy) ServerVersion() (*version.Info, error) {
	return dp.discoveryClient.ServerVersion()
}

func (dp *discoveryProxy) OpenAPISchema() (*openapi_v2.Document, error) {
	return dp.discoveryClient.OpenAPISchema()
}

func (dp *discoveryProxy) OpenAPIV3(path string, query url.Values) ([]byte, error) {
	request := dp.discoveryClient.RESTClient().Get().AbsPath("/openapi/v3", path)
	for name, values := range query {
		for _, value := range values {
			request = request.Param(name, value)
		}
	}
	return request.Do(context.TODO()).Raw()
}

func (dp *discoveryProxy) GetSwagger() (*spec.Swagger, error) {
	data, err := dp.discoveryClient.RESTClient().Get().AbsPath("/openapi/v2").Do(context.TODO()).Raw()
	if err != nil {
		return nil, err
	}
	spec := &spec.Swagger{}
	if err = json.Unmarshal(data, spec); err != nil {
		return nil, err
	}
	return spec, nil
}
