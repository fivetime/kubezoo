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

package util

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1b1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	v1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	//"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog"
)

const (
	TenantIDSeparator = "-"
	// TODO(renjingsi): move this to tenant apis and add some validations
	TenantIDLength = 6
	TenantIDKey    = "tenant"
)

// AddTenantIDPrefix add tenantId as the prefix.
func AddTenantIDPrefix(tenantID, input string) string {
	return tenantID + TenantIDSeparator + input
}

// UpstreamNamespace is the namespace a tenant's request is really addressing.
//
// Normally the tenant's view of a namespace is the upstream name without the
// prefix, and this puts it back. The exception is a request that already carries
// the prefix, which is what an in-cluster workload sends: a pod learns its own
// namespace from the environment kubelet gave it, and that is the upstream name.
// Prefixing it again produced <tid>-<tid>-default, and every operator that
// addresses its own namespace -- which is most of them -- got NotFound.
// Measured with cert-manager's webhook.
//
// The two spellings are the same namespace, and the tenant's prefix is reserved
// in the names a tenant may choose (see NamespaceNameForTenant), so there is no
// namespace this could reach that the tenant does not own.
//
// ⚠️ What this does not fix is the spelling of the answer: objects come back
// saying the tenant's name for the namespace, so a client that compares an
// object's namespace against the one it read from its environment still sees a
// mismatch. The policy layer removes that by handing pods the tenant's name in
// the first place; this is what keeps the ones it cannot reach working.
func UpstreamNamespace(tenantID, namespace string) string {
	if namespace == "" {
		return namespace
	}
	if strings.HasPrefix(namespace, tenantID+TenantIDSeparator) {
		return namespace
	}
	return AddTenantIDPrefix(tenantID, namespace)
}

// NamespaceNameForTenant reports whether a tenant may call a namespace this.
//
// The tenant's own prefix is reserved, because UpstreamNamespace reads a name
// carrying it as one already resolved. Without this a namespace a tenant called
// <tid>-foo would be stored as <tid>-<tid>-foo and then be unreachable, which is
// worse than being refused.
func NamespaceNameForTenant(tenantID, name string) error {
	if !strings.HasPrefix(name, tenantID+TenantIDSeparator) {
		return nil
	}
	return fmt.Errorf("a namespace may not be called %q: names beginning with %q are how your "+
		"workloads address these namespaces from inside the cluster, and one stored under that "+
		"name could not be reached", name, tenantID+TenantIDSeparator)
}

// TrimTenantIDPrefix removes tenantId prefix.
func TrimTenantIDPrefix(tenantID, input string) string {
	return strings.TrimPrefix(input, tenantID+TenantIDSeparator)
}

var invalidPrefixedNamespaceErr = fmt.Errorf("TenantID prefixed namespace must be in the form %s",
	AddTenantIDPrefix("tenantID", "namespace"))

var (
	errInvalidTenantNameLength = "tenant ID must be exactly 6 characters"
	errNonRfc1123TenantName    = "tenant ID must contain only alphanumeric characters or -, and must not start with a -"
)

func ValidateTenantName(tenantId string) *string {
	if len(tenantId) != TenantIDLength {
		return &errInvalidTenantNameLength
	}

	// RFC 1123 validation
	// The last character can be dash because tenant ID is always joined with the actual namespace name,
	// so the trailing dash will only lead to names like `tenant--default`
	for i, char := range tenantId {
		if 'a' <= char && char <= 'z' {
			continue
		}
		if '0' <= char && char <= '9' {
			continue
		}
		if i > 0 && char == '-' {
			continue
		}
		return &errNonRfc1123TenantName
	}

	return nil
}

// GetTenantIDFromNamespace get the tenantId from the prefix of namespace.
func GetTenantIDFromNamespace(namespace string) (string, error) {
	if len(namespace) < TenantIDLength+2 {
		return "", invalidPrefixedNamespaceErr
	}
	if namespace[TenantIDLength] != '-' {
		return "", invalidPrefixedNamespaceErr
	}
	return namespace[:TenantIDLength], nil
}

