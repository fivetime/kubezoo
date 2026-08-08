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

func hidden(t *testing.T, extra ...string) *HiddenMetadata {
	t.Helper()
	h, err := NewHiddenMetadata(extra...)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	return h
}

func svcMeta(annotations, labels map[string]string) *core.Service {
	return &core.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "rsvc", Namespace: "111111-default",
		Annotations: annotations, Labels: labels,
	}}
}

// TestATenantDoesNotSeeThePlatform is the whole point. Every kubezoo.io key a
// tenant can read is a fact about the layer underneath -- that there is one,
// what it calls things, which objects it touched -- and that is the map somebody
// uses to go looking for its edges.
func TestATenantDoesNotSeeThePlatform(t *testing.T) {
	svc := svcMeta(
		map[string]string{"kubezoo.io/cluster-ip": "192.168.200.7", "team": "payments"},
		map[string]string{"kubezoo.io/tenant": "111111", "app": "rsvc"},
	)
	hidden(t).Strip(svc)

	if _, present := svc.Annotations["kubezoo.io/cluster-ip"]; present {
		t.Error("a kubezoo.io annotation was shown to the tenant")
	}
	if _, present := svc.Labels["kubezoo.io/tenant"]; present {
		t.Error("a kubezoo.io label was shown to the tenant")
	}
	if svc.Annotations["team"] != "payments" || svc.Labels["app"] != "rsvc" {
		t.Errorf("the tenant's own metadata was taken away: %v %v", svc.Annotations, svc.Labels)
	}
}

// TestAReadModifyWriteDoesNotEraseThePlatform is the half that is easy to leave
// out, and leaving it out is a data-loss bug wearing the costume of a privacy
// feature.
//
// ⛔ The tenant never sees a hidden key, so it never sends one back. Without a
// restore, `kubectl get -o yaml | kubectl apply -f -` DELETES whatever the
// platform stored -- and for kubezoo.io/cluster-ip that means the tenant's pods
// silently go back to an address their network does not carry.
func TestAReadModifyWriteDoesNotEraseThePlatform(t *testing.T) {
	stored := svcMeta(
		map[string]string{"kubezoo.io/cluster-ip": "192.168.200.7"},
		map[string]string{"kubezoo.io/tenant": "111111"},
	)
	// What comes back from a tenant that read the stripped object.
	submitted := svcMeta(map[string]string{"team": "payments"}, map[string]string{"app": "rsvc"})

	hidden(t).Restore(submitted, stored)

	if submitted.Annotations["kubezoo.io/cluster-ip"] != "192.168.200.7" {
		t.Errorf("annotations = %v; the platform's stored value was erased by a "+
			"round-trip the tenant did not know it was making", submitted.Annotations)
	}
	if submitted.Labels["kubezoo.io/tenant"] != "111111" {
		t.Errorf("labels = %v; same", submitted.Labels)
	}
	if submitted.Annotations["team"] != "payments" {
		t.Error("the tenant's own annotation was dropped")
	}
}

// TestATenantCannotSetAHiddenKey -- the security half. A tenant that has learned
// a key exists must not be able to set it: kubezoo.io/cluster-ip is what kubezoo
// reports as a Service's address, and the tenant's own CoreDNS answers with it.
//
// ⭐ Generic on purpose. The specific guard for that one key existed; this makes
// the NEXT such key safe without anyone remembering to write one.
func TestATenantCannotSetAHiddenKey(t *testing.T) {
	// The key the platform already holds. Overwritten by the restore.
	stored := svcMeta(map[string]string{"kubezoo.io/cluster-ip": "192.168.200.7"}, nil)
	// ⚠️ And a hidden key the platform has NOT set. This is the case that
	// actually distinguishes dropping from not dropping: where a stored value
	// exists, the restore overwrites the tenant's regardless, so a test using
	// only that key passes whether or not anything was dropped. Measured -- the
	// first version of this test did exactly that.
	submitted := svcMeta(map[string]string{
		"kubezoo.io/cluster-ip": "10.6.6.6",
		"kubezoo.io/tenant-dns": "disabled",
	}, map[string]string{"kubezoo.io/platform-workload": "true"})

	hidden(t).Restore(submitted, stored)

	if got := submitted.Annotations["kubezoo.io/cluster-ip"]; got != "192.168.200.7" {
		t.Errorf("annotation = %q; the tenant's own value reached storage, so kubezoo "+
			"would report an address of the tenant's choosing and its resolver would "+
			"answer with it", got)
	}
	if _, present := submitted.Annotations["kubezoo.io/tenant-dns"]; present {
		t.Error("a tenant set a hidden annotation the platform had not set, and it survived")
	}
	if _, present := submitted.Labels["kubezoo.io/platform-workload"]; present {
		t.Error("a tenant set a hidden label the platform had not set, and it survived")
	}
}

