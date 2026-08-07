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

package convert

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// TestNativeObjectConvertorConvertTenantObjectToUpstreamObject tests the
// ConvertTenantObjectToUpstreamObject methods of NativeObjectConvertor.
func TestNativeObjectConvertorConvertTenantObjectToUpstreamObject(t *testing.T) {
	tenant := "111111"
	originName := "good"
	originNamespace := "luck"

	// ⚠️ The INTERNAL type, which is what the proxy hands the convertor --
	// apigroups.go's NewFunc for pods returns &core.Pod{}. These were versioned
	// v1.Pods, and passed only because no convertor was registered for Pod at
	// all, so they fell through to the default one, which reaches an object
	// through meta.Accessor and never looks at its type. The moment Pod got a
	// convertor the difference mattered -- and a test asserting namespace
	// prefixing on a type production never sees was not testing much.
	pod := core.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      originName,
			Namespace: originNamespace,
		},
	}

	c, _ := InitConvertors(checkGroupKind, FakeListEmptyTenantCRDsFunc, nil, nil, nil)
	err := c.ConvertTenantObjectToUpstreamObject(&pod, tenant, true)
	if err != nil {
		t.Errorf("Failed to convert tenant object to upstream object")
	}
	if pod.GetNamespace() != tenant+util.TenantIDSeparator+originNamespace {
		t.Errorf("Unexpected namespace")
	}
}

// TestNativeObjectConvertorConvertUpstreamObjectToTenantObject tests the
// ConvertUpstreamObjectToTenantObject methods of NativeObjectConvertor.
func TestNativeObjectConvertorConvertUpstreamObjectToTenantObject(t *testing.T) {
	tenant := "111111"
	originName := "good"
	originNamespace := tenant + util.TenantIDSeparator + "luck"

	pod := core.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      originName,
			Namespace: originNamespace,
		},
	}

	c, _ := InitConvertors(checkGroupKind, FakeListEmptyTenantCRDsFunc, nil, nil, nil)
	err := c.ConvertUpstreamObjectToTenantObject(&pod, tenant, true)
	if err != nil {
		t.Errorf("Failed to convert tenant object to upstream object")
	}
	if tenant+util.TenantIDSeparator+pod.GetNamespace() != originNamespace {
		t.Errorf("Unexpected namespace")
	}
}

func FakeListEmptyTenantCRDsFunc(tenantID string) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	return []*apiextensionsv1.CustomResourceDefinition{}, nil
}

// TestWebhookConfigurationsAreWired guards the wiring, not the transformer.
//
// This is the shape of defect the isolation audit found twice: PVTransformer and
// PVCTransformer are both fully implemented with passing unit tests, and neither
// is registered in InitConvertors, so neither ever runs. A transformer that is
// not in the map is the same as no transformer at all, and nothing else notices.
func TestWebhookConfigurationsAreWired(t *testing.T) {
	native, _ := InitConvertors(
		func(group, kind, tenantID string, isTenantObject bool) (bool, bool, error) {
			return true, false, nil
		},
		FakeListEmptyTenantCRDsFunc,
		nil,
		nil,
		nil,
	)
	registered := native.(*nativeObjectConvertor).nativeKindToConvertors

	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		gk := schema.GroupKind{Group: "admissionregistration.k8s.io", Kind: kind}
		convertor, ok := registered[gk]
		if !ok {
			t.Errorf("%s has no convertor registered, so a tenant's webhook reaches the whole "+
				"cluster and its clientConfig names a platform namespace", gk)
			continue
		}
		if _, isCross := convertor.(*CrossReferenceConvertor); !isCross {
			t.Errorf("%s is registered but not with a cross-reference transformer: %T", gk, convertor)
		}
	}
}

// TestPVAndPVCAreWired is the same wiring guard as the webhook one, for the two
// transformers that prompted it.
//
// NewPVTransformer and NewPVCTransformer were both fully implemented with
// passing unit tests, and neither was ever registered. The result was that a
// tenant's PersistentVolume landed upstream under a bare name it could not then
// get or delete, a second tenant creating that name got AlreadyExists, and the
// object stayed upstream permanently. The transformers were fine; the wiring was
// missing, and nothing was looking at the wiring.
func TestPVAndPVCAreWired(t *testing.T) {
	native, _ := InitConvertors(
		func(group, kind, tenantID string, isTenantObject bool) (bool, bool, error) {
			return true, false, nil
		},
		FakeListEmptyTenantCRDsFunc,
		nil,
		nil,
		nil,
	)
	registered := native.(*nativeObjectConvertor).nativeKindToConvertors

	for _, kind := range []string{"PersistentVolume", "PersistentVolumeClaim"} {
		gk := schema.GroupKind{Group: "", Kind: kind}
		convertor, ok := registered[gk]
		if !ok {
			t.Errorf("%s has no convertor registered", gk)
			continue
		}
		if _, isCross := convertor.(*CrossReferenceConvertor); !isCross {
			t.Errorf("%s is registered as %T rather than with its transformer; a nope or plain "+
				"default convertor leaves the PV/PVC binding path unconverted", gk, convertor)
		}
	}
}

// TestAccessReviewsAreWired -- the same wiring guard again, for the four kinds
// in authorization.k8s.io. Unwired, `kubectl auth can-i` answers about the
// platform's namespaces rather than the tenant's, and a SubjectAccessReview
// reads the platform's RBAC.
func TestAccessReviewsAreWired(t *testing.T) {
	native, _ := InitConvertors(
		func(group, kind, tenantID string, isTenantObject bool) (bool, bool, error) {
			return true, false, nil
		},
		FakeListEmptyTenantCRDsFunc,
		nil,
		nil,
		nil,
	)
	registered := native.(*nativeObjectConvertor).nativeKindToConvertors

	for _, kind := range []string{
		"SelfSubjectAccessReview",
		"LocalSubjectAccessReview",
		"SubjectAccessReview",
		"SelfSubjectRulesReview",
	} {
		gk := schema.GroupKind{Group: "authorization.k8s.io", Kind: kind}
		convertor, ok := registered[gk]
		if !ok {
			t.Errorf("%s has no convertor registered, so the question is asked upstream in the "+
				"platform's terms and its answer does not describe the tenant", gk)
			continue
		}
		if _, isCross := convertor.(*CrossReferenceConvertor); !isCross {
			t.Errorf("%s is registered but not with a cross-reference transformer: %T", gk, convertor)
		}
	}
}