// AddTenantIDToUserInfo add the tenantId to the extra of userinfo.
func AddTenantIDToUserInfo(tenantID string, info user.Info) user.Info {
	extra := info.GetExtra()
	if extra == nil {
		extra = map[string][]string{}
	}
	extra[TenantIDKey] = []string{tenantID}
	return &user.DefaultInfo{
		Name:   info.GetName(),
		UID:    info.GetUID(),
		Groups: info.GetGroups(),
		Extra:  extra,
	}
}

// UpstreamObjectBelongsToTenant returns true if object belongs to tenant according to tenantID.
func UpstreamObjectBelongsToTenant(obj runtime.Object, tenantID string, isNamespaceScoped bool) bool {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		klog.Errorf("failed to get accessor for object: %+v", obj)
		return false
	}

	// name of crd object is in the form: <plural>.<tenantID>-<group>
	if IsCRDObject(obj) {
		parts := strings.SplitN(accessor.GetName(), ".", 2)
		if len(parts) < 2 {
			return false
		}
		return strings.HasPrefix(parts[1], tenantID+"-")
	}

	// Nodes used to be exempted here and returned to every tenant, so that the
	// Conformance suite would pass. What a tenant saw was the platform's own
	// machines: their names, labels, addresses, capacity, and the kernel,
	// runtime and kubelet versions in status.nodeInfo -- a description of the
	// infrastructure it shares with every other tenant, and a list of what to
	// look up when something there has a CVE.
	//
	// They are now treated as the cluster-scoped objects they are, so the name
	// prefix decides, and none of the platform's nodes carry one. Conformance
	// loses whatever it was gaining; a tenant cannot inspect the machines the
	// platform runs.

	// non-crd object is namespace scoped
	if isNamespaceScoped {
		return strings.HasPrefix(accessor.GetNamespace(), tenantID+"-")
	}
	// non-crd object is cluster scoped
	return strings.HasPrefix(accessor.GetName(), tenantID+"-")
}

// IsCRDObject checks whether the input obj is a CRD object or not.
func IsCRDObject(obj runtime.Object) bool {
	switch v := obj.(type) {
	case *unstructured.Unstructured:
		t, err := meta.TypeAccessor(obj)
		if err != nil {
			klog.Errorf("failed to get type for object: %+v", v)
			return false
		}
		if (t.GetAPIVersion() == "apiextensions.k8s.io/v1" || t.GetAPIVersion() == "apiextensions.k8s.io/v1beta1") &&
			t.GetKind() == "CustomResourceDefinition" {
			return true
		}
	case *apiextensionsv1.CustomResourceDefinition:
		return true
	case *apiextensionsv1b1.CustomResourceDefinition:
		return true
	}
	return false
}

// ConvertCRDNameToUpstream convert the name of CRD with adding tenantId prefix in group.
func ConvertCRDNameToUpstream(name, tenantID string) string {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) < 2 {
		klog.Errorf("invalid crd name: %s", name)
		return name
	}
	plural, group := parts[0], parts[1]
	return fmt.Sprintf("%s.%s-%s", plural, tenantID, group)
}

// ConvertTenantObjectNameToUpstream convert the object to upstream object by adding tenantId prefix.
func ConvertTenantObjectNameToUpstream(name, tenantID string, gvk schema.GroupVersionKind) string {
	if (gvk.GroupVersion().String() == "apiextensions.k8s.io/v1" || gvk.GroupVersion().String() == "apiextensions.k8s.io/v1beta1") &&
		gvk.Kind == "CustomResourceDefinition" {
		return ConvertCRDNameToUpstream(name, tenantID)
	}
	return AddTenantIDPrefix(tenantID, name)
}

