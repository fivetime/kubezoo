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

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"github.com/fivetime/kubezoo-gateway/pkg/convert"

	"github.com/fivetime/kubezoo-gateway/pkg/proxy/pod"
	"github.com/fivetime/kubezoo-gateway/pkg/publishedclass"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/request"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog"

	tenantlister "github.com/fivetime/kubezoo-contract/pkg/generated/listers/tenant/v1alpha1"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/printers"
	printersinternal "k8s.io/kubernetes/pkg/printers/internalversion"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	quotav1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/quota/v1alpha1"
	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// tenantProxyWithLister implements StandardStorage
var _ = rest.StandardStorage(&tenantProxyWithLister{})

// tenantProxy implements the converting between tenant object and upstream object.
type tenantProxy struct {
	// refactored tenant proxy
	kind             schema.GroupVersionKind
	convertor        common.ObjectConvertor
	namespaceScoped  bool
	resource         string
	subresource      string
	shortNames       []string
	isCustomResource bool
	// isSharedGroup reports whether this proxy's group is a platform CRD group
	// shared with tenants under its real name. See upstreamGroupVersion.
	isSharedGroup func(group string) bool
	// hideProjected is set on the RoleBinding endpoint a tenant talks to, and
	// cleared on the inner proxy the ClusterRoleBinding projection drives --
	// which is the same resource and must be able to see and write the records.
	hideProjected bool

	// NewFunc returns a new instance of the type this registry returns for a
	// GET of a single object
	newFunc func() runtime.Object

	// NewListFunc returns a new list of the type this registry; it is the
	// type returned when the resource is listed
	newListFunc func() runtime.Object

	// TableConvertor is an optional interface for transforming items or lists
	// of items into tabular output. If unset, the default will be used.
	tableConvertor rest.TableConvertor

	// typeConverter reads an object against its schema, so that the fields a
	// server-side apply owns can be lifted back out of the converted object.
	typeConverter managedfields.TypeConverter

	// injectedPaths names fields kubezoo adds to an object *above* this storage,
	// which conversionDelta therefore cannot see.
	//
	// ⚠️ conversionDelta works by comparing the object before and after
	// convertTenantObjectToUpstreamObject, so it closes that window and only that
	// window. The ClusterRoleBinding projection sits above it: asRoleBinding
	// stamps the projection label inside objInfo.UpdatedObject, before this
	// storage ever sees the object, so the label is identical on both sides of
	// the comparison and lands in neither Added nor Modified. It is not in the
	// owned set either, because the tenant never wrote it. A server-side apply
	// therefore created the record with no label at all -- and the label is what
	// every consumer selects on, so the tenant's ClusterRoleBindings listed as
	// empty and the controller never derived the cluster-scoped half of them.
	//
	// Anything that injects above a storage has to say so here.
	injectedPaths *fieldpath.Set

	// publishedStorageClasses answers which storage classes the platform offers
	// and which it has retired, so that a create can be refused before it
	// provisions anything. Nil on every resource but persistentvolumeclaims.
	publishedStorageClasses publishedclass.Set
	// publishedSnapshotClasses answers which volume snapshot classes the platform
	// offers. A CR, so it is fed by a dynamic informer rather than a typed one --
	// the Set does not care which.
	publishedSnapshotClasses publishedclass.Set
	// publishedVolumeAttributesClasses is the same for
	// spec.volumeAttributesClassName -- a field the tenant may CHANGE on a bound
	// claim, so its check is not create-only.
	publishedVolumeAttributesClasses publishedclass.Set

	// maxNamespaces caps how many namespaces this tenant may own; zero means no
	// cap. Nil on every resource but namespaces.
	maxNamespaces int
	// maxCRDs caps how many CustomResourceDefinitions this tenant may own; zero
	// means no cap.
	maxCRDs int
	// publishedDeviceClasses is the set of DRA hardware tiers the platform offers
	// this tenant; naming any other is refused.
	publishedDeviceClasses publishedclass.Set
	// tenants reads the Tenant objects, so a per-tenant cap can be raised by
	// editing one object instead of restarting the gateway.
	tenants tenantlister.TenantLister

	// dynamic client is used to communicate with upstream cluster
	dynamicClient dynamic.Interface

	groupVersionKindFunc apiconfig.GroupVersionKindFunc
}

// tenantProxyWithLister is a wrapper of tenantProxy, it exposes Lister interface to enable installation of List method
// it also exposes TableConvertor interface to convert list to table
type tenantProxyWithLister struct {
	tenantProxy
}

func (p *tenantProxyWithLister) NewList() runtime.Object {
	return p.newList()
}

func (p *tenantProxyWithLister) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	return p.list(ctx, options)
}

func (tp *tenantProxyWithLister) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	if tp.tableConvertor == nil {
		return nil, fmt.Errorf("tableConvertor is nil")
	}
	return tp.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}

func (tc *tenantProxy) NamespaceScoped() bool {
	return tc.namespaceScoped
}

func (tc *tenantProxy) ShortNames() []string {
	return tc.shortNames
}

// GetSingularName returns the singular name of the resource. The API installer
// has required this of every non-subresource storage since 1.26, and it shows up
// in discovery as SINGULARNAME.
//
// Lowercasing the kind is what Kubernetes itself arrives at: checked against a
// 1.36 apiserver, all 68 resources that carry a singular name have exactly
// strings.ToLower(kind), with no exceptions.
func (tc *tenantProxy) GetSingularName() string {
	return strings.ToLower(tc.kind.Kind)
}

// NewTenantProxy returns the tenant proxy which implements the storage intefaces.
func NewTenantProxy(config apiconfig.StorageConfig) (rest.Storage, error) {
	if config.IsConnecter {
		return NewConnecterProxy(config.ProxyTransport, config.UpstreamMaster)
	}
	if (config.Resource == "pods" || config.Resource == "services" || config.Resource == "nodes") && config.Subresource == "proxy" {
		return pod.NewProxyREST(config.ProxyTransport, config.UpstreamMaster)
	}
	// A tenant's ClusterRoleBinding is not one upstream. See crbprojection.go.
	if config.Resource == "clusterrolebindings" && config.Subresource == "" &&
		config.Kind.Group == "rbac.authorization.k8s.io" {
		return newClusterRoleBindingProjection(config)
	}
	// Some cluster-scoped resources are the platform's, not the tenant's: they
	// are published read-only under their real names so a tenant can discover
	// what it may reference. See publicclass.go.
	if config.PublishedClasses != nil {
		return NewPublicClassStorage(config, config.PublishedClasses)
	}

	if config.NewFunc == nil && config.NewListFunc == nil {
		return nil, fmt.Errorf("both NewFunc and NewListFunc is nil")
	}
	if config.Subresource != "" && config.NewListFunc != nil {
		return nil, fmt.Errorf("subresource (%s:%s) should not have list method", config.Resource, config.Subresource)
	}

	var tc rest.TableConvertor = printerstorage.TableConvertor{TableGenerator: printers.NewTableGenerator().With(printersinternal.AddHandlers)}
	if config.TableConvertor != nil {
		tc = config.TableConvertor
	}

	proxy := &tenantProxy{
		kind:             config.Kind,
		namespaceScoped:  config.NamespaceScoped,
		isCustomResource: config.IsCustomResource,
		isSharedGroup:    config.IsSharedGroup,
		hideProjected: config.Resource == "rolebindings" && config.Subresource == "" &&
			config.Kind.Group == "rbac.authorization.k8s.io",
		resource:      config.Resource,
		subresource:   config.Subresource,
		shortNames:    config.ShortNames,
		newFunc:       config.NewFunc,
		newListFunc:   config.NewListFunc,
		typeConverter: config.TypeConverter,
		dynamicClient: config.DynamicClient,

		publishedStorageClasses:          config.PublishedStorageClasses,
		publishedSnapshotClasses:         config.PublishedSnapshotClasses,
		publishedVolumeAttributesClasses: config.PublishedVolumeAttributesClasses,
		maxNamespaces:                    config.MaxNamespaces,
		maxCRDs:                          config.MaxCRDs,
		publishedDeviceClasses:           config.PublishedDeviceClasses,
		tenants:                          config.Tenants,
		convertor:                        config.Convertor,
		groupVersionKindFunc:             config.GroupVersionKindFunc,
		tableConvertor:                   tc,
	}
	if config.NewListFunc == nil {
		return proxy, nil
	}
	return &tenantProxyWithLister{*proxy}, nil
}

func (tp *tenantProxy) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	if tp.groupVersionKindFunc == nil {
		return tp.kind
	}
	return tp.groupVersionKindFunc(containingGV)
}

// getClient returns a dynamic client.
func (tp *tenantProxy) getClient(ctx context.Context) (dynamic.ResourceInterface, error) {
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context")
	}
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("missing requestInfo")
	}
	var client dynamic.ResourceInterface
	gv := tp.upstreamGroupVersion(tenantID)
	client = tp.dynamicClient.Resource(gv.WithResource(tp.resource))
	if tp.namespaceScoped && len(requestInfo.Namespace) != 0 {
		namespace := util.UpstreamNamespace(tenantID, requestInfo.Namespace)
		client = tp.dynamicClient.Resource(gv.WithResource(tp.resource)).Namespace(namespace)
	}
	return client, nil
}

