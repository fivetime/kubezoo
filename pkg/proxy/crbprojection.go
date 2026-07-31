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
	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/rbac"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// clusterRoleBindingProjection serves ClusterRoleBindings for a tenant without
// ever creating one upstream.
//
// What a tenant means by cluster-wide is "all of my namespaces" -- its cluster
// is the set of namespaces kubezoo gives it. A real ClusterRoleBinding means
// every namespace in the shared cluster, so passing one through would grant a
// tenant's ServiceAccount the role in every other tenant's namespaces. Upstream
// refuses to create it, and that refusal is load-bearing: nine of the ten
// ClusterRoleBindings in cert-manager's chart are turned away by it.
//
// So the binding is projected instead. One RoleBinding per namespace the tenant
// owns, each granting exactly what the tenant asked for, in exactly the places
// the tenant meant. The effective permission is the faithful translation, and
// upstream needs no exemption to write it: binding a ClusterRole inside a
// namespace asks whether the writer holds those rules there, and the tenant
// holds star-on-star in each of its own namespaces. This costs no new privilege
// anywhere -- the projected writes go up impersonating the tenant, like every
// other write.
//
// One of the copies is also the record. The tenant's kube-system is where it
// lives, because the controller recreates that namespace if it goes missing,
// so the object cannot be lost by deleting a namespace. Reads are served from
// there alone, which keeps get, list and watch single-namespace operations
// rather than something spread across the tenant's whole world.
//
// The projections are hidden from the RoleBinding endpoints (see
// isProjectedBinding). Otherwise a tenant with ten namespaces would see ten
// copies of each of its ClusterRoleBindings in `kubectl get rolebindings -A`,
// which is exactly the illusion this is meant to keep.
type clusterRoleBindingProjection struct {
	// inner is a tenantProxy for rolebindings. Reusing it means the subject and
	// roleRef rewriting, the error reshaping and the tenant-prefix handling are
	// the same code that serves RoleBindings, rather than a second copy that can
	// drift from it.
	inner *tenantProxyWithLister

	kind           schema.GroupVersionKind
	newFunc        func() runtime.Object
	newListFunc    func() runtime.Object
	tableConvertor rest.TableConvertor
}

var _ = rest.StandardStorage(&clusterRoleBindingProjection{})

// canonicalNamespace is the tenant-facing namespace the record lives in. The
// tenant proxy prefixes it, so upstream this is <tid>-kube-system.
const canonicalNamespace = metav1.NamespaceSystem

// newClusterRoleBindingProjection builds the storage from the same config the
// generic proxy would have used, swapping the resource it actually talks to.
func newClusterRoleBindingProjection(config apiconfig.StorageConfig) (rest.Storage, error) {
	inner := config
	inner.Kind = config.Kind.GroupVersion().WithKind("RoleBinding")
	inner.Resource = "rolebindings"
	inner.NamespaceScoped = true
	inner.NewFunc = func() runtime.Object { return &rbac.RoleBinding{} }
	inner.NewListFunc = func() runtime.Object { return &rbac.RoleBindingList{} }
	inner.TableConvertor = nil

	storage, err := NewTenantProxy(inner)
	if err != nil {
		return nil, err
	}
	lister, ok := storage.(*tenantProxyWithLister)
	if !ok {
		return nil, fmt.Errorf("expected a listing proxy for rolebindings, got %T", storage)
	}
	// This one is the projection's own way in to the records, so it must not
	// hide them from itself.
	lister.hideProjected = false
	// asRoleBinding stamps this label before the inner storage sees the object,
	// which is above the window conversionDelta can compare -- so a forwarded
	// server-side apply would drop it, and the label is what every consumer of a
	// projection record selects on. See tenantProxy.injectedPaths.
	lister.injectedPaths = projectionLabelPath()

	tableConvertor := config.TableConvertor
	if tableConvertor == nil {
		tableConvertor = rest.NewDefaultTableConvertor(rbac.Resource("clusterrolebindings"))
	}
	return &clusterRoleBindingProjection{
		inner:          lister,
		kind:           config.Kind,
		newFunc:        config.NewFunc,
		newListFunc:    config.NewListFunc,
		tableConvertor: tableConvertor,
	}, nil
}

func (p *clusterRoleBindingProjection) New() runtime.Object     { return p.newFunc() }
func (p *clusterRoleBindingProjection) NewList() runtime.Object { return p.newListFunc() }
func (p *clusterRoleBindingProjection) Destroy()                {}
func (p *clusterRoleBindingProjection) NamespaceScoped() bool   { return false }
func (p *clusterRoleBindingProjection) GetSingularName() string { return "clusterrolebinding" }
func (p *clusterRoleBindingProjection) ShortNames() []string    { return nil }