// ConvertUpstreamApiGroupToTenant convert upstream the apigroup to tenant by trimming the tenantId prefix.
func ConvertUpstreamApiGroupToTenant(tenantID string, apiGroup *metav1.APIGroup) {
	if apiGroup == nil {
		return
	}
	apiGroup.Name = TrimTenantIDPrefix(tenantID, apiGroup.Name)
	for i := range apiGroup.Versions {
		apiGroup.Versions[i].GroupVersion = TrimTenantIDPrefix(tenantID, apiGroup.Versions[i].GroupVersion)
	}
	apiGroup.PreferredVersion.GroupVersion = TrimTenantIDPrefix(tenantID, apiGroup.PreferredVersion.GroupVersion)
}

// ConvertUpstreamResourceListToTenant convert upstream resource list to tenant by trimming the tenantId prefix.
func ConvertUpstreamResourceListToTenant(tenantID string, resourceList *metav1.APIResourceList) {
	if resourceList == nil {
		return
	}
	resourceList.GroupVersion = TrimTenantIDPrefix(tenantID, resourceList.GroupVersion)
	for i := range resourceList.APIResources {
		resourceList.APIResources[i].Group = TrimTenantIDPrefix(tenantID, resourceList.APIResources[i].Group)
	}
}

// GetUnstructured return Unstructured for any given kubernetes type.
func GetUnstructured(resource interface{}) (*unstructured.Unstructured, error) {
	content, err := json.Marshal(resource)
	if err != nil {
		return nil, err
	}
	unstructuredResource := &unstructured.Unstructured{}
	err = unstructuredResource.UnmarshalJSON(content)
	if err != nil {
		return nil, err
	}
	return unstructuredResource, nil
}

var groupKindNamespaced = map[metav1.GroupKind]bool{
	{Group: "", Kind: "Binding"}:                                                      true,
	{Group: "", Kind: "ComponentStatus"}:                                              false,
	{Group: "", Kind: "ConfigMap"}:                                                    true,
	{Group: "", Kind: "Endpoints"}:                                                    true,
	{Group: "", Kind: "Event"}:                                                        true,
	{Group: "", Kind: "LimitRange"}:                                                   true,
	{Group: "", Kind: "Namespace"}:                                                    false,
	{Group: "", Kind: "Node"}:                                                         false,
	{Group: "", Kind: "PersistentVolumeClaim"}:                                        true,
	{Group: "", Kind: "PersistentVolume"}:                                             false,
	{Group: "", Kind: "Pod"}:                                                          true,
	{Group: "", Kind: "PodTemplate"}:                                                  true,
	{Group: "", Kind: "ReplicationController"}:                                        true,
	{Group: "", Kind: "ResourceQuota"}:                                                true,
	{Group: "", Kind: "Secret"}:                                                       true,
	{Group: "", Kind: "ServiceAccount"}:                                               true,
	{Group: "", Kind: "Service"}:                                                      true,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}:     false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}:   false,
	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:                 false,
	{Group: "apps", Kind: "ControllerRevision"}:                                       true,
	{Group: "apps", Kind: "DaemonSet"}:                                                true,
	{Group: "apps", Kind: "Deployment"}:                                               true,
	{Group: "apps", Kind: "ReplicaSet"}:                                               true,
	{Group: "apps", Kind: "StatefulSet"}:                                              true,
	{Group: "authentication.k8s.io", Kind: "TokenReview"}:                             false,
	{Group: "authorization.k8s.io", Kind: "LocalSubjectAccessReview"}:                 true,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectAccessReview"}:                  false,
	{Group: "authorization.k8s.io", Kind: "SelfSubjectRulesReview"}:                   false,
	{Group: "authorization.k8s.io", Kind: "SubjectAccessReview"}:                      false,
	{Group: "autoscaling", Kind: "HorizontalPodAutoscaler"}:                           true,
	{Group: "autoscaling", Kind: "Scale"}:                                             true,
	{Group: "batch", Kind: "CronJob"}:                                                 true,
	{Group: "batch", Kind: "Job"}:                                                     true,
	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}:                 false,
	{Group: "coordination.k8s.io", Kind: "Lease"}:                                     true,
	{Group: "discovery.k8s.io", Kind: "EndpointSlice"}:                                true,
	{Group: "events.k8s.io", Kind: "Event"}:                                           true,
	{Group: "networking.k8s.io", Kind: "IngressClass"}:                                false,
	{Group: "networking.k8s.io", Kind: "Ingress"}:                                     true,
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}:                               true,
	{Group: "node.k8s.io", Kind: "RuntimeClass"}:                                      false,
	{Group: "policy", Kind: "PodDisruptionBudget"}:                                    true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}:                  false,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:                         false,
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}:                         true,
	{Group: "rbac.authorization.k8s.io", Kind: "Role"}:                                true,
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}:                               false,
	{Group: "storage.k8s.io", Kind: "CSIDriver"}:                                      false,
	{Group: "storage.k8s.io", Kind: "CSINode"}:                                        false,
	{Group: "storage.k8s.io", Kind: "StorageClass"}:                                   false,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:                               false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"}:          false,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicyBinding"}:   false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"}:        false,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"}: false,
	{Group: "apiregistration.k8s.io", Kind: "APIService"}:                             false,
	{Group: "authentication.k8s.io", Kind: "SelfSubjectReview"}:                       false,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"}:                       false,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"}:       false,
	{Group: "networking.k8s.io", Kind: "IPAddress"}:                                   false,
	{Group: "networking.k8s.io", Kind: "ServiceCIDR"}:                                 false,
	{Group: "resource.k8s.io", Kind: "DeviceClass"}:                                   false,
	{Group: "resource.k8s.io", Kind: "ResourceClaim"}:                                 true,
	{Group: "resource.k8s.io", Kind: "ResourceClaimTemplate"}:                         true,
	{Group: "resource.k8s.io", Kind: "ResourceSlice"}:                                 false,
	{Group: "storage.k8s.io", Kind: "CSIStorageCapacity"}:                             true,
	{Group: "storage.k8s.io", Kind: "VolumeAttributesClass"}:                          false,
}

