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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

func annotatedService() *core.Service {
	return &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rsvc", Namespace: "111111-default",
			Annotations: map[string]string{convert.ClusterIPAnnotation: "192.168.200.236"},
		},
		Spec: core.ServiceSpec{ClusterIP: "254.51.24.88", ClusterIPs: []string{"254.51.24.88"}},
	}
}

// TestAWriteIsBuiltOnTheStoredObject is the regression guard for a defect the
// unit tests on the translation itself could not have caught.
//
// ⛔ Every other translation on the read path is reversible -- a namespace
// prefix comes off and goes back on -- so it never mattered whether an internal
// read landed before or after them. spec.clusterIP is not reversible: the
// address shown to the tenant is one the data plane allocated, and the upstream
// address cannot be computed back from it.
//
// So reading the TENANT view before an update destroyed the value every write
// has to preserve. The guard compared the submitted address against an "old"
// that already carried the same translated address, concluded nothing had
// changed, and passed it upstream -- refused there with "may not change once
// set". Measured on the live cluster: EVERY update to such a Service failed,
// `kubectl annotate` included, not only the round-trip the translation was
// written for.
//
// ⚠️ convert.RestoreUpstreamClusterIP and its tests were correct throughout.
// The defect was in which read fed them, which is why this test is on the gate
// rather than on the translation.
func TestAWriteIsBuiltOnTheStoredObject(t *testing.T) {
	stored := annotatedService()
	(&tenantProxy{storageView: true}).applyTenantViews(stored)
	if stored.Spec.ClusterIP != "254.51.24.88" {
		t.Errorf("clusterIP = %q, want the stored address; a write applied on top of "+
			"this would carry the translated one back into storage", stored.Spec.ClusterIP)
	}
	if len(stored.Spec.ClusterIPs) != 1 || stored.Spec.ClusterIPs[0] != "254.51.24.88" {
		t.Errorf("clusterIPs = %v, want the stored address", stored.Spec.ClusterIPs)
	}
}

// TestATenantReadIsTranslated is the other half. A gate that never opens would
// disable the feature and look exactly like a passing test suite.
func TestATenantReadIsTranslated(t *testing.T) {
	shown := annotatedService()
	(&tenantProxy{}).applyTenantViews(shown)
	if shown.Spec.ClusterIP != "192.168.200.236" {
		t.Errorf("clusterIP = %q, want the data plane's address; the tenant is being "+
			"handed one its workloads cannot dial", shown.Spec.ClusterIP)
	}
	if len(shown.Spec.ClusterIPs) != 1 || shown.Spec.ClusterIPs[0] != "192.168.200.236" {
		t.Errorf("clusterIPs = %v, want the data plane's address", shown.Spec.ClusterIPs)
	}
}

// TestReadForUpdateAsksForTheStoredObject pins the single line whose absence
// caused the outage: readForUpdate has to ask for the storage view. Everything
// above is correct behaviour of a flag nothing sets.
func TestReadForUpdateAsksForTheStoredObject(t *testing.T) {
	for name, tp := range map[string]*tenantProxy{
		"main resource": {},
		"subresource":   {subresource: "status"},
	} {
		t.Run(name, func(t *testing.T) {
			target := tp.forUpdate()
			if !target.storageView {
				t.Error("readForUpdate reads the tenant view, so a write is applied on " +
					"top of a translated object and writes the translation back")
			}
			if target.subresource != "" {
				t.Errorf("subresource = %q; a subresource has no stored object of its own",
					target.subresource)
			}
		})
	}
}

// TestATenantCannotBindItsPod is the regression guard for a confirmed live
// cross-tenant escape.
//
// ⛔ Measured, not reasoned about: tenant 333333 created a pod, had its
// nodeSelector replaced with its own pool by admission exactly as designed, and
// then bound that same pod onto 111111-node-az1 with one POST to pods/binding.
// The pod stayed Pending -- the other tenant's provider refuses to run it -- but
// it was BOUND, and a bound pod counts against that node's allocatable whether
// or not it ever starts. That node's capacity is the victim's quota, so this is
// one tenant exhausting a neighbour's schedulable capacity with pods that never
// run.
//
// ⭐ Placement injection cannot cover it. A binding is a SECOND write, made
// after the pod exists, and rewriting fields on the pod -- how every other
// placement rule here works -- reaches the first write only.
//
// ⚠️ Both spellings. The same Binding arrives as pods/binding and as the older
// top-level bindings resource; a guard written against one string would leave
// the other open.
func TestATenantCannotBindItsPod(t *testing.T) {
	tp := &tenantProxy{}
	binding := &core.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: "victimpod", Namespace: "333333-default"},
		Target:     core.ObjectReference{Kind: "Node", Name: "111111-node-az1"},
	}
	err := tp.refuseTenantBinding(binding)
	if err == nil {
		t.Fatal("a tenant was allowed to bind a pod to a node of its choosing; " +
			"measured on the live cluster, that node can belong to another tenant and " +
			"the bound pod consumes its quota without ever running")
	}
	if !strings.Contains(err.Error(), "placement belongs to the platform") {
		t.Errorf("refusal does not explain itself: %v", err)
	}
}

// TestBindingGuardLeavesEverythingElseAlone -- a guard that refused more than
// Bindings would break ordinary writes, and would do it on the create path that
// every tenant object goes through.
func TestBindingGuardLeavesEverythingElseAlone(t *testing.T) {
	tp := &tenantProxy{}
	for name, obj := range map[string]runtime.Object{
		"a pod":     &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
		"a service": &core.Service{ObjectMeta: metav1.ObjectMeta{Name: "s"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tp.refuseTenantBinding(obj); err != nil {
				t.Errorf("refused %s: %v", name, err)
			}
		})
	}
}