// shapeError turns an upstream error into the one the tenant should see.
//
// Beyond trimming the tenant prefix it fixes a mismatch that stops ordinary
// tooling working. Upstream refuses a tenant's request into a namespace that
// does not exist with Forbidden, because a namespace that does not exist has no
// RoleBinding either -- while the same request from a cluster-admin gets
// NotFound, since for them it is only a missing object. Tools read the
// difference as fatal versus routine.
//
// Measured: `helm install --create-namespace` never even attempts to create the
// namespace. It checks whether the chart's resources already exist first, gets
// Forbidden where a cluster-admin would get NotFound, and gives up -- so a
// tenant had to create the namespace by hand before every chart.
//
// NotFound is also the truthful answer from where the tenant stands: no such
// namespace, so no such object in it.
//
// Deliberately narrow. Only a Get, and only once the namespace has been
// confirmed absent, so a genuine permission error on an existing namespace is
// still reported as one. List is left alone: upstream answers an empty list
// rather than NotFound there, and matching that means synthesising a response
// rather than reshaping an error.
func (tp *tenantProxy) shapeError(ctx context.Context, err error, tenantID string) error {
	if !apierrors.IsForbidden(err) || !tp.namespaceScoped {
		return util.TrimTenantIDFromError(err, tenantID)
	}
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	if !ok || requestInfo.Namespace == "" {
		return util.TrimTenantIDFromError(err, tenantID)
	}
	upstreamNamespace := util.UpstreamNamespace(tenantID, requestInfo.Namespace)
	_, nsErr := tp.dynamicClient.Resource(namespaceGVR).Get(ctx, upstreamNamespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(nsErr) {
		return util.TrimTenantIDFromError(err, tenantID)
	}
	return apierrors.NewNotFound(schema.GroupResource{
		Group:    tp.kind.Group,
		Resource: tp.resource,
	}, requestInfo.Name)
}

// Destroy releases resources held by the storage. rest.Storage grew this method
// in Kubernetes 1.26; the proxy holds no resources of its own.
func (tp *tenantProxy) Destroy() {}

// convertUnstructuredToOutput convert the unstructured to runtime object.
func (tp *tenantProxy) convertUnstructuredToOutput(utd *unstructured.Unstructured, output runtime.Object) error {
	if o, ok := output.(*unstructured.Unstructured); ok {
		o.SetUnstructuredContent(utd.UnstructuredContent())
		return nil
	}

	kind := tp.GroupVersionKind(tp.kind.GroupVersion())
	original, err := nativeScheme.New(kind)
	if err != nil {
		return err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(utd.Object, original); err != nil {
		return err
	}
	if err := nativeScheme.Convert(original, output, context.TODO()); err != nil {
		return err
	}
	return nil
}

// convertUnstructuredListToOutput convert a unstructured list to runtime object.
func (tp *tenantProxy) convertUnstructuredListToOutput(utdList *unstructured.UnstructuredList, output runtime.Object) error {
	if o, ok := output.(*unstructured.UnstructuredList); ok {
		o.SetUnstructuredContent(utdList.UnstructuredContent())
		return nil
	}

	origin, err := nativeScheme.New(tp.kind.GroupVersion().WithKind(tp.kind.Kind + "List"))
	if err != nil {
		return err
	}

	js, err := utdList.MarshalJSON()
	if err != nil {
		return err
	}

	if err := json.Unmarshal(js, &origin); err != nil {
		return err
	}
	if err := nativeScheme.Convert(origin, output, context.TODO()); err != nil {
		return err
	}
	return nil
}

// Get finds a resource in the upstream cluster by name and returns it.
// Although it can return an arbitrary error value, IsNotFound(err) is true for the
// returned error value err when the specified resource is not found.
// readForUpdate reads the object an update is about to change.
//
// ⛔ NOT tp.Get, and the difference is a permission real Kubernetes does not
// ask for. On a subresource storage tp.Get reads the SUBRESOURCE path -- `GET
// issuers/foo/status` -- so upstream authorizes `get` on `issuers/status`. An
// operator's RBAC grants `update` and `patch` on the status subresource and
// `get` on the parent, which is what upstream itself requires, and is refused
// here. cert-manager's chart is exactly that shape, and its controller could
// not write a single Issuer status:
//
//	issuers.cert-manager.io "selfsigned" is forbidden: cannot get resource
//	"issuers/status" in API group "cert-manager.io"
//
// ⭐ Reading the parent is equivalent, not a workaround: a GET on the status
// subresource returns the whole object, the same one the parent returns. Only
// the permission asked for differs.
func (tp *tenantProxy) readForUpdate(ctx context.Context, name string) (runtime.Object, error) {
	if tp.subresource == "" {
		return tp.Get(ctx, name, &metav1.GetOptions{})
	}
	parent := *tp
	parent.subresource = ""
	return parent.Get(ctx, name, &metav1.GetOptions{})
}

func (tp *tenantProxy) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	if tp.servesRoleBindings() && util.IsManagedBindingName(name) {
		// Hidden here, so reading one by name has to agree with listing.
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: tp.kind.Group, Resource: tp.resource}, name)
	}
	if tp.newFunc == nil {
		return nil, fmt.Errorf("newFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context")
	}

	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, err
	}
	var utd *unstructured.Unstructured
	if !tp.namespaceScoped {
		name = util.ConvertTenantObjectNameToUpstream(name, tenantID, tp.kind)
	}
	if subResource := tp.subresource; subResource != "" {
		utd, err = client.Get(ctx, name, *options, subResource)
	} else {
		utd, err = client.Get(ctx, name, *options)
	}
	if err != nil {
		return nil, tp.shapeError(ctx, err, tenantID)
	}

	// convert unstructured object to internal for non CRD resources
	output := tp.New()
	if err := tp.convertUnstructuredToOutput(utd, output); err != nil {
		return nil, err
	}
	if err := tp.convertUpstreamObjectToTenantObject(output, tenantID, tp.requestNamespace(ctx)); err != nil {
		return nil, err
	}

	return output, nil
}

// New returns an empty object that can be used with Update after request data has been put into it.
func (tp *tenantProxy) New() runtime.Object {
	if tp.newFunc == nil {
		return nil
	}
	return tp.newFunc()
}

// newList returns an empty object that can be used with the List call.
func (tp *tenantProxy) newList() runtime.Object {
	return tp.newListFunc()
}

// Update finds a resource in the storage and updates it. Some implementations
// may allow updates creates the object - they should set the created boolean
// to true.
func (tp *tenantProxy) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo,
	_ rest.ValidateObjectFunc, _ rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	if tp.newFunc == nil {
		return nil, false, fmt.Errorf("newFunc is nil")
	}

	requestInfo, ok := request.RequestInfoFrom(ctx)
	if !ok {
		return nil, false, fmt.Errorf("missing requestInfo")
	}
	if requestInfo.Verb == "patch" {
		return tp.guaranteedUpdate(ctx, name, objInfo, forceAllowCreate, options)
	}

	original, err := tp.readForUpdate(ctx, name)
	if err != nil && !errors.IsNotFound(err) {
		return nil, false, err
	}
	if errors.IsNotFound(err) {
		if !forceAllowCreate {
			return nil, false, err
		}
		// Get returns nil alongside NotFound, and the transformer is entitled to
		// a real object. The generic registry hands it a zero value of the right
		// type here; so does this.
		original = tp.New()
	}

	obj, err := objInfo.UpdatedObject(ctx, original)
	if err != nil {
		return nil, false, err
	}
	// Checked here rather than in update(), which is the one place on this path
	// that has the stored claim to compare against -- the whole rule is "only
	// when the value changes".
	if err := tp.refuseUnpublishedVolumeAttributesClass(obj, original); err != nil {
		return nil, false, err
	}
	if err := tp.refuseExternalNameService(obj); err != nil {
		return nil, false, err
	}
	if err := tp.refuseForgedEndpointAddress(ctx, obj); err != nil {
		return nil, false, err
	}
	if err := tp.refuseUnpublishedEphemeralClasses(obj, original); err != nil {
		return nil, false, err
	}
	if err := tp.refuseNewExternalIPs(obj, original); err != nil {
		return nil, false, err
	}
	if err := tp.refuseNodePorts(obj, original); err != nil {
		return nil, false, err
	}
	if err := tp.refuseUnpublishedDeviceClass(obj); err != nil {
		return nil, false, err
	}
	if err := tp.refusePlatformQuotaWrite(obj); err != nil {
		return nil, false, err
	}
	return tp.update(ctx, obj, options)
}

// update convert the tenant object to upstream object before updating
// to the upstream server, and then convert the response to tenant object.
func (tp *tenantProxy) update(ctx context.Context, obj runtime.Object, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, false, fmt.Errorf("missing tenantID in context")
	}
	// ⚠️ On the update paths too, and that is the point: a POD's volumes are
	// immutable, but a TEMPLATE's are not, so a tenant refused at create can
	// simply create the workload without the volume and patch it in afterwards.
	// TestEveryWritePathRunsTheSameGuards said exactly that, in those words, the
	// moment it could see this guard at all.
	if err := tp.refuseInlineCSIVolume(obj); err != nil {
		return nil, false, err
	}

	// A forwarded apply has to know which fields conversion introduced, and the
	// only way to know is to keep the object as it stood before. Snapshotted only
	// when the request really is an apply, so ordinary writes pay nothing.
	var tenantForm *unstructured.Unstructured
	if tp.subresource == "" && util.IsApplyPatch(ctx) {
		snapshot, err := tp.convertInternalObjectToUnstructuredObject(obj.DeepCopyObject())
		if err != nil {
			return nil, false, err
		}
		tenantForm = snapshot
	}

	// 1. convert the internal version of tenant object to upstream object
	if err := tp.convertTenantObjectToUpstreamObject(obj, tenantID); err != nil {
		return nil, false, err
	}

	// 2. convert the internal obj to unstructured
	utd, err := tp.convertInternalObjectToUnstructuredObject(obj)
	if err != nil {
		return nil, false, err
	}

	// 3. call update api
	var (
		got     *unstructured.Unstructured
		created bool
	)
	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := tp.refuseReservedName(tenantID, utd.GetName()); err != nil {
		return nil, false, err
	}
	if err := tp.refuseProjectedName(utd.GetName()); err != nil {
		return nil, false, err
	}
	if err := tp.refuseProjectedLabel(utd); err != nil {
		return nil, false, err
	}
	// A server-side apply goes up as an apply, so that upstream records who
	// applied what and the next apply from the same manager converges instead of
	// conflicting with this one. Everything above has already run, so the object
	// is fully converted; forwardApply only decides which verb carries it.
	if tp.subresource == "" && options != nil {
		applied, err := tp.forwardApply(ctx, utd, tenantForm, options.FieldManager, options)
		if err != nil {
			return nil, false, util.TrimTenantIDFromError(err, tenantID)
		}
		if applied != nil {
			return tp.finishWrite(applied, tenantID, tp.requestNamespace(ctx), false)
		}
	}
	if subresource := tp.subresource; subresource == "" {
		got, created, err = client.Update(ctx, utd, *options)
	} else if subresource == "status" {
		got, err = client.UpdateStatus(ctx, utd, *options)
	} else {
		got, created, err = client.Update(ctx, utd, *options, subresource)
	}
	if err != nil {
		return nil, false, util.TrimTenantIDFromError(err, tenantID)
	}

	// 4. convert got to output
	output := tp.New()
	if err := tp.convertUnstructuredToOutput(got, output); err != nil {
		return nil, false, err
	}

	// 5. convert got to tenant
	if err := tp.convertUpstreamObjectToTenantObject(output, tenantID, tp.requestNamespace(ctx)); err != nil {
		return nil, false, err
	}

	return output, created, nil
}