// IsGroupKindNamespaced check the kind is namespace scoped or not.
func IsGroupKindNamespaced(kind metav1.GroupKind) (bool, error) {
	namespaced, ok := groupKindNamespaced[kind]
	if !ok {
		return false, fmt.Errorf("unrecognized kind: %+v", kind)
	}
	return namespaced, nil
}

// IsNativeAPIGroup reports whether an API group is one of Kubernetes' own.
//
// A tenant's custom resource groups are stored upstream with the tenant prefix
// while native groups are not, so anything that has only a group name to work
// with -- an access review's resourceAttributes, say, where there is no kind to
// look up -- needs to tell the two apart before deciding to prefix.
//
// The set is derived from the scope table rather than listed again, so a group
// added there is native here too and cannot drift.
func IsNativeAPIGroup(group string) bool {
	return nativeAPIGroups[group]
}

var nativeAPIGroups = func() map[string]bool {
	groups := make(map[string]bool, len(groupKindNamespaced))
	for groupKind := range groupKindNamespaced {
		groups[groupKind.Group] = true
	}
	// Served by the CRD handler rather than the generic proxy, so it has no
	// entry in the scope table, and it is unmistakably native.
	groups["apiextensions.k8s.io"] = true
	return groups
}()

// TenantIDFrom returns tenantID from ctx.
func TenantIDFrom(ctx context.Context) string {
	tenantExtra := "tenant"
	user, ok := request.UserFrom(ctx)
	if !ok {
		return ""
	}
	if len(user.GetExtra()[tenantExtra]) > 0 {
		return user.GetExtra()[tenantExtra][0]
	}
	return ""
}

// ListCRDsForTenant returns the CRDs belonged to the tenant.
func ListCRDsForTenant(tenantID string, crdLister v1.CustomResourceDefinitionLister) ([]*extensionsv1.CustomResourceDefinition, error) {
	crdList, err := crdLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	tenantCRDs := make([]*extensionsv1.CustomResourceDefinition, 0, len(crdList))
	for _, crd := range crdList {
		if UpstreamObjectBelongsToTenant(crd, tenantID, false) {
			tenantCRDs = append(tenantCRDs, crd)
		}
	}
	return tenantCRDs, nil
}

