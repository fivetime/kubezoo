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

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubewharf/kubezoo/pkg/util"
)

// TestNativeObjectConvertorConvertTenantObjectToUpstreamObject tests the
// ConvertTenantObjectToUpstreamObject methods of NativeObjectConvertor.
func TestNativeObjectConvertorConvertTenantObjectToUpstreamObject(t *testing.T) {
	tenant := "111111"
	originName := "good"
	originNamespace := "luck"

	pod := v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      originName,
			Namespace: originNamespace,
		},
	}

	c, _ := InitConvertors(checkGroupKind, FakeListEmptyTenantCRDsFunc)
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

	pod := v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      originName,
			Namespace: originNamespace,
		},
	}

	c, _ := InitConvertors(checkGroupKind, FakeListEmptyTenantCRDsFunc)
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
