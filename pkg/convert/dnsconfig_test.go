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
	"k8s.io/apimachinery/pkg/runtime"
	apps "k8s.io/kubernetes/pkg/apis/apps"
	batch "k8s.io/kubernetes/pkg/apis/batch"
	core "k8s.io/kubernetes/pkg/apis/core"
)

// testTenant and deploymentIn come from the other tests in this package.

func fixedDNS(ip string) TenantDNSFunc {
	return func(string) (TenantDNS, bool) {
		return TenantDNS{Nameserver: ip, ClusterDomain: "cluster.local"}, true
	}
}

// TestTemplateSearchListIsTheTENANTsNamespace is the whole point of the change.
//
// ⭐ The pod runs in the upstream namespace 111111-default, and kubelet would
// have built its search list from that -- which is why "rsvc.default" does not
// resolve today and "rsvc.111111-default.svc.cluster.local" does. The search
// list written here has to name the namespace the TENANT knows, or short names
// keep resolving under the leaked name and nothing about the change is visible.
func TestTemplateSearchListIsTheTENANTsNamespace(t *testing.T) {
	d := deploymentIn(testTenant + "-default")
	tr := NewDNSConfigTransformer(fixedDNS("10.9.9.9"))
	if _, err := tr.Forward(d, testTenant); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	spec := d.Spec.Template.Spec
	if spec.DNSPolicy != core.DNSNone {
		t.Errorf("dnsPolicy = %q, want None -- without None kubelet keeps prepending the platform resolver", spec.DNSPolicy)
	}
	if spec.DNSConfig == nil {
		t.Fatal("no dnsConfig was written")
	}
	if got := spec.DNSConfig.Nameservers; len(got) != 1 || got[0] != "10.9.9.9" {
		t.Errorf("nameservers = %v, want [10.9.9.9]", got)
	}
	want := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	if len(spec.DNSConfig.Searches) != len(want) {
		t.Fatalf("searches = %v, want %v", spec.DNSConfig.Searches, want)
	}
	for i := range want {
		if spec.DNSConfig.Searches[i] != want[i] {
			t.Errorf("searches[%d] = %q, want %q -- an upstream-named entry here is the leak this exists to remove",
				i, spec.DNSConfig.Searches[i], want[i])
		}
	}
	// ⚠️ Named explicitly. dnsPolicy: None means kubelet contributes nothing at
	// all, so an ndots left unset would silently become 1 and every short name
	// would go out as an absolute query.
	if len(spec.DNSConfig.Options) != 1 || spec.DNSConfig.Options[0].Name != "ndots" ||
		spec.DNSConfig.Options[0].Value == nil || *spec.DNSConfig.Options[0].Value != "5" {
		t.Errorf("options = %+v, want ndots=5", spec.DNSConfig.Options)
	}
}

// TestForwardLeavesALivePodAlone locks the CREATE-only rule.
//
// ⛔ dnsPolicy and dnsConfig are not in the set of pod spec fields an update may
// change. Writing them from the convertor -- which runs on every write -- would
// make upstream refuse every update to a pod created before this existed, and
// the tenant could no longer touch its own running pod. That is the same trap
// placement.go and saprojection.go each hit once already.
func TestDNSForwardLeavesALivePodAlone(t *testing.T) {
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-default"},
		Spec:       core.PodSpec{DNSPolicy: core.DNSClusterFirst},
	}
	tr := NewDNSConfigTransformer(fixedDNS("10.9.9.9"))
	if _, err := tr.Forward(pod, testTenant); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if pod.Spec.DNSPolicy != core.DNSClusterFirst || pod.Spec.DNSConfig != nil {
		t.Errorf("Forward rewrote a live pod (dnsPolicy=%q dnsConfig=%+v); "+
			"these fields are immutable on update, so this makes upstream refuse every "+
			"write to a pod that predates the change", pod.Spec.DNSPolicy, pod.Spec.DNSConfig)
	}
}

// TestSetPodDNSIsWhatCreateUses is the other half: the bare pod path must still
// get a resolver, or a tenant's `kubectl run` resolves differently from the same
// tenant's Deployment.
func TestSetPodDNSIsWhatCreateUses(t *testing.T) {
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-web"}}
	SetPodDNS(pod, testTenant, fixedDNS("10.9.9.9"))
	if pod.Spec.DNSPolicy != core.DNSNone || pod.Spec.DNSConfig == nil {
		t.Fatalf("SetPodDNS did not configure the pod: policy=%q config=%+v",
			pod.Spec.DNSPolicy, pod.Spec.DNSConfig)
	}
	if got := pod.Spec.DNSConfig.Searches[0]; got != "web.svc.cluster.local" {
		t.Errorf("searches[0] = %q, want web.svc.cluster.local -- the namespace must be the tenant's name for it", got)
	}
}