// CheckGroupKindFunc returns whether resource of the group/kind is namespaced and whether it is custom resource group for the tenant.
type CheckGroupKindFunc func(group, kind, tenantID string, isTenantObject bool) (namespaced, customResourceGroup bool, err error)

// NewCheckGroupKindFunc returns a check function to check the group/kind type.
func NewCheckGroupKindFunc(crdLister v1.CustomResourceDefinitionLister) CheckGroupKindFunc {
	return func(group, kind, tenantID string, isTenantObject bool) (namespaced, customResourceGroup bool, err error) {
		// native group/kind
		namespaced, err = IsGroupKindNamespaced(metav1.GroupKind{Group: group, Kind: kind})
		if err == nil {
			return namespaced, false, nil
		}

		crdList, err := ListCRDsForTenant(tenantID, crdLister)
		if err != nil {
			return false, false, err
		}

		// isTenantObject says which form the group is already in. A reference on
		// its way in from the tenant names the group as the tenant sees it, so
		// the prefix has to be added before looking it up; one on its way back
		// out carries the upstream group, which is prefixed already. Adding the
		// prefix in both directions is what this used to do, and on the way out
		// it produced 111111-111111-example.com, matched nothing, and fell into
		// the branch below -- which reported the tenant's own CRD as a platform
		// one, so the group was never trimmed and the tenant read its own
		// ownerReferences back as 111111-example.com/v1. Measured.
		lookupGroup := group
		if isTenantObject {
			lookupGroup = AddTenantIDPrefix(tenantID, group)
		}
		for _, crd := range crdList {
			if crd.Spec.Group == lookupGroup && crd.Spec.Names.Kind == kind {
				return crd.Spec.Scope == extensionsv1.NamespaceScoped, true, nil
			}
		}

		// A group that is not one of this tenant's may still be a real CRD the
		// platform installed. A platform controller can stamp such a reference
		// onto a tenant's object, and the tenant has to be able to read that
		// object back, so resolve it on the way out -- as a platform object,
		// whose name and group are not prefixed and must not be trimmed.
		//
		// Deliberately not on the way in. Sharing a platform CRD with a tenant
		// is not implemented (see docs/faq.md), and accepting a tenant-authored
		// reference to one would both contradict that and answer the question
		// "does this CRD exist upstream" for any group the tenant cares to name.
		if !isTenantObject {
			allCRDs, err := crdLister.List(labels.Everything())
			if err != nil {
				return false, false, err
			}
			for _, crd := range allCRDs {
				if crd.Spec.Group == group && crd.Spec.Names.Kind == kind {
					return crd.Spec.Scope == extensionsv1.NamespaceScoped, false, nil
				}
			}
		}

		return false, false, fmt.Errorf("unregistered crd group: %s, kind: %s", lookupGroup, kind)
	}
}

// TenantFrom returns the value of the tenant info on the ctx.
func TenantFrom(ctx context.Context) (string, bool) {
	tenantExtra := "tenant"
	user, ok := request.UserFrom(ctx)
	if !ok {
		return "", false
	}
	if len(user.GetExtra()[tenantExtra]) > 0 {
		return user.GetExtra()[tenantExtra][0], true
	}
	return "", false
}