func (tp *tenantProxy) convertInternalObjectToVersionedObject(obj runtime.Object) (runtime.Object, error) {
	kind := tp.GroupVersionKind(tp.kind.GroupVersion())
	return runtime.UnsafeObjectConvertor(nativeScheme).ConvertToVersion(obj, kind.GroupVersion())
}

func (tp *tenantProxy) convertInternalObjectToUnstructuredObject(obj runtime.Object) (*unstructured.Unstructured, error) {
	if utd, ok := obj.(*unstructured.Unstructured); ok {
		// for custom resource, the internal object is already unstructured, so skip converting
		return utd, nil
	}
	versioned, err := tp.convertInternalObjectToVersionedObject(obj)
	if err != nil {
		return nil, err
	}
	utdObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(versioned)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: utdObj}, nil
}

// Create creates a new version of a resource.
func (tp *tenantProxy) Create(ctx context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	if tp.newFunc == nil {
		return nil, fmt.Errorf("newFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context")
	}

	// See update(): what conversion introduces is not owned by anybody, so the
	// pre-conversion object is what tells a forwarded apply to carry it.
	var tenantForm *unstructured.Unstructured
	if tp.subresource == "" && util.IsApplyPatch(ctx) {
		snapshot, err := tp.convertInternalObjectToUnstructuredObject(obj.DeepCopyObject())
		if err != nil {
			return nil, err
		}
		tenantForm = snapshot
	}

	// 1. convert the internal version of tenant object to upstream object
	if err := tp.convertTenantObjectToUpstreamObject(obj, tenantID); err != nil {
		return nil, err
	}

	// ⭐ Placement, for a live Pod only, and only here. A template is placed by
	// the convertor, which needs no verb; a Pod cannot be, because its placement
	// is immutable once stored -- rewriting it on an update makes upstream refuse
	// the write and leaves the tenant unable to touch its own running pod.
	//
	// ⚠️ Placed BEFORE the object is turned into unstructured, so that a
	// server-side apply carries these fields: conversionDelta compares the
	// snapshot taken above against what goes upstream, and anything injected
	// after this line would fall outside that window and be dropped from the
	// apply. See tenantProxy.injectedPaths for what that costs when it happens.
	if pod, ok := obj.(*core.Pod); ok && tp.subresource == "" {
		convert.PlacePod(pod, tenantID)
		// ⭐ And the pod's own name for its namespace, here for the same reason
		// and with a sharper edge: spec.volumes is immutable once the pod is
		// stored, so doing it in the convertor would make every update to a pod
		// that predates this be refused upstream. saprojection.go.
		convert.ProjectPodNamespace(pod, tenantID)
		// ⭐ CREATE only: a pod's spec.volumes is immutable once stored, so this is
		// the only write where an inline CSI volume can appear -- and refusing on
		// an update would strand a pod that predates the check.
		if err := RefusePodInlineCSIVolume(pod); err != nil {
			return nil, err
		}
	}

	// 2. convert the internal obj to unstructured
	utd, err := tp.convertInternalObjectToUnstructuredObject(obj)
	if err != nil {
		return nil, err
	}
	if err := tp.refuseReservedName(tenantID, utd.GetName()); err != nil {
		return nil, err
	}
	if err := tp.refuseProjectedName(utd.GetName()); err != nil {
		return nil, err
	}
	if err := tp.refuseProjectedLabel(utd); err != nil {
		return nil, err
	}
	// Before forwardApply, so that a server-side apply is refused too -- that is
	// the path `kubectl apply --server-side` takes for an object that does not
	// exist yet, and it is the common way PVCs get created.
	// ⭐ Snapshots, beside storage classes and for the same reason: publication is
	// the authorization. And the pre-provisioned refusal beside it, which is the
	// escape the whole snapshot integration is shaped around -- see
	// pkg/proxy/volumesnapshot.go and docs/design-volumesnapshot-cn.md.
	if err := tp.refuseInlineCSIVolume(obj); err != nil {
		return nil, err
	}
	if err := tp.refuseUnpublishedSnapshotClass(obj); err != nil {
		return nil, err
	}
	if err := tp.refusePreProvisionedSnapshot(obj); err != nil {
		return nil, err
	}
	if err := tp.refuseUnpublishedStorageClass(obj); err != nil {
		return nil, err
	}
	// nil: there is no stored claim, so any class named here is newly named.
	if err := tp.refuseUnpublishedVolumeAttributesClass(obj, nil); err != nil {
		return nil, err
	}
	if err := tp.refuseTenantChosenNode(obj); err != nil {
		return nil, err
	}
	if err := tp.refuseTooManyNamespaces(ctx, obj, tenantID); err != nil {
		return nil, err
	}
	if err := tp.refuseTooManyCRDs(ctx, obj, tenantID); err != nil {
		return nil, err
	}
	// nil: nothing is stored, so every address named here is newly claimed.
	if err := tp.refuseExternalNameService(obj); err != nil {
		return nil, err
	}
	if err := tp.refuseForgedEndpointAddress(ctx, obj); err != nil {
		return nil, err
	}
	if err := tp.refuseUnpublishedEphemeralClasses(obj, nil); err != nil {
		return nil, err
	}
	if err := tp.refuseNewExternalIPs(obj, nil); err != nil {
		return nil, err
	}
	// nil for the same reason: on a create there is no port to have kept.
	if err := tp.refuseNodePorts(obj, nil); err != nil {
		return nil, err
	}
	if err := tp.refuseUnpublishedDeviceClass(obj); err != nil {
		return nil, err
	}
	// ⚠️ Needed on the CREATE path specifically, not only on the update paths.
	// The label this refuses is what the quota reconciler finds its own objects
	// by, and a tenant is admin in its own namespaces -- so it can create an
	// ordinary ResourceQuota carrying that label and no ownerReference. The
	// reconciler handles the decoy by stripping the label rather than adopting
	// it, but that is a repair after the fact; this is what keeps it from being
	// written at all.
	if err := tp.refusePlatformQuotaWrite(obj); err != nil {
		return nil, err
	}

	// An apply that has to create the object is still an apply, and this is the
	// path it takes -- most resources refuse to be created by an update. Sending
	// it as a create would have upstream record it as one, and then the tenant's
	// next apply would conflict with its own first one.
	if tp.subresource == "" && options != nil && options.FieldManager != "" {
		applied, applyErr := tp.forwardApply(ctx, utd, tenantForm, options.FieldManager, &metav1.UpdateOptions{
			DryRun: options.DryRun, FieldManager: options.FieldManager,
		})
		if applyErr != nil {
			return nil, util.TrimTenantIDFromError(applyErr, tenantID)
		}
		if applied != nil {
			out, _, err := tp.finishWrite(applied, tenantID, tp.requestNamespace(ctx), true)
			return out, err
		}
	}

	// 3. call create api
	var got *unstructured.Unstructured
	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, err
	}
	if subresource := tp.subresource; subresource == "" {
		got, err = client.Create(ctx, utd, *options)
	} else {
		// The parent object's name comes from the request path. Reading it out
		// of the body only works for the subresources whose body happens to be
		// the parent, or to carry its name by convention; a TokenRequest does
		// neither.
		requestInfo, ok := request.RequestInfoFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("no request info in context, cannot address subresource %q", subresource)
		}
		// The path carries the tenant's name for the parent, so a cluster-scoped
		// parent still needs the prefix -- the same conversion Get does. Taking
		// the name from the body used to supply this for free, because the body
		// had already been converted.
		name := requestInfo.Name
		if !tp.namespaceScoped {
			name = util.ConvertTenantObjectNameToUpstream(name, tenantID, tp.kind)
		}
		got, err = client.CreateSubresource(ctx, name, utd, *options, subresource)
	}
	if err != nil {
		return nil, util.TrimTenantIDFromError(err, tenantID)
	}

	// 4. convert the got(if it is not an unstructured object) to internal
	output := tp.New()
	if err := tp.convertUnstructuredToOutput(got, output); err != nil {
		return nil, err
	}

	// 5. convert the internal object to tenant
	if err := tp.convertUpstreamObjectToTenantObject(output, tenantID, tp.requestNamespace(ctx)); err != nil {
		return nil, err
	}

	return output, nil
}

