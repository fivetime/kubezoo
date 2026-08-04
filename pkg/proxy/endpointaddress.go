/*
Copyright 2024 The KubeZoo Authors.

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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	core "k8s.io/kubernetes/pkg/apis/core"
	discovery "k8s.io/kubernetes/pkg/apis/discovery"
)

var podGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// refuseForgedEndpointAddress stops a tenant putting an address into its own
// Service's endpoints that does not belong to one of its own pods.
//
// ⛔ SEVENTH SPEC, SEVENTH TIME ALL THREE LAYERS WERE EMPTY. The convertors
// translate targetRef and nothing else -- addresses, ports, addressType,
// conditions and the labels pass through untouched (pkg/convert/endpoints.go,
// pkg/convert/endpointslice.go). No policy in kubezoo-contract mentions
// endpoints. Upstream validates only the FORM of an address
// (pkg/apis/core/validation ValidateEndpointIP rejects unspecified, loopback and
// link-local and nothing else), so a node IP, a ClusterIP, another tenant's pod
// IP or any routable address off the cluster all pass.
//
// ⭐ Cross-tenant is contained, and by something outside those three layers: the
// binding is keyed on the SLICE'S OWN NAMESPACE plus the service-name label, in
// every consumer -- kube-proxy, the apiserver's resolvers, services/proxy, the
// aggregator. kubezoo prefixes that namespace unconditionally, so a tenant can
// only ever attach endpoints to a Service of its own. This guard is NOT what
// stops cross-tenant attachment; do not weaken the namespace prefixing on the
// strength of it.
//
// ⛔ What is NOT contained is who follows the address. An ingress controller
// connects to endpoint IPs DIRECTLY, bypassing the ClusterIP -- and nginx is a
// published ingress class here. A metrics scraper follows them with a bearer
// token. Those are platform components dialling a tenant-controlled address:
// the same shape as the kubetron authURL and the ExternalName resolver, and the
// reason this is a guard rather than a note.
//
// ⭐ The check is upstream's own, moved earlier. services/proxy already refuses
// exactly this, in isValidAddress
// (pkg/registry/core/service/storage/storage.go): targetRef present, its
// namespace matching, the pod existing, and the address among that pod's IPs.
// It is the one check in the whole tree aimed at this field, and it protects one
// caller. Applying it at the write makes the forged address not exist.
//
// ⚠️ It costs a pod GET per endpoint written, and that is affordable HERE and
// nowhere else: a tenant writing endpoints by hand is rare, and the
// EndpointSlice controller's own writes never pass through kubezoo. A
// cluster-wide pod informer was declined for the quota work for good reasons;
// this is not that.
func (tp *tenantProxy) refuseForgedEndpointAddress(ctx context.Context, obj runtime.Object) error {
	switch typed := obj.(type) {
	case *core.Endpoints:
		for i := range typed.Subsets {
			for j := range typed.Subsets[i].Addresses {
				a := &typed.Subsets[i].Addresses[j]
				p := field.NewPath("subsets").Index(i).Child("addresses").Index(j)
				if err := tp.addressBelongsToAPod(ctx, a.IP, a.TargetRef, typed.Namespace, p, "Endpoints", typed.Name); err != nil {
					return err
				}
			}
			// notReadyAddresses reach the same consumers the moment a readiness
			// probe flips; treating them as harmless is how a check gets bypassed
			// by writing the address into the other list.
			for j := range typed.Subsets[i].NotReadyAddresses {
				a := &typed.Subsets[i].NotReadyAddresses[j]
				p := field.NewPath("subsets").Index(i).Child("notReadyAddresses").Index(j)
				if err := tp.addressBelongsToAPod(ctx, a.IP, a.TargetRef, typed.Namespace, p, "Endpoints", typed.Name); err != nil {
					return err
				}
			}
		}
	case *discovery.EndpointSlice:
		for i := range typed.Endpoints {
			e := &typed.Endpoints[i]
			for j := range e.Addresses {
				p := field.NewPath("endpoints").Index(i).Child("addresses").Index(j)
				if err := tp.addressBelongsToAPod(ctx, e.Addresses[j], e.TargetRef, typed.Namespace, p, "EndpointSlice", typed.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (tp *tenantProxy) addressBelongsToAPod(ctx context.Context, address string,
	targetRef *core.ObjectReference, namespace string, path *field.Path, kind, name string) error {

	refuse := func(detail string) error {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "discovery.k8s.io", Kind: kind}, name,
			field.ErrorList{field.Forbidden(path, detail)})
	}
	if address == "" {
		return nil
	}
	// ⚠️ An endpoint with no targetRef passes through the convertors completely
	// untouched -- objectreference.go returns immediately on nil -- so this is
	// the shape a forged address actually takes.
	if targetRef == nil || targetRef.Kind != "Pod" || targetRef.Name == "" {
		return refuse(fmt.Sprintf("address %q names no pod: an endpoint must carry a "+
			"targetRef to a Pod of yours, because the address is what the platform's "+
			"ingress and metrics components connect to", address))
	}
	// The object is already in upstream form here, so its namespace is the
	// tenant's prefixed one and the reference has been prefixed alongside it.
	if targetRef.Namespace != "" && targetRef.Namespace != namespace {
		return refuse(fmt.Sprintf("targetRef names namespace %q, which is not this object's",
			targetRef.Namespace))
	}
	pod, err := tp.dynamicClient.Resource(podGVR).Namespace(namespace).
		Get(ctx, targetRef.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return refuse(fmt.Sprintf("targetRef names pod %q, which does not exist", targetRef.Name))
		}
		return err
	}
	ips, _, err := unstructured.NestedSlice(pod.Object, "status", "podIPs")
	if err != nil {
		return err
	}
	for _, raw := range ips {
		if m, ok := raw.(map[string]interface{}); ok {
			if ip, _ := m["ip"].(string); ip == address {
				return nil
			}
		}
	}
	// A pod with no IP yet is not a forgery; it is a race with the scheduler.
	if len(ips) == 0 {
		return nil
	}
	return refuse(fmt.Sprintf("address %q is not one of pod %q's addresses", address, targetRef.Name))
}
