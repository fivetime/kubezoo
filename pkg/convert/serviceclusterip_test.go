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

package convert

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	core "k8s.io/kubernetes/pkg/apis/core"
)

func svcWith(clusterIP string, annotations map[string]string) *core.Service {
	s := &core.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "rsvc", Namespace: "111111-default", Annotations: annotations},
		Spec:       core.ServiceSpec{ClusterIP: clusterIP},
	}
	if clusterIP != "" && clusterIP != core.ClusterIPNone {
		s.Spec.ClusterIPs = []string{clusterIP}
	}
	return s
}

const (
	upstreamIP = "254.51.140.34" // the upstream cluster's service CIDR
	tenantIP   = "192.168.200.7" // the data plane's VIP on the tenant network
)

// TestTenantSeesTheAddressItCanReach is the whole point: the upstream ClusterIP
// comes from a service CIDR that does not exist on the tenant's own network, so
// reporting it hands the tenant an address none of its workloads can dial.
func TestTenantSeesTheAddressItCanReach(t *testing.T) {
	svc := svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: tenantIP})
	got, ok := TenantClusterIP(svc)
	if !ok || got != tenantIP {
		t.Errorf("TenantClusterIP = %q,%v; want %q,true", got, ok, tenantIP)
	}
}

// TestHeadlessStaysHeadless -- "None" is the tenant's own words, usually a
// StatefulSet's governing Service. Overwriting it with a single address would
// turn per-pod DNS into one address for a workload that specifically asked not
// to have one, and nothing would report the change.
func TestHeadlessStaysHeadless(t *testing.T) {
	svc := svcWith(core.ClusterIPNone, map[string]string{ClusterIPAnnotation: tenantIP})
	if got, ok := TenantClusterIP(svc); ok {
		t.Errorf("a headless Service was given the address %q; per-pod DNS becomes a "+
			"single address and the tenant is never told", got)
	}
}

// TestNoAnnotationReportsUpstream -- the data plane fills the annotation in
// shortly after the Service is created. Reporting nothing during that window
// would invent a state stock Kubernetes never produces, a ClusterIP Service with
// no address, which client code has no branch for.
func TestNoAnnotationReportsUpstream(t *testing.T) {
	for name, svc := range map[string]*core.Service{
		"no annotations at all": svcWith(upstreamIP, nil),
		"annotation empty":      svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: ""}),
		"annotation not an IP":  svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: "pending"}),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := TenantClusterIP(svc); ok {
				t.Errorf("reported %q; the upstream address has to stand until the data "+
					"plane has published one", got)
			}
		})
	}
}

// The two cases that used to live here -- a tenant cannot write the annotation,
// and the platform's stored value survives a tenant write -- moved to
// hiddenmeta_test.go when the rule stopped being specific to this one key. See
// TestATenantCannotSetAHiddenKey and TestAReadModifyWriteDoesNotEraseThePlatform.

// TestApplyRoundTripIsStable is the case a tenant hits by accident, constantly.
//
// ⭐ `kubectl get svc -o yaml | kubectl apply -f -`. The tenant is shown the
// data plane's address, so that is what it sends back, and upstream would see
// its own ClusterIP changing and refuse with "may not change once set" -- naming
// a field the tenant never knowingly touched.
func TestApplyRoundTripIsStable(t *testing.T) {
	old := svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: tenantIP})
	submitted := svcWith(tenantIP, nil)
	if !RestoreUpstreamClusterIP(submitted, old) {
		t.Fatal("a round-tripped Service was refused")
	}
	if submitted.Spec.ClusterIP != upstreamIP {
		t.Errorf("clusterIP = %q, want the upstream %q", submitted.Spec.ClusterIP, upstreamIP)
	}
	// ⚠️ clusterIPs travels with clusterIP and upstream validates them against
	// each other. Translating one and not the other is a rejected write in the
	// dual-stack case and a silently inconsistent object in the single-stack one.
	if len(submitted.Spec.ClusterIPs) != 1 || submitted.Spec.ClusterIPs[0] != upstreamIP {
		t.Errorf("clusterIPs = %v, want [%s]", submitted.Spec.ClusterIPs, upstreamIP)
	}
}

// TestSubmittingTheUpstreamAddressIsAlsoFine -- a tenant that somehow holds the
// upstream address, or a controller replaying an object read before the data
// plane published, must not be refused: that value is what storage holds.
func TestSubmittingTheUpstreamAddressIsAlsoFine(t *testing.T) {
	old := svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: tenantIP})
	submitted := svcWith(upstreamIP, nil)
	if !RestoreUpstreamClusterIP(submitted, old) {
		t.Error("the address upstream actually holds was refused")
	}
}

// TestAThirdAddressIsRefused -- the remaining case. It cannot be honoured, since
// the address comes from the data plane's allocator rather than anything kubezoo
// can ask for, and accepting it silently would leave the tenant believing it had
// chosen.
func TestAThirdAddressIsRefused(t *testing.T) {
	old := svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: tenantIP})
	if RestoreUpstreamClusterIP(svcWith("10.9.9.9", nil), old) {
		t.Error("a tenant named an address of its own and the write was allowed through")
	}
}

// TestHeadlessTransitionsAreLeftToUpstream -- changing a Service between
// headless and not is forbidden by Kubernetes, and that refusal has to stay
// upstream's rather than be silently absorbed here. This pins that this code
// neither performs the change nor pretends it succeeded.
func TestHeadlessTransitionsAreLeftToUpstream(t *testing.T) {
	old := svcWith(upstreamIP, map[string]string{ClusterIPAnnotation: tenantIP})
	toHeadless := svcWith(core.ClusterIPNone, nil)
	if RestoreUpstreamClusterIP(toHeadless, old) {
		t.Error("a ClusterIP Service was allowed to become headless without upstream seeing the change")
	}
	if toHeadless.Spec.ClusterIP != core.ClusterIPNone {
		t.Errorf("the submitted value was rewritten to %q, hiding the transition from "+
			"upstream's validation", toHeadless.Spec.ClusterIP)
	}

	oldHeadless := svcWith(core.ClusterIPNone, nil)
	stillHeadless := svcWith(core.ClusterIPNone, nil)
	if !RestoreUpstreamClusterIP(stillHeadless, oldHeadless) {
		t.Error("a headless Service updated in place was refused")
	}
}