// Delete convert the tenant object to upstream object before deleting
// to the upstream server, and then convert the response to tenant object.
func (tp *tenantProxy) Delete(ctx context.Context, name string, _ rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if tp.newFunc == nil {
		return nil, false, fmt.Errorf("newFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, false, fmt.Errorf("tanentID doesn't exist in context")
	}

	if !tp.namespaceScoped {
		name = util.ConvertTenantObjectNameToUpstream(name, tenantID, tp.kind)
	}
	if err := tp.refuseProjectedName(name); err != nil {
		return nil, false, err
	}
	if err := tp.refuseReservedName(tenantID, name); err != nil {
		return nil, false, err
	}
	var (
		got     *unstructured.Unstructured
		deleted bool
		err     error
	)
	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, false, err
	}
	if subresource := tp.subresource; subresource == "" {
		got, deleted, err = client.Delete(ctx, name, *options)
	} else {
		got, deleted, err = client.Delete(ctx, name, *options, subresource)
	}
	if err != nil {
		return nil, deleted, util.TrimTenantIDFromError(err, tenantID)
	}

	if got.GetAPIVersion() == "v1" && got.GroupVersionKind().Kind == "Status" {
		// if we get status, probably we are handling cr object
		status := &metav1.Status{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, status); err != nil {
			return nil, deleted, util.TrimTenantIDFromError(err, tenantID)
		}
		ret := util.TrimTenantIDFromStatus(*status, tenantID)
		return &ret, deleted, nil
	}

	output := tp.New()
	if err := tp.convertUnstructuredToOutput(got, output); err != nil {
		return nil, deleted, err
	}

	if err := tp.convertUpstreamObjectToTenantObject(output, tenantID, tp.requestNamespace(ctx)); err != nil {
		return nil, deleted, err
	}

	return output, deleted, nil
}

// list convert the tenant object to upstream object before list from
// the upstream server, and then convert the response to tenant object.
func (tp *tenantProxy) list(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if tp.newListFunc == nil {
		return nil, fmt.Errorf("newListFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context %v", ctx)
	}

	proxyOptions, err := util.ConvertInternalListOptions(ctx, options, tenantID, tp.listOptionScope())
	if err != nil {
		return nil, err
	}
	var utdList *unstructured.UnstructuredList
	if tp.shouldFanOut(ctx) {
		// No namespace in the request, so this is the tenant's whole world.
		// Reading each of its namespaces beats listing the cluster and
		// discarding everyone else's objects -- see docs/design-list-fanout-cn.md.
		utdList, err = tp.listAcrossNamespaces(ctx, proxyOptions, tenantID)
		if err != nil {
			return nil, util.TrimTenantIDFromError(err, tenantID)
		}
	} else {
		client, err := tp.getClient(ctx)
		if err != nil {
			return nil, err
		}
		// A cluster-scoped list filters after fetching, so it reads over the
		// whole cluster's range and upstream's cursor names somebody else's
		// object. See clusterscopedcursor.go.
		if !tp.namespaceScoped && proxyOptions.Continue != "" {
			upstreamToken, err := decodeClusterScopedContinue(proxyOptions.Continue)
			if err != nil {
				return nil, err
			}
			proxyOptions.Continue = upstreamToken
		}
		utdList, err = client.List(ctx, *proxyOptions)
		if err != nil {
			return nil, util.TrimTenantIDFromError(err, tenantID)
		}
		utdList = util.FilterUnstructuredList(utdList, tenantID, tp.namespaceScoped)
		if !tp.namespaceScoped {
			if err := hideUpstreamListCursor(utdList); err != nil {
				return nil, err
			}
		}
	}
	utdList = tp.hideProjections(utdList)

	// convert internal/unstructured list item one by one
	for i := range utdList.Items {
		// convert each item of the unstructured list to internal version for non-CRD resources
		oupObj := tp.New()
		if err := tp.convertUnstructuredToOutput(&utdList.Items[i], oupObj); err != nil {
			return nil, err
		}
		// convert to tenant
		if err := tp.convertUpstreamObjectToTenantObject(oupObj, tenantID, tp.requestNamespace(ctx)); err != nil {
			return nil, err
		}
		// convert it back to unstructured and put it back to the unstructured list
		utd, err := tp.convertInternalObjectToUnstructuredObject(oupObj)
		if err != nil {
			return nil, err
		}
		utdList.Items[i] = *utd
	}

	if tp.isCustomResource {
		utdList.SetAPIVersion(util.TrimTenantIDPrefix(tenantID, utdList.GetAPIVersion()))
	}

	// convert the entire unstructured list to internal version of list for non-CRD resources
	oupList := tp.newList()
	if err := tp.convertUnstructuredListToOutput(utdList, oupList); err != nil {
		return nil, err
	}

	return oupList, nil
}

// servesRoleBindings reports whether this proxy is the RoleBinding endpoint a
// tenant talks to, rather than the inner one the ClusterRoleBinding projection
// drives. They are the same resource, so the difference cannot be read off the
// config.
func (tp *tenantProxy) servesRoleBindings() bool {
	return tp.hideProjected
}

// hideProjections drops the RoleBindings kubezoo keeps in a tenant's namespaces.
//
// The projections carrying ClusterRoleBindings live one per namespace, so
// leaving them visible would put a copy of every ClusterRoleBinding into every
// namespace of `kubectl get rolebindings -A`, and a tenant told they are
// cluster-scoped would find several of each. Their own endpoint is where they
// belong.
//
// The per-namespace admin binding goes too, and that fixes something worse than
// noise: it references a ClusterRole with no tenant prefix, which the backward
// transform refuses, so a single one of them failed the entire list. Measured --
// a tenant could not list RoleBindings in its own namespace at all.
func (tp *tenantProxy) hideProjections(list *unstructured.UnstructuredList) *unstructured.UnstructuredList {
	if list == nil || !tp.servesRoleBindings() {
		return list
	}
	kept := make([]unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		if util.IsManagedBindingName(list.Items[i].GetName()) {
			continue
		}
		kept = append(kept, list.Items[i])
	}
	list.Items = kept
	return list
}

// refuseProjectedName stops a tenant writing one of the RoleBindings kubezoo
// keeps for it.
//
// Namespaced objects carry no tenant prefix in their name -- only their
// namespace does -- so without this a tenant could write a RoleBinding named
// like a projection and take over what its own ClusterRoleBinding grants, in
// one namespace or all of them, or overwrite the binding that grants it its own
// namespace.
func (tp *tenantProxy) refuseProjectedName(name string) error {
	if !tp.servesRoleBindings() || !util.IsManagedBindingName(name) {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Group: tp.kind.Group, Resource: tp.resource}, name,
		fmt.Errorf("this name belongs to a binding kubezoo keeps for your tenant; "+
			"write the ClusterRoleBinding itself, or choose another name"))
}

// refuseProjectedLabel stops a tenant labelling one of its own RoleBindings as a
// projection record.
//
// The label is how the controller finds the record set, and nothing but the name
// was guarded. A tenant that set it on an ordinary binding in an ordinary
// namespace had that binding deleted as an orphan within the second, repeatedly,
// because it is in no record set; one set in the tenant's own kube-system became
// a record, projected into every namespace the tenant owns and feeding the
// derived cluster-scoped grants, without ever passing refuseProjectedName.
//
// Both outcomes are contained to the tenant -- the derived rules are still
// filtered to its own API groups -- so this is a foot-gun rather than an escape.
// Refused rather than rewritten, because it has no legitimate use: the way to
// have a projection is to write the ClusterRoleBinding.
func (tp *tenantProxy) refuseProjectedLabel(obj *unstructured.Unstructured) error {
	if !tp.servesRoleBindings() {
		return nil
	}
	if _, claimed := obj.GetLabels()[common.ProjectedClusterRoleBindingLabelKey]; !claimed {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Group: tp.kind.Group, Resource: tp.resource}, obj.GetName(),
		fmt.Errorf("the label %s belongs to kubezoo; write the ClusterRoleBinding itself",
			common.ProjectedClusterRoleBindingLabelKey))
}

// finishWrite turns what upstream returned into what the tenant should see.
func (tp *tenantProxy) finishWrite(got *unstructured.Unstructured, tenantID, requestNamespace string,
	created bool) (runtime.Object, bool, error) {
	output := tp.New()
	if err := tp.convertUnstructuredToOutput(got, output); err != nil {
		return nil, false, err
	}
	if err := tp.convertUpstreamObjectToTenantObject(output, tenantID, requestNamespace); err != nil {
		return nil, false, err
	}
	return output, created, nil
}

