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
	"testing"

	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	extensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func crd(name, group, kind string) *extensionsv1.CustomResourceDefinition {
	return &extensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: extensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: extensionsv1.CustomResourceDefinitionNames{Kind: kind},
			Scope: extensionsv1.NamespaceScoped,
		},
	}
}

// checkerOver builds the real check function over a lister holding these CRDs.
func checkerOver(t *testing.T, crds ...*extensionsv1.CustomResourceDefinition) CheckGroupKindFunc {
	t.Helper()
	objs := make([]interface{}, 0, len(crds))
	for _, c := range crds {
		objs = append(objs, c)
	}
	client := extensionsfake.NewSimpleClientset()
	factory := extensionsinformers.NewSharedInformerFactory(client, 0)
	informer := factory.Apiextensions().V1().CustomResourceDefinitions()
	for _, o := range objs {
		if err := informer.Informer().GetIndexer().Add(o); err != nil {
			t.Fatalf("seeding the CRD lister: %v", err)
		}
	}
	return NewCheckGroupKindFunc(informer.Lister())
}

// TestTenantCRDResolvesInBothDirections is the round trip. The group a tenant
// writes is unprefixed and the group coming back from upstream is prefixed, so
// the same CRD has to be recognised from either form -- and recognised as the
// tenant's, which is what makes the prefix get added on the way in and trimmed
// on the way out.
//
// It used to add the prefix in both directions. On the way out that produced
// test01-test01-example.com, matched nothing, and fell through to the lookup
// for platform CRDs, which found the tenant's own CRD and reported it as a
// platform one -- so the group was never trimmed and the tenant read its own
// ownerReferences back as test01-example.com/v1, an object it could read but
// not apply again. Measured in a cluster before this was fixed.
func TestTenantCRDResolvesInBothDirections(t *testing.T) {
	check := checkerOver(t, crd("foos.test01-example.com", "test01-example.com", "Foo"))

	for _, tc := range []struct {
		name           string
		group          string
		isTenantObject bool
	}{
		{"inbound, as the tenant writes it", "example.com", true},
		{"outbound, as upstream stores it", "test01-example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			namespaced, customResourceGroup, err := check(tc.group, "Foo", "test01", tc.isTenantObject)
			if err != nil {
				t.Fatalf("resolving %s: %v", tc.group, err)
			}
			if !customResourceGroup {
				t.Errorf("the tenant's own CRD was not reported as the tenant's; the group would "+
					"be left prefixed and the tenant could not apply the object back")
			}
			if !namespaced {
				t.Errorf("namespaced = false, want true")
			}
		})
	}
}

// TestPlatformCRDResolvesOnlyOutbound -- a platform controller may stamp a
// reference to its own CRD onto a tenant's object, and the tenant has to be
// able to read that object back, so the outbound direction resolves it as a
// platform object. The inbound direction must not: sharing a platform CRD with
// a tenant is not implemented, and accepting a tenant-authored reference to one
// would answer "does this CRD exist upstream" for any group a tenant names.
func TestPlatformCRDResolvesOnlyOutbound(t *testing.T) {
	check := checkerOver(t, crd("clonesets.platform.io", "platform.io", "CloneSet"))

	namespaced, customResourceGroup, err := check("platform.io", "CloneSet", "test01", false)
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	if customResourceGroup {
		t.Error("a platform CRD was reported as the tenant's; its group would be trimmed")
	}
	if !namespaced {
		t.Error("namespaced = false, want true")
	}

	if _, _, err := check("platform.io", "CloneSet", "test01", true); err == nil {
		t.Error("a tenant-authored reference to a platform CRD was accepted; that is both " +
			"sharing the CRD by accident and an oracle for which CRDs exist upstream")
	}
}