func (p *clusterRoleBindingProjection) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return p.kind
}

func (p *clusterRoleBindingProjection) ConvertToTable(ctx context.Context, object runtime.Object,
	tableOptions runtime.Object) (*metav1.Table, error) {
	return p.tableConvertor.ConvertToTable(ctx, object, tableOptions)
}

// inNamespace returns a context that addresses the record's namespace, so the
// inner proxy resolves the same way it would for a namespaced request.
func inNamespace(ctx context.Context, namespace string) context.Context {
	info, ok := apirequest.RequestInfoFrom(ctx)
	if !ok {
		return ctx
	}
	scoped := *info
	scoped.Namespace = namespace
	scoped.Resource = "rolebindings"
	return apirequest.WithRequestInfo(apirequest.WithNamespace(ctx, namespace), &scoped)
}

// asClusterRoleBindingError rewrites an error about the record into one about
// the object the tenant asked for.
//
// Without it a tenant that mistypes a name is told that a RoleBinding called
// kubezoo:clusterrolebinding:whatever does not exist, which names an object it
// has never heard of and cannot look at.
func asClusterRoleBindingError(err error, name string) error {
	if err == nil {
		return nil
	}
	status, ok := err.(apierrors.APIStatus)
	if !ok {
		return err
	}
	original := status.Status()
	reshaped := *original.DeepCopy()
	recordName := util.ProjectedBindingName(name)
	reshaped.Message = strings.ReplaceAll(reshaped.Message, recordName, name)
	reshaped.Message = strings.ReplaceAll(reshaped.Message, "rolebindings.rbac.authorization.k8s.io",
		"clusterrolebindings.rbac.authorization.k8s.io")
	if reshaped.Details != nil {
		details := *reshaped.Details
		details.Name = name
		details.Kind = "clusterrolebindings"
		reshaped.Details = &details
	}
	return &apierrors.StatusError{ErrStatus: reshaped}
}

// Get reads the record and presents it as the cluster-scoped object the tenant
// wrote.
func (p *clusterRoleBindingProjection) Get(ctx context.Context, name string,
	options *metav1.GetOptions) (runtime.Object, error) {

	obj, err := p.inner.Get(inNamespace(ctx, canonicalNamespace), util.ProjectedBindingName(name), options)
	if err != nil {
		return nil, asClusterRoleBindingError(err, name)
	}
	return asClusterRoleBinding(obj)
}

// List returns every record, and only records: a tenant's ordinary RoleBindings
// live in the same namespace and must not appear here.
func (p *clusterRoleBindingProjection) List(ctx context.Context,
	options *metainternalversion.ListOptions) (runtime.Object, error) {

	scoped, err := withProjectedSelector(options)
	if err != nil {
		return nil, err
	}
	obj, err := p.inner.List(inNamespace(ctx, canonicalNamespace), scoped)
	if err != nil {
		return nil, err
	}
	list, ok := obj.(*rbac.RoleBindingList)
	if !ok {
		return nil, fmt.Errorf("expected a RoleBindingList from the record namespace, got %T", obj)
	}
	out := &rbac.ClusterRoleBindingList{ListMeta: list.ListMeta}
	for i := range list.Items {
		converted, err := asClusterRoleBinding(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, *converted.(*rbac.ClusterRoleBinding))
	}
	return out, nil
}

// Watch streams the records, converted. Only the record namespace is watched:
// the copies carry no information the record does not, and streaming them all
// would emit one event per namespace for every change.
func (p *clusterRoleBindingProjection) Watch(ctx context.Context,
	options *metainternalversion.ListOptions) (watch.Interface, error) {

	scoped, err := withProjectedSelector(options)
	if err != nil {
		return nil, err
	}
	inner, err := p.inner.Watch(inNamespace(ctx, canonicalNamespace), scoped)
	if err != nil {
		return nil, err
	}
	return newProjectionWatch(inner), nil
}