// refuseReservedName stops a tenant addressing the cluster-scoped RBAC objects
// kubezoo manages on its behalf.
//
// Names of cluster-scoped objects carry the tenant prefix, so a tenant asking
// for a ClusterRole called cluster-admin is asking for <tid>-cluster-admin --
// the role the controller creates and binds cluster-wide to that tenant. Today
// that only lets it delete or narrow its own rights, which the controller
// repairs within seconds, but it is the collision that would turn any future
// privilege here into an escape: with escalate granted, overwriting that role
// reached kube-system's and another tenant's secrets. Measured.
//
// Scoped to RBAC. A tenant may perfectly well have a PersistentVolume called
// admin; it is the roles kubezoo binds that must stay its own.
// refuseUnpublishedStorageClass refuses a create that would reference a storage
// class the platform is not offering.
//
// ⚠️ CREATE ONLY, and that is load-bearing rather than an omission. A bound PVC's
// spec.storageClassName is immutable, so a tenant cannot repair its own manifest
// once a class stops being offered. Checking on update would therefore fail every
// later write to a PVC that already names it -- including a GitOps controller
// reapplying a manifest it has not changed, which would turn withdrawing a class
// into a reconcile loop the tenant has no way out of. Every create path funnels
// through tenantProxy.Create: a POST arrives there directly, and a server-side
// apply of an object that does not exist arrives via guaranteedUpdate, which
// hands the missing case to Create rather than sending it as an update.
//
// ⭐ Publication is now authorization, not only discovery. It used to be only
// discovery -- a tenant that learned a platform-internal class name out of band
// could still provision on it -- and closing that is the point of this. The cost
// is that REMOVING A LABEL IS NO LONGER A SAFE, REVERSIBLE ACT: an operator
// tidying up, or fixing a typo, stops new claims on that class at once. That is
// why "deprecated" exists and why docs/operations-cn.md leads with the inventory
// step. Objects that already exist are untouched either way; only new claims are
// refused.
//
// ⛔ PersistentVolume is not covered HERE, and the reasoning that first said so
// was wrong. It read: "refusing it would block a tenant from statically providing
// storage without protecting anything." The last clause was false. A
// PersistentVolume is cluster-scoped and the binder never looks at tenancy, so a
// tenant's static volume carrying a published class can be bound by ANOTHER
// tenant's claim -- and a static volume pre-empts dynamic provisioning, so it
// wins. What closes that is not the class name but the claimRef, because a
// tenant's own claim may only name a published class too: the legitimate use and
// the attack are the same write. See refuseUnreservedPV in pkg/convert/pv.go.
//
// IngressClass needs nothing here either:
// an unpublished ingress class is not refused but PREFIXED into the tenant's own
// namespace of names, so it can only be served by a controller the tenant runs.
func (tp *tenantProxy) refuseUnpublishedStorageClass(obj runtime.Object) error {
	if tp.publishedStorageClasses == nil {
		return nil
	}
	pvc, ok := obj.(*core.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	// Empty is not "names no class": it means the default class, which the
	// setdefault admission plugin fills in upstream, and which the platform chose.
	//
	// ⚠️ This line is what stops the check refusing MOST PVCs ever written --
	// Visible("") is false, so without it every claim that leaves the class to the
	// default would be turned away. It was already here, deliberately redundant,
	// while the check only looked at Retired(); it is load-bearing now.
	//
	// ⛔ And it is a hole by construction, which is worth stating rather than
	// papering over: setdefault runs UPSTREAM, after this, so a claim that names
	// no class reaches whichever class carries
	// storageclass.kubernetes.io/is-default-class -- published or not, retired or
	// not. Retiring the default class therefore requires clearing that annotation
	// too; the label alone does not stop new claims landing on it. Closing this
	// here would mean refusing empty, which refuses most claims ever written.
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil
	}
	name := *pvc.Spec.StorageClassName
	// ⚠️ Before the cache has filled, the store is legitimately empty, and empty
	// is indistinguishable from "the platform published nothing" -- so answering
	// from it would refuse every claim for the first seconds after each restart.
	// Not a hypothetical: the readiness gate keeps /readyz red until the informers
	// sync, but a single-replica StatefulSet has nothing draining traffic away
	// from it in the meantime.
	//
	// Answered as Unavailable rather than Invalid, and rather than letting the
	// claim through. A tenant sees "retry", which clients do; an operator can tell
	// it apart from a real refusal; and the boundary does not open for the first
	// seconds after every restart, which is the one window an attacker can arrange.
	if !tp.publishedStorageClasses.HasSynced() {
		return apierrors.NewServiceUnavailable(
			"the list of available storage classes is still loading; retry shortly")
	}
	if !tp.publishedStorageClasses.Visible(name) {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}, pvc.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("spec", "storageClassName"), name,
				fmt.Sprintf("no storage class %q is available to you; the ones that are "+
					"can be listed with `kubectl get storageclass`, and leaving this field "+
					"unset asks for the default one.", name),
			)})
	}
	if tp.publishedStorageClasses.Retired(name) {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}, pvc.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("spec", "storageClassName"), name,
				fmt.Sprintf("storage class %q is being retired and is not accepting new claims; "+
					"the ones still available are listed by `kubectl get storageclass`. "+
					"Claims that already use it keep working -- this refuses only new ones.", name),
			)})
	}
	return nil
}

// refusePlatformQuotaWrite stops a tenant editing the quota object the platform
// derived for it.
//
// ⛔ A tenant is admin in its own namespaces -- tenantNamespaceAdminRole is "*"
// on "*", deliberately, so that custom resources from tenant CRDs are covered --
// and the per-namespace ResourceQuota the ClusterResourceQuota controller
// derives lives in one of those namespaces. So the tenant can edit it, and one
// edit was enough: the reconciler finds that quota by its createdBy label while
// the admission webhook finds it by autoupdate, and the repair compared spec
// only. Removing just autoupdate left the reconciler finding an unchanged quota
// and doing nothing, while the webhook selected none at all.
//
// ⚠️ What stops is the TENANT-WIDE aggregate. Upstream's own admission keeps
// enforcing the per-namespace object, which reads no labels -- so every
// namespace admits the full allowance independently and the real ceiling becomes
// allowance times namespaces.
//
// ⭐ Refused here as well as repaired there, and the two do different jobs. The
// reconciler closes the permanent case but only after a reconcile; this closes
// the window, because kubezoo sees every tenant write before it lands. Neither
// alone is enough: the reconciler cannot stop the edit, and this cannot repair a
// quota that some other path already damaged.
//
// A tenant creating a ResourceQuota of its own is untouched -- limiting yourself
// further is always allowed. Only the platform's own object is protected, and it
// is recognised by the label the platform put there.
func (tp *tenantProxy) refusePlatformQuotaWrite(obj runtime.Object) error {
	quota, ok := obj.(*core.ResourceQuota)
	if !ok {
		return nil
	}
	if _, mine := quota.Labels[quotav1alpha1.ClusterResourceQuotaCreatedby]; !mine {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "resourcequotas"}, quota.Name,
		fmt.Errorf("this ResourceQuota is derived from your tenant quota and is maintained by "+
			"the platform; editing it would stop your tenant-wide quota being enforced. "+
			"Create a ResourceQuota of your own if you want to limit this namespace further"))
}

// refuseNewExternalIPs stops a tenant claiming traffic to addresses it does not
// own.
//
// ⛔ A Service carrying spec.externalIPs makes the data plane on EVERY node
// intercept traffic to those addresses and hand it to that Service's endpoints,
// with no check that the writer has any claim to the address. A tenant can take
// another tenant's service, the platform's own -- apiserver, DNS, a registry --
// or any address outside the cluster, so that every pod in the cluster talking to
// it reaches the tenant's pods instead. This is CVE-2020-8554.
//
// ⚠️ Kubernetes' own mitigation, the DenyServiceExternalIPs admission plugin,
// denies the field to EVERYONE including the platform. That is why the decision
// belongs here, where the writer's tenancy is known -- and why enabling the
// plugin upstream is not a substitute if the platform has a legitimate use.
//
// ⭐ Subset rather than refusal, which is the upstream plugin's rule: keeping or
// dropping an address is allowed, adding one is not. A Service that already
// carries an address therefore stays writable by its owner, who would otherwise
// be unable even to remove it.
func (tp *tenantProxy) refuseNewExternalIPs(obj, old runtime.Object) error {
	svc, ok := obj.(*core.Service)
	if !ok {
		return nil
	}
	oldSvc, _ := old.(*core.Service)
	if !convert.ExternalIPsAreNew(svc, oldSvc) {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Service"}, svc.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("spec", "externalIPs"),
			"claiming an external IP is not available to tenants: every node would "+
				"intercept traffic to that address and deliver it here, whoever the address "+
				"actually belongs to. Use a Service of type LoadBalancer, or ask the platform "+
				"to route the address to you.",
		)})
}

// refuseNodePorts stops a tenant opening a port on the platform's nodes.
//
// ⛔ Refused rather than confined, because there is nothing to confine it to. A
// tenant has no node concept at all: it does not own the machines, does not
// choose where its pods land, and has no way to address a node. A port opened on
// EVERY node is therefore outside everything this layer maintains -- reachable
// by anyone who can reach the node network, with no tenancy anywhere in the
// path, past the tenant's own NetworkPolicies. The range is shared and finite as
// well, so one tenant pinning 30080 takes it from every other.
//
// That is the whole reason. What follows is why the refusal costs nothing, not
// why it is right.
//
// ⭐ No capability is lost. kubetron does not use node ports: it forked away
// from the node:nodePort shape it inherited and addresses the backing pod
// directly. LoadBalancer stays the way a tenant publishes a Service -- and it is
// the way that has an owner, a cost, and an address belonging to somebody.
//
// ⚠️ kubetron's own "ports" are not these ports, and the difference is exactly
// the point. A Neutron/OVN port is a network attachment -- the interface a pod
// is plugged into, INSIDE the tenant's own network, with what may reach it
// governed by security groups the tenant administers in OpenStack, through
// Horizon. That policy surface is not in this API at all, which is why nothing
// here and nothing in kubetron's code manages it. A node port is an L4 port
// number on one of the platform's machines: outside any tenant network, and
// governed by no policy surface anywhere. Same word, opposite situations -- one
// is a tenant-owned thing whose controls simply live on another control plane,
// the other has no owner and no controls.
//
// Two refusals, because they are two different things to ask for: the type can
// be requested with no port named at all, and upstream then allocates one; and a
// port can be named on a Service of any type, including LoadBalancer.
//
// ⚠️ The subset rule on the named ports is not leniency. Services created before
// this rule carry ports upstream allocated for them, and an outright refusal
// would leave those unwritable by their owner -- who could then not even remove
// them. Adding is refused; keeping and dropping are not.
func (tp *tenantProxy) refuseNodePorts(obj, old runtime.Object) error {
	svc, ok := obj.(*core.Service)
	if !ok {
		return nil
	}
	if svc.Spec.Type == core.ServiceTypeNodePort {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "", Kind: "Service"}, svc.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("spec", "type"),
				"a Service of type NodePort is not available to tenants: it opens a port on "+
					"every node of the platform, and a node is not something you own or can "+
					"address. Use type LoadBalancer to publish a Service, or type ClusterIP "+
					"behind an Ingress.",
			)})
	}
	oldSvc, _ := old.(*core.Service)
	if !convert.NodePortsAreNew(svc, oldSvc) {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Service"}, svc.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("spec", "ports", "nodePort"),
			"naming a node port is not available to tenants: the port is opened on every "+
				"node of the platform, and the range is shared with every other tenant on a "+
				"first-come-first-served basis. Nothing in this platform's data plane "+
				"reaches your pods through it.",
		)})
}