// TestBackwardKeepsTheInjectedConfig guards a fix that looks like a bug.
//
// ⚠️ Hiding a platform-injected field is the usual thing to do, and here it
// would break every read-modify-write: the tenant would send back a pod whose
// dnsPolicy differs from the stored one, and upstream refuses that. The same
// reasoning keeps saprojection.go's injected volume visible.
func TestBackwardKeepsTheInjectedConfig(t *testing.T) {
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testTenant + "-default"}}
	SetPodDNS(pod, testTenant, fixedDNS("10.9.9.9"))
	tr := NewDNSConfigTransformer(fixedDNS("10.9.9.9"))
	if _, err := tr.Backward(pod, testTenant); err != nil {
		t.Fatalf("Backward: %v", err)
	}
	if pod.Spec.DNSConfig == nil || pod.Spec.DNSPolicy != core.DNSNone {
		t.Error("Backward stripped the injected DNS settings; a tenant reading a pod and " +
			"applying it back would then send a dnsPolicy that differs from the stored one, " +
			"which upstream refuses -- turning every read-modify-write into a failure")
	}
}

// TestNoResolverLeavesThePodAlone locks the fail-open decision.
//
// ⭐ Between a tenant being created and its resolver being provisioned there is
// no answer to give. Writing dnsPolicy: None with an empty nameserver list would
// leave the pod with NO resolver at all -- strictly worse than the platform one
// it would otherwise keep.
func TestNoResolverLeavesThePodAlone(t *testing.T) {
	for name, fn := range map[string]TenantDNSFunc{
		"nil resolver":      nil,
		"not provisioned":   func(string) (TenantDNS, bool) { return TenantDNS{}, false },
		"no address":        func(string) (TenantDNS, bool) { return TenantDNS{ClusterDomain: "cluster.local"}, true },
		"no cluster domain": func(string) (TenantDNS, bool) { return TenantDNS{Nameserver: "10.9.9.9"}, true },
	} {
		t.Run(name, func(t *testing.T) {
			d := deploymentIn(testTenant + "-default")
			tr := NewDNSConfigTransformer(fn)
			if _, err := tr.Forward(d, testTenant); err != nil {
				t.Fatalf("Forward: %v", err)
			}
			spec := d.Spec.Template.Spec
			if spec.DNSPolicy != "" || spec.DNSConfig != nil {
				t.Errorf("pod was configured anyway: policy=%q config=%+v; "+
					"a nameserver-less dnsPolicy None gives the pod no resolver at all",
					spec.DNSPolicy, spec.DNSConfig)
			}
		})
	}
}

// TestEveryPodCarrierGetsItsResolver is the coverage half.
//
// ⚠️ A kind that carries a pod template but is missed here keeps the platform
// resolver, silently -- no error, nothing that fails to build, just a workload
// whose names resolve under the upstream namespace while everything else in the
// same tenant resolves under the tenant's. Mirrors the placement coverage test
// for the same reason.
func TestEveryPodCarrierGetsItsResolver(t *testing.T) {
	ns := testTenant + "-default"
	meta := metav1.ObjectMeta{Name: "x", Namespace: ns}
	// ⚠️ Pod is deliberately absent: Forward skips a live pod on purpose, and
	// TestForwardLeavesALivePodAlone is what covers it. Every OTHER carrier must
	// be here.
	samples := map[string]runtime.Object{
		"PodTemplate":           &core.PodTemplate{ObjectMeta: meta},
		"ReplicationController": &core.ReplicationController{ObjectMeta: meta, Spec: core.ReplicationControllerSpec{Template: &core.PodTemplateSpec{}}},
		"Deployment":            &apps.Deployment{ObjectMeta: meta},
		"StatefulSet":           &apps.StatefulSet{ObjectMeta: meta},
		"DaemonSet":             &apps.DaemonSet{ObjectMeta: meta},
		"ReplicaSet":            &apps.ReplicaSet{ObjectMeta: meta},
		"Job":                   &batch.Job{ObjectMeta: meta},
		"CronJob":               &batch.CronJob{ObjectMeta: meta},
	}
	tr := NewDNSConfigTransformer(fixedDNS("10.9.9.9"))
	for kind, obj := range samples {
		if _, err := tr.Forward(obj, testTenant); err != nil {
			t.Fatalf("Forward(%s): %v", kind, err)
		}
		spec, err := podSpecOf(obj)
		if err != nil || spec == nil {
			t.Fatalf("%s carries no reachable pod template: %v", kind, err)
		}
		if spec.DNSConfig == nil {
			t.Errorf("%s did not get a resolver; its pods keep resolving under the upstream "+
				"namespace while the rest of the tenant resolves under the tenant's", kind)
		}
	}
	// ⚠️ A sample map that quietly fell behind PodCarryingKinds would leave this
	// green while covering less than it claims -- the exact failure the placement
	// coverage test exists to prevent, one kind over.
	if len(samples) != len(PodCarryingKinds)-1 {
		t.Fatalf("this test knows %d carriers besides Pod, PodCarryingKinds has %d; "+
			"a kind added to one and not the other is a kind whose pods get no resolver",
			len(samples), len(PodCarryingKinds)-1)
	}
}