// TestOnACreateThereIsNothingToRestore -- old is nil, so dropping is all that
// happens. A tenant must not be able to plant a hidden key on the first write
// either.
func TestOnACreateThereIsNothingToRestore(t *testing.T) {
	submitted := svcMeta(map[string]string{"kubezoo.io/cluster-ip": "10.6.6.6", "team": "x"}, nil)
	hidden(t).Restore(submitted, nil)
	if _, present := submitted.Annotations["kubezoo.io/cluster-ip"]; present {
		t.Error("a tenant planted a hidden key on create")
	}
	if submitted.Annotations["team"] != "x" {
		t.Error("the tenant's own annotation was dropped")
	}
}

// TestListsAreCovered -- and this one is not a formality. meta.ExtractList has
// to hand back pointers INTO the list for the mutation to stick; if it copied,
// every list read would silently show the platform's metadata while every single
// GET hid it. `kubectl get`, an informer's initial LIST and an operator's cache
// fill all take this path.
func TestListsAreCovered(t *testing.T) {
	list := &core.ServiceList{Items: []core.Service{
		*svcMeta(map[string]string{"kubezoo.io/cluster-ip": "192.168.200.7"}, nil),
		*svcMeta(nil, map[string]string{"kubezoo.io/tenant": "111111"}),
	}}
	hidden(t).Strip(list)
	if _, present := list.Items[0].Annotations["kubezoo.io/cluster-ip"]; present {
		t.Error("list item 0 still shows the platform's annotation")
	}
	if _, present := list.Items[1].Labels["kubezoo.io/tenant"]; present {
		t.Error("list item 1 still shows the platform's label")
	}
}

// TestAnOperatorCanHideMore -- the configurable half, and the platform pattern
// stays in force alongside it.
func TestAnOperatorCanHideMore(t *testing.T) {
	h := hidden(t, `^knaas\.io/`, `^internal-`)
	svc := svcMeta(map[string]string{
		"kubezoo.io/cluster-ip": "1.2.3.4",
		"knaas.io/address":      "5.6.7.8",
		"internal-note":         "x",
		"team":                  "payments",
	}, nil)
	h.Strip(svc)
	if len(svc.Annotations) != 1 || svc.Annotations["team"] != "payments" {
		t.Errorf("annotations = %v, want only the tenant's own", svc.Annotations)
	}
}

// TestABadPatternIsAnError -- silently skipping one would leave an operator
// believing a rule is in force that matches nothing, and nothing would say so
// until someone audited what tenants can see.
func TestABadPatternIsAnError(t *testing.T) {
	if _, err := NewHiddenMetadata("^[unclosed"); err == nil {
		t.Error("a pattern that does not compile was accepted; the rule an operator " +
			"asked for would then match nothing, silently")
	}
}

// TestThePlatformPatternCannotBeConfiguredAway -- an operator passing its own
// list must not end up with kubezoo.io visible.
func TestThePlatformPatternCannotBeConfiguredAway(t *testing.T) {
	svc := svcMeta(map[string]string{"kubezoo.io/tenant-dns": "disabled"}, nil)
	hidden(t, `^something-else/`).Strip(svc)
	if len(svc.Annotations) != 0 {
		t.Errorf("annotations = %v; the platform pattern is not optional", svc.Annotations)
	}
}