// crdGVR is how a CustomResourceDefinition is addressed through the dynamic
// client, which is the only way to count them from here.
var crdGVR = schema.GroupVersionResource{
	Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
}

// refuseTooManyCRDs caps how many CustomResourceDefinitions one tenant may own.
//
// ⛔ A ceiling on a shared-structure amplifier, and a worse one than namespaces
// because it does not go away between requests. Every tenant CRD is a real CRD
// upstream, so it lands in that cluster's discovery document and its OpenAPI --
// which EVERY client of that cluster downloads, tenant or not. kubezoo holds
// its own informer over all of them and builds a type converter per CRD. One
// tenant with ten thousand CRDs makes every other tenant's kubectl slower, and
// there is nothing the other tenants can do about it.
//
// ⚠️ Namespaces cost per request (the cross-namespace fan-out); this costs
// continuously, whether the tenant that created them ever uses them again.
//
// ⭐ CREATE only, like the namespace cap and for the same reason: a tenant over
// the limit still has to be able to write and delete the CRDs it already has --
// deleting one is the only way back under the limit.
//
// ⚠️ A counting failure ALLOWS. An upstream hiccup must not reach a tenant as a
// quota it cannot see and cannot explain; the two are indistinguishable from
// where the tenant stands.
func (tp *tenantProxy) refuseTooManyCRDs(ctx context.Context, obj runtime.Object,
	tenantID string) error {

	limit := maxCRDsFor(tp.tenants, tenantID, tp.maxCRDs)
	if limit <= 0 {
		return nil
	}
	if _, ok := obj.(*apiextensions.CustomResourceDefinition); !ok {
		return nil
	}
	owned, err := tp.countTenantCRDs(ctx, tenantID)
	if err != nil {
		klog.Errorf("counting tenant %s's CRDs to apply --max-crds-per-tenant: %v", tenantID, err)
		return nil
	}
	if owned < limit {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "",
		fmt.Errorf("this tenant already owns %d custom resource definitions and the limit is %d; "+
			"delete one you no longer need, or ask the platform to raise the limit",
			owned, limit))
}

// countTenantCRDs counts the CRDs whose API group belongs to this tenant.
//
// ⚠️ Counted by GROUP prefix, not by name. A tenant's CRD is prefixed on its
// group -- pkg/convert's CRD transformer rewrites spec.group -- and the object's
// name is derived from it (<plural>.<group>), so matching on the name would work
// by accident today and stop working the moment naming changes. The group is
// where the tenancy actually lives.
func (tp *tenantProxy) countTenantCRDs(ctx context.Context, tenantID string) (int, error) {
	list, err := tp.dynamicClient.Resource(crdGVR).List(asTenantAdmin(ctx, tenantID), metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	prefix := tenantID + "-"
	owned := 0
	for i := range list.Items {
		group, found, err := unstructured.NestedString(list.Items[i].Object, "spec", "group")
		if err != nil || !found {
			continue
		}
		if strings.HasPrefix(group, prefix) {
			owned++
		}
	}
	return owned, nil
}

// refuseTooManyNamespaces caps how many namespaces one tenant may own.
//
// ⚠️ A ceiling on a shared-cluster amplifier, not a billing control. A tenant's
// cross-namespace list is assembled by reading each of its namespaces in turn --
// listAcrossNamespaces, one upstream request per namespace, in a loop -- so every
// `kubectl get pods` a tenant runs costs as many upstream requests as it owns
// namespaces, against the apiserver every tenant shares. Mostly-empty namespaces
// are the worst case, because the walk has to reach all of them before it can
// fill a single page.
//
// ⭐ Counted with a LIST rather than an informer on purpose. Creating a namespace
// is rare -- nothing does it in a loop -- and the alternative is another watch on
// every namespace in the cluster, carried permanently, to make a rare path
// cheaper.
//
// ⚠️ CREATE only. An update to an existing namespace must go through even when
// the tenant is over the limit, or lowering the cap would leave every namespace
// the tenant already owns unwritable -- and one of the things a tenant does to a
// namespace it owns is delete the workloads inside it.
func (tp *tenantProxy) refuseTooManyNamespaces(ctx context.Context, obj runtime.Object,
	tenantID string) error {

	limit := maxNamespacesFor(tp.tenants, tenantID, tp.maxNamespaces)
	if limit <= 0 {
		return nil
	}
	if _, ok := obj.(*core.Namespace); !ok {
		return nil
	}
	owned, _, err := tp.tenantNamespaces(ctx, tenantID, "")
	if err != nil {
		// Refusing on a failed count would make a blip in the upstream apiserver
		// look like a quota, and the tenant cannot tell the two apart. The
		// namespace goes through; the cap is a ceiling, not a boundary.
		klog.Errorf("counting tenant %s's namespaces to apply --max-namespaces-per-tenant: %v",
			tenantID, err)
		return nil
	}
	if len(owned) < limit {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "namespaces"}, "",
		fmt.Errorf("this tenant already owns %d namespaces and the limit is %d; "+
			"delete one you no longer need, or ask the platform to raise the limit",
			len(owned), limit))
}

// refuseTenantChosenNode refuses a pod that names the node it wants to run on.
//
// spec.nodeName goes around the scheduler completely: kubelet takes the pod
// because the pod names the node, and every placement rule that lives in the
// scheduler -- taints above all -- is simply not consulted. What still applies is
// nodeSelector, which kubelet checks, which is why the per-tenant pool LABEL is
// load-bearing and the taint alone is not.
//
// ⚠️ CREATE ONLY, and here that is not a preference but the only workable rule.
// The scheduler writes spec.nodeName onto every pod it binds, so from that
// moment every update to that pod -- a label, an annotation, a status write --
// carries it. Refusing on update would fail every later write to every running
// pod in the cluster. This is the one operation where the field can only have
// come from the tenant.
//
// A second copy of what tenant-scheduling.yaml denies, for the same reason the
// placement injection is: that policy is a webhook, and a webhook that was never
// registered does not fail, so failurePolicy does not cover it. Templates are
// handled in pkg/convert/placement.go instead, where the field can be cleared
// rather than refused because nothing but a tenant ever writes it there.
func (tp *tenantProxy) refuseTenantChosenNode(obj runtime.Object) error {
	pod, ok := obj.(*core.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Pod"}, pod.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("spec", "nodeName"),
			"choosing the node is not available to tenants: it goes around the scheduler, "+
				"and with it every rule about where your workloads may run. Leave it unset "+
				"and the scheduler will place the pod in your own node pool.",
		)})
}

// refuseUnpublishedVolumeAttributesClass refuses a claim that names a
// VolumeAttributesClass the platform is not offering.
//
// A VolumeAttributesClass carries the CSI driver's IOPS and throughput
// parameters, so naming one asks for a performance tier -- something a platform
// sells rather than something a tenant picks. Nothing validated this field
// before; it reached upstream untranslated, exactly as spec.storageClassName did
// before that was closed. The gate is GA and LockToDefault in 1.36, so it is live
// and cannot be switched off.
//
// ⚠️ NOT create-only, and copying the storage class rule here would be wrong.
// spec.volumeAttributesClassName is MUTABLE after the claim is bound -- the API
// says so in as many words and ValidatePersistentVolumeClaimUpdate excludes it
// from the immutability comparison for a Bound claim -- so a tenant can raise its
// own performance tier on an existing claim, and a create-only check would miss
// every such change.
//
// ⚠️ But only when the value CHANGES. old is the stored claim, nil on a create.
// Refusing on every update instead would fail each later write to a claim that
// already names the class -- a GitOps controller reapplying an unchanged manifest
// among them -- which is the reconcile loop the storage class rule exists to
// avoid. Withdrawing a class therefore leaves the claims already on it alone,
// same as there.
func (tp *tenantProxy) refuseUnpublishedVolumeAttributesClass(obj, old runtime.Object) error {
	if tp.publishedVolumeAttributesClasses == nil {
		return nil
	}
	pvc, ok := obj.(*core.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	name := derefClass(pvc.Spec.VolumeAttributesClassName)
	// Empty means no class is applied, which is the default and always allowed.
	// Unlike storageClassName nothing fills this in upstream, so empty really is
	// "none" rather than "the default one".
	if name == "" {
		return nil
	}
	if oldPVC, ok := old.(*core.PersistentVolumeClaim); ok &&
		derefClass(oldPVC.Spec.VolumeAttributesClassName) == name {
		// Unchanged. Whatever the platform has done with the class since, this
		// write is not what put the tenant on it.
		return nil
	}
	// Same reasoning as the storage class check: an empty cache is
	// indistinguishable from "the platform published nothing", so answer
	// Unavailable and let the client retry rather than refuse or wave it through.
	if !tp.publishedVolumeAttributesClasses.HasSynced() {
		return apierrors.NewServiceUnavailable(
			"the list of available volume attributes classes is still loading; retry shortly")
	}
	invalid := func(detail string) error {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}, pvc.Name,
			field.ErrorList{field.Invalid(
				field.NewPath("spec", "volumeAttributesClassName"), name, detail)})
	}
	if !tp.publishedVolumeAttributesClasses.Visible(name) {
		return invalid(fmt.Sprintf("no volume attributes class %q is available to you; "+
			"the ones that are can be listed with `kubectl get volumeattributesclass`, "+
			"and leaving this field unset applies none.", name))
	}
	if tp.publishedVolumeAttributesClasses.Retired(name) {
		return invalid(fmt.Sprintf("volume attributes class %q is being retired and is not "+
			"accepting new claims; claims already using it keep working -- this refuses "+
			"only new references to it.", name))
	}
	return nil
}