// Create writes the record and then the copies.
//
// The copies are written here rather than left to the controller because a
// chart installs a binding and immediately expects its workload to be able to
// act. Waiting for a resync would mean the operator spends that time being
// refused. The controller reconciles them anyway, which is what repairs a
// partial write -- so a copy that fails here is logged and left for it, while
// the record, the thing the tenant can see and delete, is what this call
// succeeds or fails on.
func (p *clusterRoleBindingProjection) Create(ctx context.Context, obj runtime.Object,
	createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {

	record, err := asRoleBinding(obj)
	if err != nil {
		return nil, err
	}
	created, err := p.inner.Create(inNamespace(ctx, canonicalNamespace), record, createValidation, options)
	if err != nil {
		return nil, asClusterRoleBindingError(err, obj.(*rbac.ClusterRoleBinding).Name)
	}
	p.project(ctx, record.(*rbac.RoleBinding).Name)
	return asClusterRoleBinding(created)
}

func (p *clusterRoleBindingProjection) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {

	recordName := util.ProjectedBindingName(name)
	updated, created, err := p.inner.Update(inNamespace(ctx, canonicalNamespace), recordName,
		&projectedUpdatedObject{inner: objInfo}, createValidation, updateValidation, forceAllowCreate, options)
	if err != nil {
		return nil, false, asClusterRoleBindingError(err, name)
	}
	p.project(ctx, recordName)
	out, err := asClusterRoleBinding(updated)
	return out, created, err
}

// Delete removes the record first. A copy left behind still grants, so the
// copies are removed too, and the controller removes any this misses because it
// treats a copy with no record as an orphan.
func (p *clusterRoleBindingProjection) Delete(ctx context.Context, name string,
	deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {

	recordName := util.ProjectedBindingName(name)
	deleted, immediate, err := p.inner.Delete(inNamespace(ctx, canonicalNamespace), recordName, deleteValidation, options)
	if err != nil {
		return nil, false, asClusterRoleBindingError(err, name)
	}
	p.unproject(ctx, recordName)
	if _, isRecord := deleted.(*rbac.RoleBinding); !isRecord {
		// A delete answers with a Status rather than the object unless the
		// resource supports graceful deletion. Passing it through is what the
		// generic proxy does, and converting it is what used to fail the call
		// after the deletion had already happened.
		return deleted, immediate, nil
	}
	out, err := asClusterRoleBinding(deleted)
	return out, immediate, err
}

func (p *clusterRoleBindingProjection) DeleteCollection(ctx context.Context,
	deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions,
	listOptions *metainternalversion.ListOptions) (runtime.Object, error) {

	listed, err := p.List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	list := listed.(*rbac.ClusterRoleBindingList)
	for i := range list.Items {
		if _, _, err := p.Delete(ctx, list.Items[i].Name, deleteValidation, options); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// project copies the record into every other namespace the tenant owns.
func (p *clusterRoleBindingProjection) project(ctx context.Context, recordName string) {
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return
	}
	namespaces, _, err := p.inner.tenantNamespaces(ctx, tenantID, "")
	if err != nil {
		klog.Warningf("tenant %s: could not list its namespaces to project clusterrolebinding %s; "+
			"the controller will do it on the next resync: %v", tenantID, recordName, err)
		return
	}
	client := p.inner.dynamicClient.Resource(roleBindingGVR)
	upstreamRecord := util.AddTenantIDPrefix(tenantID, canonicalNamespace)
	record, err := client.Namespace(upstreamRecord).Get(ctx, recordName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("tenant %s: could not read back clusterrolebinding record %s to project it: %v",
			tenantID, recordName, err)
		return
	}
	for _, namespace := range namespaces {
		if namespace == upstreamRecord {
			continue
		}
		copied := record.DeepCopy()
		copied.SetNamespace(namespace)
		copied.SetResourceVersion("")
		copied.SetUID("")
		copied.SetCreationTimestamp(metav1.Time{})
		copied.SetManagedFields(nil)
		if _, err := client.Namespace(namespace).Create(ctx, copied, metav1.CreateOptions{}); err == nil {
			continue
		}
		// Already there, or racing another writer. Update rather than read
		// first, since the common case for a chart upgrade is that it exists.
		existing, getErr := client.Namespace(namespace).Get(ctx, recordName, metav1.GetOptions{})
		if getErr != nil {
			klog.Warningf("tenant %s: clusterrolebinding %s is not in namespace %s and could not be "+
				"put there; the controller will retry: %v", tenantID, recordName, namespace, getErr)
			continue
		}
		copied.SetResourceVersion(existing.GetResourceVersion())
		if _, _, err := client.Namespace(namespace).Update(ctx, copied, metav1.UpdateOptions{}); err != nil {
			klog.Warningf("tenant %s: clusterrolebinding %s is stale in namespace %s; the controller "+
				"will retry: %v", tenantID, recordName, namespace, err)
		}
	}
}

// unproject removes the copies.
func (p *clusterRoleBindingProjection) unproject(ctx context.Context, recordName string) {
	tenantID, ok := util.TenantFrom(ctx)
	if !ok {
		return
	}
	namespaces, _, err := p.inner.tenantNamespaces(ctx, tenantID, "")
	if err != nil {
		klog.Warningf("tenant %s: could not list its namespaces to withdraw clusterrolebinding %s; "+
			"the controller will remove the copies as orphans: %v", tenantID, recordName, err)
		return
	}
	client := p.inner.dynamicClient.Resource(roleBindingGVR)
	for _, namespace := range namespaces {
		if _, _, err := client.Namespace(namespace).Delete(ctx, recordName, metav1.DeleteOptions{}); err != nil {
			klog.V(4).Infof("tenant %s: removing clusterrolebinding %s from namespace %s: %v",
				tenantID, recordName, namespace, err)
		}
	}
}

var roleBindingGVR = schema.GroupVersionResource{
	Group:    "rbac.authorization.k8s.io",
	Version:  "v1",
	Resource: "rolebindings",
}

// withProjectedSelector narrows a list or watch to the records, and rewrites the
// field selector into the records' own terms.
//
// ⚠️ The field selector used to go through untouched, and this is the one place
// util.ConvertInternalListOptions cannot help: it is handed the *inner* storage's
// scope, which says namespaced RoleBinding, so it correctly leaves metadata.name
// alone -- while the object the client asked about is a cluster-scoped
// ClusterRoleBinding whose upstream name is neither the tenant's name nor
// tenantID+"-"+name, but ProjectedBindingName(name).
//
// Two ways that went wrong, both silent:
//   - `metadata.name=my-crb` matched nothing, because the record is called
//     kubezoo:clusterrolebinding:my-crb. A single-object informer -- the standard
//     client-go pattern, and the very case the option convertor's own comment
//     names -- watched an empty world forever, while `kubectl get
//     clusterrolebinding my-crb` worked, since Get translates the name.
//   - `metadata.name!=keepme` matched *everything*, keepme's own record included,
//     because no record is literally named keepme. A DeleteCollection carrying it
//     deleted the one object it was told to spare.
//
// Transforming the value covers both, because Transform preserves the operator:
// name!=X becomes recordName!=ProjectedBindingName(X), which is exact.
func withProjectedSelector(options *metainternalversion.ListOptions) (*metainternalversion.ListOptions, error) {
	scoped := metainternalversion.ListOptions{}
	if options != nil {
		scoped = *options
	}
	selector := labels.SelectorFromSet(labels.Set{common.ProjectedClusterRoleBindingLabelKey: "true"})
	if scoped.LabelSelector != nil && !scoped.LabelSelector.Empty() {
		if requirements, selectable := scoped.LabelSelector.Requirements(); selectable {
			selector = selector.Add(requirements...)
		}
	}
	scoped.LabelSelector = selector

	if scoped.FieldSelector != nil {
		transformed, err := scoped.FieldSelector.Transform(func(label, value string) (string, string, error) {
			switch label {
			case "metadata.name":
				if value != "" {
					value = util.ProjectedBindingName(value)
				}
			case "metadata.namespace":
				// A ClusterRoleBinding has no namespace. Left alone it would be
				// matched against the record's, which is the tenant's kube-system
				// and means nothing the client could have intended.
				return "", "", fmt.Errorf(
					"metadata.namespace is not a field of a cluster-scoped clusterrolebinding")
			}
			return label, value, nil
		})
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		scoped.FieldSelector = transformed
	}
	return &scoped, nil
}

// projectionLabelPath is the one field this storage injects, named so that a
// forwarded server-side apply carries it.
func projectionLabelPath() *fieldpath.Set {
	label := common.ProjectedClusterRoleBindingLabelKey
	return fieldpath.NewSet(fieldpath.Path{
		fieldpath.PathElement{FieldName: strPtr("metadata")},
		fieldpath.PathElement{FieldName: strPtr("labels")},
		fieldpath.PathElement{FieldName: &label},
	})
}

func strPtr(s string) *string { return &s }

// asRoleBinding turns the tenant's ClusterRoleBinding into the record.
func asRoleBinding(obj runtime.Object) (runtime.Object, error) {
	crb, ok := obj.(*rbac.ClusterRoleBinding)
	if !ok {
		return nil, fmt.Errorf("expected a ClusterRoleBinding, got %T", obj)
	}
	// Validated here because storing it as a RoleBinding is what lets it past
	// validation at all: upstream requires a namespace on a ServiceAccount
	// subject of a cluster-scoped binding, and does not for a namespaced one. An
	// empty namespace has no single meaning here -- each projected copy would
	// resolve it to its own namespace, which is not what a ClusterRoleBinding
	// says -- and the cluster-scoped binding derived from the record is invalid
	// outright, which failed the rest of that tenant's reconcile every pass.
	for i := range crb.Subjects {
		if crb.Subjects[i].Kind == rbac.ServiceAccountKind && crb.Subjects[i].Namespace == "" {
			return nil, apierrors.NewInvalid(
				rbac.Kind("ClusterRoleBinding"), crb.Name,
				field.ErrorList{field.Required(
					field.NewPath("subjects").Index(i).Child("namespace"),
					"a ServiceAccount subject of a ClusterRoleBinding must name its namespace")})
		}
	}
	record := &rbac.RoleBinding{
		ObjectMeta: *crb.ObjectMeta.DeepCopy(),
		RoleRef:    crb.RoleRef,
		Subjects:   crb.Subjects,
	}
	record.Name = util.ProjectedBindingName(crb.Name)
	record.GenerateName = ""
	record.Namespace = canonicalNamespace
	if record.Labels == nil {
		record.Labels = map[string]string{}
	}
	record.Labels[common.ProjectedClusterRoleBindingLabelKey] = "true"
	return record, nil
}

// asClusterRoleBinding turns a record back into what the tenant wrote.
func asClusterRoleBinding(obj runtime.Object) (runtime.Object, error) {
	record, ok := obj.(*rbac.RoleBinding)
	if !ok {
		return nil, fmt.Errorf("expected a RoleBinding record, got %T", obj)
	}
	crb := &rbac.ClusterRoleBinding{
		ObjectMeta: *record.ObjectMeta.DeepCopy(),
		RoleRef:    record.RoleRef,
		Subjects:   record.Subjects,
	}
	crb.Name = util.TrimProjectedBindingName(record.Name)
	crb.Namespace = ""
	delete(crb.Labels, common.ProjectedClusterRoleBindingLabelKey)
	if len(crb.Labels) == 0 {
		crb.Labels = nil
	}
	return crb, nil
}

// projectedUpdatedObject adapts the tenant's update, which is written against a
// ClusterRoleBinding, to the record the inner proxy is updating.
type projectedUpdatedObject struct {
	inner rest.UpdatedObjectInfo
}

func (o *projectedUpdatedObject) Preconditions() *metav1.Preconditions {
	return o.inner.Preconditions()
}

func (o *projectedUpdatedObject) UpdatedObject(ctx context.Context, oldObj runtime.Object) (runtime.Object, error) {
	oldCRB, err := asClusterRoleBinding(oldObj)
	if err != nil {
		return nil, err
	}
	updated, err := o.inner.UpdatedObject(ctx, oldCRB)
	if err != nil {
		return nil, err
	}
	return asRoleBinding(updated)
}

// projectionWatch converts the record's events into the tenant's.
type projectionWatch struct {
	inner    watch.Interface
	result   chan watch.Event
	stop     chan struct{}
	stopOnce sync.Once
}

func newProjectionWatch(inner watch.Interface) watch.Interface {
	w := &projectionWatch{
		inner:  inner,
		result: make(chan watch.Event),
		stop:   make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *projectionWatch) run() {
	defer close(w.result)
	for {
		select {
		case <-w.stop:
			return
		case event, open := <-w.inner.ResultChan():
			if !open {
				return
			}
			if record, ok := event.Object.(*rbac.RoleBinding); ok {
				converted, err := asClusterRoleBinding(record)
				if err != nil {
					continue
				}
				event.Object = converted
			}
			// ⚠️ This send used to have no escape. Stopping the inner watch does
			// not unblock a send already in flight on an unbuffered channel whose
			// reader has gone, so a ClusterRoleBinding watch stopped at the wrong
			// instant leaked this goroutine for the life of the process. Every
			// other pump in the package selects on a stop channel; this one did
			// not.
			select {
			case w.result <- event:
			case <-w.stop:
				return
			}
		}
	}
}

func (w *projectionWatch) ResultChan() <-chan watch.Event { return w.result }

func (w *projectionWatch) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	w.inner.Stop()
}

// unusedUnstructured keeps the import list honest if the projection ever stops
// touching unstructured objects directly.
var _ = unstructured.Unstructured{}