// ConvertInternalListOptions converts internal versions to v1 version.
func ConvertInternalListOptions(ctx context.Context, options *metainternalversion.ListOptions, tenantID string) (*metav1.ListOptions, error) {
	var err error
	out := &metav1.ListOptions{}
	if options.FieldSelector != nil {
		fn := func(label, value string) (string, string, error) {
			if label == "involvedObject.namespace" && value != "" && tenantID != "" {
				value = tenantID + "-" + value
			}
			return label, value, nil
		}
		if options.FieldSelector, err = options.FieldSelector.Transform(fn); err != nil {
			err = errors.NewBadRequest(err.Error())
			return nil, err
		}
	}
	if err = metainternalversion.Convert_internalversion_ListOptions_To_v1_ListOptions(options, out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// FilterUnstructuredList filter the unstructures not belonged to the tenant
func FilterUnstructuredList(utdList *unstructured.UnstructuredList, tenantID string, isNamespaceScoped bool) *unstructured.UnstructuredList {
	filtered := &unstructured.UnstructuredList{
		Object: utdList.Object,
		Items:  make([]unstructured.Unstructured, 0),
	}
	for i := range utdList.Items {
		if UpstreamObjectBelongsToTenant(&utdList.Items[i], tenantID, isNamespaceScoped) {
			filtered.Items = append(filtered.Items, utdList.Items[i])
		}
	}
	return filtered
}

type FakeCRDLister struct {
	Crds []*apiextensionsv1.CustomResourceDefinition
}

func (l *FakeCRDLister) List(selector labels.Selector) (ret []*apiextensionsv1.CustomResourceDefinition, err error) {
	return l.Crds, nil
}

func (l *FakeCRDLister) Get(name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	return nil, nil
}

// GetGVR returns the corresponding GVR for the given APIResource.
func GetGVR(rsrc metav1.APIResource) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    rsrc.Group,
		Version:  rsrc.Version,
		Resource: rsrc.Name,
	}
}

// IsCRD checks if the given APIResource is the CRD.
func IsCRD(r metav1.APIResource) bool {
	return r.Group == "apiextensions.k8s.io" &&
		r.Name == "customresourcedefinitions"
}

// FlattenResourceLists flattens the given nested list and return a list of resources.
func FlattenResourceLists(resourceLists []*metav1.APIResourceList) (ret []metav1.APIResource) {
	for _, resourceList := range resourceLists {
		for _, resource := range resourceList.APIResources {
			group, version := splitGV(resourceList.GroupVersion)
			resource.Group = group
			resource.Version = version
			ret = append(ret, resource)
		}
	}
	return
}

// splitGV splits the GroupVersion string (group/version).
func splitGV(GV string) (group, version string) {
	tks := strings.Split(GV, "/")
	if len(tks) < 1 {
		return
	}
	// i.e., v1 -> core/v1
	version = tks[0]
	if len(tks) < 2 {
		return
	}
	group, version = tks[0], tks[1]
	return
}

// ContainString checks if the slice contains the string.
func ContainString(sli []string, s string) bool {
	for _, ts := range sli {
		if ts == s {
			return true
		}
	}
	return false
}

// RemoveString removes the string from the slice, if found.
func RemoveString(sli []string, s string) (ret []string) {
	for i, ts := range sli {
		if ts == s {
			sli = append(sli[:i], sli[i+1:]...)
		}
	}
	ret = sli
	return
}

const (
	// proxiedGroupPrefix begins the name of the group kubezoo asserts, and only
	// kubezoo asserts, when it forwards a tenant's request upstream.
	proxiedGroupPrefix = "kubezoo:proxied:"

	// kubezooGroupPrefix is the group namespace kubezoo owns. Nothing arriving
	// at the front door may carry a group under it, whoever issued the
	// credential, because these names are how kubezoo tells upstream that it
	// forwarded a request itself.
	kubezooGroupPrefix = "kubezoo:"

	// RoleAuthorGroup carries the one permission a tenant cannot be given
	// directly: escalate on clusterroles.
	//
	// A tenant holds its namespaced permissions per namespace, so RBAC's
	// escalation check -- "do you hold, at cluster scope, everything this role
	// grants?" -- refuses every ClusterRole an operator chart ships, even though
	// the answer to the question that means something here, "do you hold it in
	// all of your namespaces?", is yes. Measured: a ClusterRole over pods,
	// secrets, configmaps and events is refused outright.
	//
	// escalate is the documented exemption from that check, and it is all this
	// group grants. The verbs that write the object still come from the tenant's
	// own role, so a suspended tenant gains nothing here, and the group on its
	// own is worth nothing to anybody.
	//
	// It is asserted only on writes to clusterroles. Not on clusterrolebindings,
	// and that is what keeps it contained: a ClusterRole grants nothing until it
	// is bound, a RoleBinding in the tenant's own namespace binds it to that
	// namespace alone -- which the tenant could already do -- and binding it
	// cluster-wide still faces the escalation check with no exemption, because
	// this group does not cover it and the tenant holds neither bind nor
	// escalate on clusterrolebindings.
	RoleAuthorGroup = "kubezoo:role-author"
)