func derefClass(name *string) string {
	if name == nil {
		return ""
	}
	return *name
}

func (tp *tenantProxy) refuseReservedName(tenantID, upstreamName string) error {
	if tp.namespaceScoped || tp.kind.Group != "rbac.authorization.k8s.io" {
		return nil
	}
	if !common.IsReservedClusterName(tenantID, upstreamName) {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Group: tp.kind.Group, Resource: tp.resource},
		util.TrimTenantIDPrefix(tenantID, upstreamName),
		fmt.Errorf("this name is managed by the platform for your tenant and cannot be written directly; "+
			"choose another name"))
}

// shouldFanOut reports whether this list has to be assembled from the tenant's
// namespaces one at a time.
//
// Only when the resource is namespaced and the request named no namespace --
// `kubectl get pods -A` and the cluster-wide watches informers open. A request
// that names a namespace is already scoped and goes upstream untouched.
// listOptionScope tells the option convertor what kind of resource this is, which
// is what decides how a field selector has to be rewritten -- metadata.name is
// prefixed upstream for a cluster-scoped resource and not for a namespaced one,
// and a CRD carries its prefix in the middle of its name rather than at the front.
func (tp *tenantProxy) listOptionScope() util.ListOptionScope {
	return util.ListOptionScope{NamespaceScoped: tp.namespaceScoped, Kind: tp.kind}
}

func (tp *tenantProxy) shouldFanOut(ctx context.Context) bool {
	if !tp.namespaceScoped {
		return false
	}
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	return ok && requestInfo.Namespace == ""
}

// DeleteCollection convert the tenant object to upstream object before listing
// from the upstream server, and then delete the item one by one according to the list.
func (tp *tenantProxy) DeleteCollection(ctx context.Context, _ rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	if tp.newListFunc == nil {
		return nil, fmt.Errorf("newListFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context")
	}

	proxyListOptions, err := util.ConvertInternalListOptions(ctx, listOptions, tenantID, tp.listOptionScope())
	if err != nil {
		return nil, err
	}
	client, err := tp.getClient(ctx)
	if err != nil {
		return nil, err
	}
	utdList, err := client.List(ctx, *proxyListOptions)
	if err != nil {
		return nil, util.TrimTenantIDFromError(err, tenantID)
	}
	utdList = util.FilterUnstructuredList(utdList, tenantID, tp.namespaceScoped)
	if !tp.namespaceScoped {
		// Same leak as the list path: the response carries upstream's cursor.
		if err := hideUpstreamListCursor(utdList); err != nil {
			return nil, err
		}
	}
	// ⚠️ The one verb that operates on a whole collection was also the one verb
	// with no guard on the names kubezoo hides. Get returns NotFound for them and
	// List drops them, so a tenant is told the projection records do not exist --
	// and then a DeleteCollection deleted them anyway, upstream agreeing because
	// the tenant really is admin in its own namespaces. Deleting a *record* is
	// not repaired by anything: the controller sees an empty record set, deletes
	// every surviving copy in every namespace as an orphan, and withdraws the
	// derived cluster bindings, so the tenant's ClusterRoleBindings are silently
	// gone for good.
	utdList = tp.hideProjections(utdList)
	for i := range utdList.Items {
		name := utdList.Items[i].GetName()
		if err := tp.refuseProjectedName(name); err != nil {
			// Belt and braces: hideProjections already dropped these, and the
			// reserved names it does not cover are not the tenant's to delete.
			continue
		}
		if err := tp.refuseReservedName(tenantID, name); err != nil {
			continue
		}
		_, _, err = client.Delete(ctx, name, *options)
		if err != nil {
			return nil, util.TrimTenantIDFromError(err, tenantID)
		}
	}

	// convert upstream object to tenant object one at a time
	for i := range utdList.Items {
		// convert each item of the unstructured list to internal version for non-CRD resources
		oupObj := tp.New()
		if err := tp.convertUnstructuredToOutput(&utdList.Items[i], oupObj); err != nil {
			return nil, err
		}
		// convert to tenant
		if err := tp.convertUpstreamObjectToTenantObject(oupObj, tenantID, tp.requestNamespace(ctx)); err != nil {
			return nil, err
		}
		// convert it back to unstructured and put it back to the unstructured list
		utd, err := tp.convertInternalObjectToUnstructuredObject(oupObj)
		if err != nil {
			return nil, err
		}
		utdList.Items[i] = *utd
	}

	//if tp.isCustomResource {
	//	utdList.SetAPIVersion(util.TrimTenantIDPrefix(tp.tenantID, utdList.GetAPIVersion()))
	//}

	// convert the entire unstructured list to internal version of list for non-CRD resources
	oupList := tp.newList()
	if err := tp.convertUnstructuredListToOutput(utdList, oupList); err != nil {
		return nil, err
	}

	return oupList, nil
}

// Watch return a proxy watch if need proxy.
func (tp *tenantProxy) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	if tp.newListFunc == nil {
		return nil, fmt.Errorf("newListFunc is nil")
	}
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tanentID doesn't exist in context")
	}

	proxyOpt, err := util.ConvertInternalListOptions(ctx, options, tenantID, tp.listOptionScope())
	if err != nil {
		return nil, err
	}
	var w watch.Interface
	if tp.shouldFanOut(ctx) {
		// One stream per namespace, merged. See watchmux.go -- the tenant has
		// no cluster to watch, so its cross-namespace watch is its namespaces.
		namespaces, _, nsErr := tp.tenantNamespaces(ctx, tenantID, "")
		if nsErr != nil {
			return nil, util.TrimTenantIDFromError(nsErr, tenantID)
		}
		if w, err = newWatchMux(ctx, tp, tenantID, *proxyOpt, namespaces); err != nil {
			return nil, util.TrimTenantIDFromError(err, tenantID)
		}
	} else {
		client, err := tp.getClient(ctx)
		if err != nil {
			return nil, err
		}
		if w, err = client.Watch(ctx, *proxyOpt); err != nil {
			return nil, util.TrimTenantIDFromError(err, tenantID)
		}
	}
	return newProxyWatch(w, tp, tenantID, tp.requestNamespace(ctx))
}

// convertTenantObjectToUpstreamObject converts tenant object to upstream object.
func (tp *tenantProxy) convertTenantObjectToUpstreamObject(obj runtime.Object, tenantID string) error {
	// if obj is of type unstructured, it should be custom resource, whose apiVersion is prefixed with tenant id
	// (eg: 888888-stable.example.com), leave trimming of tenant id prefix to custom convertor
	if _, ok := obj.(*unstructured.Unstructured); !ok {
		// GVK for internal type object is always empty, set it with the right kind so that we can pick a convertor for it
		obj.GetObjectKind().SetGroupVersionKind(tp.kind)
	}
	return tp.convertor.ConvertTenantObjectToUpstreamObject(obj, tenantID, tp.namespaceScoped)
}

// requestNamespace returns the namespace exactly as the caller spelled it, which
// is not always the tenant's own name for it. Empty when the request names none.
func (tp *tenantProxy) requestNamespace(ctx context.Context) string {
	if !tp.namespaceScoped {
		return ""
	}
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	if !ok {
		return ""
	}
	return requestInfo.Namespace
}

// convertUpstreamObjectToTenantObject converts upstream object to tenant object.
//
// requestNamespace is the namespace as the CALLER spelled it, and answering in
// that spelling is the point of the parameter rather than a nicety.
//
// ⚠️ A tenant may address a namespace by either name -- its own `default`, or the
// upstream `<tid>-default` -- and the second is not a curiosity: a workload's
// client-go reads its namespace out of the projected service account file, which
// kubelet writes from the upstream apiserver's view, so every in-cluster
// controller a tenant runs uses the upstream spelling. This used to answer such a
// request with the object relabelled `default`, and then rest.EnsureObjectNamespace
// MatchesRequestNamespace -- which runs ABOVE this storage, in the generic patch
// and update handlers -- compared the two and refused the write with a BadRequest
// that says nothing about namespaces being rewritten at all.
//
// The failure was worse than a plain one because it depended on the client:
// `kubectl apply` survived by accident, its computed patch happening to carry
// metadata.namespace and so overwriting the answer, while `kubectl patch` and
// controller-runtime's MergeFrom -- which omit an unchanged namespace -- did not.
// Reads, creates and deletes were unaffected throughout, so an in-cluster
// controller could list and create but not patch.
//
// It is passed rather than read from a context here so that the compiler asks
// every call site what the caller said. Forgetting one is how this comes back.
func (tp *tenantProxy) convertUpstreamObjectToTenantObject(obj runtime.Object,
	tenantID, requestNamespace string) error {
	// if obj is of type unstructured, it should be custom resource, whose apiVersion is prefixed with tenant id
	// (eg: 888888-stable.example.com), leave trimming of tenant id prefix to custom convertor
	if _, ok := obj.(*unstructured.Unstructured); !ok {
		// GVK for internal type object is always empty, set it with the right kind so that we can pick a convertor for it
		obj.GetObjectKind().SetGroupVersionKind(tp.kind)
	}
	if err := tp.convertor.ConvertUpstreamObjectToTenantObject(obj, tenantID, tp.namespaceScoped); err != nil {
		return err
	}
	echoRequestNamespace(obj, tenantID, requestNamespace)
	return nil
}

