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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	core "k8s.io/kubernetes/pkg/apis/core"
)

// refuseTenantWrittenPodAddresses stops a tenant writing the address fields of
// its own pod's status.
//
// ⛔ THIS IS WHAT MADE THE ENDPOINT GUARD CIRCULAR. kubezoo serves pods/status
// to tenants (cmd/kubezoo/app/apigroups.go), and a tenant is "*" on "*" inside
// its own namespaces, so it can write status.podIPs. refuseForgedEndpointAddress
// then validates an endpoint address BY READING THAT SAME FIELD -- a
// tenant-written value checked against a tenant-written value, which passes by
// construction. The guard shipped that way and this is the half that was missing.
//
// ⭐ And the forged address is a reachable surface on its own, not only through
// endpoints: the apiserver's pods/proxy subresource dials status.podIP directly
// (pkg/registry/core/pod/strategy.go getPodIP), filtering nothing but
// !IsGlobalUnicast -- from the control plane's own network namespace, which no
// tenant NetworkPolicy or OVN construct reaches.
//
// ⚠️ NOT a blanket refusal of pods/status, and that distinction is the whole
// design. A readiness gate is a tenant's controller writing status.conditions --
// a documented, legitimate use of exactly this subresource. Refusing the
// subresource would take that away to close a hole in four fields. So the
// comparison is field by field against what is stored: conditions stay writable,
// the addresses do not.
//
// The fields here are the ones kubelet and the scheduler own. A tenant changing
// any of them is not describing its pod, it is choosing what something else will
// dial.
func (tp *tenantProxy) refuseTenantWrittenPodAddresses(obj, old runtime.Object) error {
	if tp.subresource != "status" {
		return nil
	}
	pod, ok := obj.(*core.Pod)
	if !ok {
		return nil
	}
	oldPod, ok := old.(*core.Pod)
	if !ok {
		// ⚠️ Nothing stored to compare against. A status write to an object that
		// does not exist is not a path a tenant can use to forge an address --
		// guaranteedUpdate hands the missing case to Create, and a pod Create has
		// its status dropped by upstream's strategy. Refusing here instead would
		// fail that legitimate create.
		return nil
	}

	changed := func(path string, a, b interface{}) error {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "", Kind: "Pod"}, pod.Name,
			field.ErrorList{field.Forbidden(
				field.NewPath("status", path),
				"a pod's addresses are written by the kubelet and the scheduler, not by its "+
					"owner: something else decides what to connect to on the strength of them "+
					"-- the apiserver's pod proxy dials this address directly, and an endpoint "+
					"naming it is checked against it",
			)})
	}
	// ⚠️ No scalar podIP here: the INTERNAL PodStatus of the k8s version this
	// builds against (v1.36.3) carries PodIPs alone, and the versioned type's
	// podIP is kept in sync from PodIPs[0] by conversion. Reading the local
	// /root/kubernetes checkout instead would have said otherwise -- it is v1.37
	// beta, where the field exists. Cite the module that compiles, not the
	// checkout that is handy.
	if !samePodIPs(pod.Status.PodIPs, oldPod.Status.PodIPs) {
		return changed("podIPs", pod.Status.PodIPs, oldPod.Status.PodIPs)
	}
	if pod.Status.HostIP != oldPod.Status.HostIP {
		return changed("hostIP", pod.Status.HostIP, oldPod.Status.HostIP)
	}
	if !sameHostIPs(pod.Status.HostIPs, oldPod.Status.HostIPs) {
		return changed("hostIPs", pod.Status.HostIPs, oldPod.Status.HostIPs)
	}
	// ⚠️ nominatedNodeName is the scheduler's, and a tenant setting it is choosing
	// placement by another name -- which refuseTenantChosenNode exists to stop on
	// the spec side.
	if pod.Status.NominatedNodeName != oldPod.Status.NominatedNodeName {
		return changed("nominatedNodeName", pod.Status.NominatedNodeName, oldPod.Status.NominatedNodeName)
	}
	return nil
}

func samePodIPs(a, b []core.PodIP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP {
			return false
		}
	}
	return true
}

func sameHostIPs(a, b []core.HostIP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP {
			return false
		}
	}
	return true
}