// TenantAdminUser is the upstream identity kubezoo impersonates for a tenant's
// own credential. pkg/dynamic sets the impersonation headers on every forwarded
// request, so upstream RBAC is evaluated against this user rather than against
// kubezoo's, which is what makes upstream RBAC mean anything at all.
func TenantAdminUser(tenantID string) string {
	return tenantID + "-admin"
}

// ProxiedGroup names the group that carries a tenant's cluster-scoped
// permissions upstream.
//
// Those permissions used to belong to TenantAdminUser directly, which made them
// usable by anything holding that identity, whether or not it arrived through
// kubezoo. It matters because a cluster-scoped grant cannot be bounded by name:
// RBAC's resourceNames is an exact match with no prefix form, so a grant on
// ingressclasses or customresourcedefinitions is a grant on every tenant's.
// Measured as 111111-admin: list returns 222222's objects, and delete on
// 222222-nginx is allowed.
//
// Holding them in a group makes them conditional on having been forwarded,
// because a group is asserted by whoever authorized the impersonation and
// cannot be presented by the tenant. What kubezoo permits is unchanged; what
// the same identity permits outside kubezoo becomes nothing.
//
// Per tenant rather than one shared group: the roles differ -- each carries
// that tenant's own CRD groups -- so a shared group would hand every tenant
// every other tenant's role, which is the problem again.
//
// ⚠️ Deployment requirement: no credential issued to a tenant may carry this
// group. Tenant certificates are signed by kubezoo's own CA from a CSR template
// that fixes the subject, so this holds as long as tenants cannot choose their
// own organization. Kubezoo drops the group from anything arriving at its front
// door, which catches a mis-issued credential there, but a credential upstream
// trusts is beyond its reach.
func ProxiedGroup(tenantID string) string {
	return proxiedGroupPrefix + tenantID
}

// ImpersonationGroups returns the groups kubezoo asserts upstream for a request
// it is forwarding.
//
// A proxied group already on the incoming identity is dropped rather than
// passed on. Kubezoo issues that name itself, so one arriving means a credential
// was minted carrying it, and forwarding it would let one tenant's credential
// ask for another tenant's role.
//
// The group is added only for the tenant's own credential, the identity that
// held these permissions before. A tenant's ServiceAccounts reach upstream as
// themselves and hold only what the tenant granted them per namespace; adding
// the group for them would quietly turn every workload into a cluster-scoped
// one.
func ImpersonationGroups(ctx context.Context, userName string, groups []string) []string {
	tenantID := TenantIDFrom(ctx)
	forwarded := make([]string, 0, len(groups)+2)
	for _, group := range groups {
		if strings.HasPrefix(group, kubezooGroupPrefix) {
			klog.Warningf("dropping group %q from incoming identity %q: kubezoo issues names under "+
				"%q itself, so a credential carrying one was mis-issued", group, userName, kubezooGroupPrefix)
			continue
		}
		forwarded = append(forwarded, group)
	}
	if tenantID == "" || userName != TenantAdminUser(tenantID) {
		return forwarded
	}
	forwarded = append(forwarded, ProxiedGroup(tenantID))
	if writesAClusterRole(ctx) {
		forwarded = append(forwarded, RoleAuthorGroup)
	}
	return forwarded
}

// writesAClusterRole reports whether this request is a tenant writing a
// ClusterRole, which is the only place the escalate exemption is asserted.
//
// Deliberately not clusterrolebindings: see RoleAuthorGroup. Reads do not need
// it either -- the check runs on the way in, not on the way out.
func writesAClusterRole(ctx context.Context) bool {
	info, ok := request.RequestInfoFrom(ctx)
	if !ok || !info.IsResourceRequest {
		return false
	}
	if info.APIGroup != "rbac.authorization.k8s.io" || info.Resource != "clusterroles" {
		return false
	}
	switch info.Verb {
	case "create", "update", "patch":
		return true
	}
	return false
}