// echoRequestNamespace puts the namespace back into the spelling the caller used.
//
// Returns nothing: every reason to leave the object alone is a legitimate answer
// rather than a failure, and an error nobody can act on would only invite a
// caller to abort a request over one of them.
func echoRequestNamespace(obj runtime.Object, tenantID, requestNamespace string) {
	if requestNamespace == "" {
		// A cluster-scoped request, or one spanning every namespace the tenant
		// owns -- there is no single spelling to answer in, and the objects come
		// from namespaces the caller never named.
		return
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		// Lists reach here too, and a list has no namespace of its own to fix.
		// Its items are converted one at a time by the caller.
		return
	}
	current := accessor.GetNamespace()
	if current == requestNamespace {
		return
	}
	// Only when the two names denote the SAME namespace. Anything else means the
	// object did not come from where the caller asked -- the ClusterRoleBinding
	// projection drives an inner RoleBinding proxy under the outer request's
	// context, so this really happens -- and stamping the request's namespace onto
	// it would hide that rather than surface it. This also covers an object
	// carrying no namespace at all: UpstreamNamespace leaves "" alone, so it can
	// never match a request that named one.
	if util.UpstreamNamespace(tenantID, current) != util.UpstreamNamespace(tenantID, requestNamespace) {
		return
	}
	accessor.SetNamespace(requestNamespace)
}

// guaranteedUpdate ensures a guaranteed updating.
// guaranteedUpdate serves a patch, retrying if another writer got in first.
//
// A patch may have to create the object. An apply always may -- server-side
// apply is a patch, and applying something for the first time is how it is
// normally used -- and that path was broken for every resource: Get answers a
// missing object with a nil and a NotFound, and the nil went to
// runtime.SetZeroValue, which returned "expected pointer, but got invalid kind".
// Measured with `kubectl apply --server-side` on a plain ConfigMap, which the
// same request straight to the API server accepts. It matters well beyond
// kubectl, because controller-runtime applies objects server-side as a matter of
// course: cert-manager's controller writes certificate secrets that way and its
// webhook signs its own CA that way, and both sat there retrying forever.
func (tp *tenantProxy) guaranteedUpdate(ctx context.Context, name string,
	objInfo rest.UpdatedObjectInfo, forceAllowCreate bool, options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	for {
		original, err := tp.readForUpdate(ctx, name)
		if err != nil && !errors.IsNotFound(err) {
			return nil, false, err
		}
		missing := errors.IsNotFound(err)
		if missing {
			if !forceAllowCreate {
				// An ordinary patch of something that is not there is a
				// NotFound, and saying so is the whole answer.
				return nil, false, err
			}
			// What the generic registry passes the transformer when the key is
			// empty: a zero value of the right type, never a nil.
			original = tp.New()
		}

		if err := checkPreconditions(objInfo.Preconditions(), original); err != nil {
			return nil, false, err
		}
		updated, err := objInfo.UpdatedObject(ctx, original)
		if err != nil {
			return nil, false, err
		}

		if missing {
			// Create, not update. Most resources refuse to be created by a PUT
			// -- AllowCreateOnUpdate is false for almost everything -- so
			// sending this as an update would come back NotFound after the
			// patch had already been resolved.
			created, err := tp.Create(ctx, updated, nil, &metav1.CreateOptions{
				DryRun: options.DryRun, FieldManager: options.FieldManager,
			})
			if errors.IsAlreadyExists(err) {
				// Someone created it while we were deciding. Go round again and
				// treat it as the update it now is.
				continue
			}
			if err != nil {
				return nil, false, err
			}
			return created, true, nil
		}

		if err := tp.refuseUnpublishedVolumeAttributesClass(updated, original); err != nil {
			return nil, false, err
		}
		// ⭐ PATCH too, and the parity test named exactly this attack: create an
		// ordinary Service, write the webhook against it, then patch the type to
		// ExternalName. Nothing revalidates the webhook -- the resolver runs at
		// call time -- so this write is the last chance to refuse it.
		if err := tp.refuseExternalNameService(updated); err != nil {
			return nil, false, err
		}
		if err := tp.refuseForgedEndpointAddress(ctx, updated); err != nil {
			return nil, false, err
		}
		// ⚠️ A template's volumes ARE mutable, unlike a live pod's: refusing only
		// on create would leave "create the workload clean, then patch the
		// ephemeral volume in" wide open -- the same two-step the parity test
		// named for the inline csi volume.
		if err := tp.refuseUnpublishedEphemeralClasses(updated, original); err != nil {
			return nil, false, err
		}
		if err := tp.refuseNewExternalIPs(updated, original); err != nil {
			return nil, false, err
		}
		if err := tp.refuseNodePorts(updated, original); err != nil {
			return nil, false, err
		}
		if err := tp.refuseUnpublishedDeviceClass(updated); err != nil {
			return nil, false, err
		}
		if err := tp.refusePlatformQuotaWrite(updated); err != nil {
			return nil, false, err
		}
		got, created, err := tp.update(ctx, updated, options)
		if errors.IsConflict(err) && strings.Contains(err.Error(), genericregistry.OptimisticLockErrorMsg) {
			// retry update on optimistic lock conflict
			continue
		}
		if err != nil {
			return nil, false, err
		}
		return got, created, nil
	}
}

// checkPreconditions checks the precondition for guarantee updating.
func checkPreconditions(preconditions *metav1.Preconditions, obj runtime.Object) error {
	if preconditions == nil {
		return nil
	}
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("can't enforce preconditions %v on un-introspectable object %v, got error: %v", *preconditions, obj, err)
	}
	if preconditions.UID != nil && *preconditions.UID != objMeta.GetUID() {
		return fmt.Errorf("precondition failed: UID in precondition: %v, UID in object meta: %v", *preconditions.UID, objMeta.GetUID())
	}
	if preconditions.ResourceVersion != nil && *preconditions.ResourceVersion != objMeta.GetResourceVersion() {
		return fmt.Errorf("Precondition failed: ResourceVersion in precondition: %v, ResourceVersion in object meta: %v", *preconditions.ResourceVersion, objMeta.GetResourceVersion())
	}
	return nil
}


// upstreamGroupVersion is the group/version this proxy addresses upstream.
//
// ⛔ A custom resource carries the tenant prefix on its group -- that is what
// makes it the tenant's, and what keeps two tenants' CRDs of the same name
// apart. A SHARED platform group does not: it exists upstream under its real
// name, which is also the name the platform's own controller watches. Prefixing
// it addresses a group that does not exist, and every request 404s.
//
// ⚠️ Found only by running it. Discovery advertised the group correctly and the
// CRD handler routed it correctly, and the request still failed -- the prefix is
// applied THREE separate times, in the read path, the fanout and the watch mux,
// and each one had to be found. A group being reachable and a group being
// FORWARDABLE are different things.
func (tp *tenantProxy) upstreamGroupVersion(tenantID string) schema.GroupVersion {
	gv := tp.kind.GroupVersion()
	if !tp.isCustomResource {
		return gv
	}
	if tp.isSharedGroup != nil && tp.isSharedGroup(gv.Group) {
		return gv
	}
	gv.Group = util.AddTenantIDPrefix(tenantID, gv.Group)
	return gv
}

// refuseExternalNameService stops a tenant reconstituting the one webhook
// capability kubezoo refuses outright.
//
// ⛔ MEASURED AGAINST SOURCE, THREE PLACES. pkg/convert/webhookconfiguration.go
// refuses clientConfig.url, and says why: "a URL cannot be confined to the
// tenant; use clientConfig.service to name a service in one of your namespaces".
// That reasoning has an unstated premise -- that a Service in the tenant's own
// namespace can only reach the tenant's own pods. True of ClusterIP. FALSE of
// ExternalName: upstream's ResolveCluster
// (staging/src/k8s.io/apiserver/pkg/util/proxy/proxy.go) has
//
//     case svc.Spec.Type == v1.ServiceTypeExternalName:
//         return &url.URL{Scheme: "https", Host: JoinHostPort(svc.Spec.ExternalName, port)}
//
// so a tenant writes an ExternalName Service in its own namespace, points a
// webhook's clientConfig.service at it -- the APPROVED path -- and the apiserver
// dials an arbitrary host and port FROM THE CONTROL PLANE'S NETWORK NAMESPACE,
// carrying an AdmissionReview. No tenant NetworkPolicy and no data-plane
// construct reaches that connection.
//
// ⚠️ Not gated on --enable-aggregator-routing. That flag chooses between two
// resolvers and BOTH reach this: the default NewClusterIPServiceResolver calls
// ResolveCluster, which is the function above.
//
// ⚠️ TLS is not a container either. The caBundle is the tenant's own and the
// ServerName is <svc>.<tid>-<ns>.svc, a name whose CA the tenant controls, so it
// can present a matching certificate on any host it likes.
//
// ⭐ Refused on the SERVICE rather than in the webhook path, and that is the
// whole design decision. Checking it where the webhook is written leaves a
// timing hole: create an ordinary Service, write the webhook against it, then
// patch the Service to ExternalName -- and nothing revalidates the webhook,
// because the resolver runs at call time. Closing that hole means checking the
// SERVICE write, which is this. The precise fix and the blunt one converge.
//
// ⚠️ The cost, stated rather than hidden: a tenant loses ExternalName entirely,
// including the legitimate use of aliasing an external name into cluster DNS. It
// can still use that external name directly. Giving it back needs a way to keep
// the control plane from resolving it -- which is upstream's behaviour, not
// kubezoo's -- so it is not a label away.
func (tp *tenantProxy) refuseExternalNameService(obj runtime.Object) error {
	svc, ok := obj.(*core.Service)
	if !ok || svc.Spec.Type != core.ServiceTypeExternalName {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Service"}, svc.Name,
		field.ErrorList{field.Forbidden(
			field.NewPath("spec", "type"),
			"an ExternalName service is a URL the control plane will follow: an admission "+
				"webhook naming it makes the apiserver connect to that host from its own "+
				"network, which is the reason clientConfig.url is refused. Point your "+
				"workloads at the external name directly instead.",
		)})
}