// ReservedClusterRoleNames are the upstream names kubezoo keeps for itself, per
// tenant, in the RBAC group.
//
// They collide with what a tenant produces: names of cluster-scoped objects get
// the tenant prefix, so a tenant creating a ClusterRole called cluster-admin
// addresses <tid>-cluster-admin -- which is the very role the controller creates
// and binds cluster-wide to that tenant. Measured: with escalate granted, a
// tenant overwrote it with star-on-star and reached kube-system's and another
// tenant's secrets. Without escalate it can still delete it, which is self-harm
// the controller repairs, but the collision is what turns any future privilege
// on this path into an escape.
func ReservedClusterRoleNames(tenantID string) []string {
	return []string{
		tenantID + "-cluster-admin",
		tenantID + "-admin",
	}
}

// IsReservedClusterName reports whether an upstream name is one kubezoo manages
// for this tenant and the tenant must not write.
func IsReservedClusterName(tenantID, upstreamName string) bool {
	for _, reserved := range ReservedClusterRoleNames(tenantID) {
		if upstreamName == reserved {
			return true
		}
	}
	return false
}

// projectedBindingPrefix begins the name of every RoleBinding that carries a
// tenant's ClusterRoleBinding.
//
// The name has to differ from anything the tenant can write, because the
// projections live in the same namespaces as the tenant's own RoleBindings and
// a collision would let a tenant overwrite one, or forge one. Namespaced
// objects do not get a tenant prefix -- only their namespace does -- so the
// separation has to come from the name itself. Writes to it are refused; see
// tenantProxy.refuseReservedName.
const projectedBindingPrefix = "kubezoo:clusterrolebinding:"

// ProjectedBindingName is the RoleBinding name carrying a ClusterRoleBinding.
func ProjectedBindingName(clusterRoleBindingName string) string {
	return projectedBindingPrefix + clusterRoleBindingName
}

// TrimProjectedBindingName recovers the ClusterRoleBinding name.
func TrimProjectedBindingName(roleBindingName string) string {
	return strings.TrimPrefix(roleBindingName, projectedBindingPrefix)
}

// IsProjectedBindingName reports whether a RoleBinding name is one of these.
func IsProjectedBindingName(roleBindingName string) bool {
	return strings.HasPrefix(roleBindingName, projectedBindingPrefix)
}

// IsManagedBindingName reports whether a RoleBinding is one kubezoo keeps in a
// tenant's namespaces on its behalf, rather than one the tenant wrote.
//
// Two kinds: the per-namespace binding that grants the tenant its namespace, and
// the projections carrying its ClusterRoleBindings. Both live in the tenant's
// namespaces and neither is the tenant's to read or write, so kubezoo owns the
// whole "kubezoo:" name space for RoleBindings -- which is also why a tenant's
// own names, unprefixed because only namespaces carry the tenant prefix, cannot
// collide with them.
func IsManagedBindingName(roleBindingName string) bool {
	return strings.HasPrefix(roleBindingName, kubezooGroupPrefix)
}

// applyForceKey marks a server-side apply that asked to take fields from other
// managers.
type applyForceKey struct{}

// WithApplyForce records the force flag of an apply request.
//
// It is a query parameter, and the storage layer is handed metav1.UpdateOptions,
// which has no field for it -- so without carrying it here an apply forwarded
// upstream would lose the one thing that decides whether a conflict stops it.
func WithApplyForce(ctx context.Context, force bool) context.Context {
	return context.WithValue(ctx, applyForceKey{}, force)
}

// ApplyForceFrom reports whether this apply asked to force.
func ApplyForceFrom(ctx context.Context) bool {
	force, ok := ctx.Value(applyForceKey{}).(bool)
	return ok && force
}
