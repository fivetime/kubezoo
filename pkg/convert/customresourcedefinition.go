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
	"strings"

	"github.com/pkg/errors"
	crdinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// CRDConvertor implements the transformation between client and
// upstream server for CustomResourceDefinition resource.
type CRDConvertor struct {
	ownerRefTransformer OwnerReferenceTransformer
}

var _ common.ObjectConvertor = &CRDConvertor{}

// NewCRDConvertor initiates a CRDConvertor which implements the
// ObjectConvertor interfaces.
func NewCRDConvertor(ort OwnerReferenceTransformer) common.ObjectConvertor {
	return &CRDConvertor{
		ownerRefTransformer: ort,
	}
}

// ConvertTenantObjectToUpstreamObject convert the tenant object to
// upstream object.
func (t *CRDConvertor) ConvertTenantObjectToUpstreamObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	crd, ok := obj.(*crdinternal.CustomResourceDefinition)
	if !ok {
		return errors.Errorf("fail to assert the runtime object to the internal version of crd")
	}

	if crd.Name != crd.Spec.Names.Plural+"."+crd.Spec.Group {
		return errors.Errorf("The CustomResourceDefinition \"%s\" is invalid: metadata.name: Invalid value: \"%s\": must be spec.names.plural+\".\"+spec.group",
			crd.Name, crd.Name)
	}
	crd.Spec.Group = util.AddTenantIDPrefix(tenantID, crd.Spec.Group)
	crd.Name = crd.Spec.Names.Plural + "." + crd.Spec.Group
	if err := forwardCRDConversionWebhook(crd, tenantID); err != nil {
		return err
	}
	for i := range crd.OwnerReferences {
		target, err := t.ownerRefTransformer.Forward(&crd.OwnerReferences[i], tenantID)
		if err != nil {
			return err
		}
		crd.OwnerReferences[i] = *target
	}
	return nil
}

// ConvertUpstreamObjectToTenantObject convert the upstream object to
// tenant object.
func (t *CRDConvertor) ConvertUpstreamObjectToTenantObject(obj runtime.Object, tenantID string, isNamespaceScoped bool) error {
	crd, ok := obj.(*crdinternal.CustomResourceDefinition)
	if !ok {
		return errors.Errorf("fail to assert the runtime object to the internal version of crd")
	}

	if !strings.HasPrefix(crd.Spec.Group, tenantID) {
		return errors.Errorf("invalid spec.group %s for crd %s, tenant id is %s", crd.Spec.Group, crd.Name, tenantID)
	}
	crd.Spec.Group = util.TrimTenantIDPrefix(tenantID, crd.Spec.Group)
	crd.Name = crd.Spec.Names.Plural + "." + crd.Spec.Group
	if err := backwardCRDConversionWebhook(crd, tenantID); err != nil {
		return err
	}
	for i := range crd.OwnerReferences {
		target, err := t.ownerRefTransformer.Backward(&crd.OwnerReferences[i], tenantID)
		if err != nil {
			return err
		}
		crd.OwnerReferences[i] = *target
	}
	return nil
}

// A CRD carries a second webhook that has nothing to do with
// admissionregistration: spec.conversion.webhook converts between the CRD's own
// versions, and its clientConfig has the same shape and the same problem. It was
// not rewritten either, so a tenant naming a namespace reached the platform's
// namespace of that name rather than its own.
//
// Only the client config needs confining here. A conversion webhook is invoked
// solely for custom resources of this CRD, and the CRD's group already carries
// the tenant prefix, so there is no cross-tenant reach to close.

func forwardCRDConversionWebhook(crd *crdinternal.CustomResourceDefinition, tenantID string) error {
	clientConfig := crdConversionClientConfig(crd)
	if clientConfig == nil {
		return nil
	}
	if clientConfig.URL != nil {
		return errors.Errorf("crd %s: spec.conversion.webhook.clientConfig.url is not available "+
			"to tenants, because a URL cannot be confined to the tenant; use service to name a "+
			"service in one of your namespaces", crd.Name)
	}
	if clientConfig.Service != nil && len(clientConfig.Service.Namespace) > 0 {
		clientConfig.Service.Namespace = util.AddTenantIDPrefix(tenantID, clientConfig.Service.Namespace)
	}
	return nil
}

func backwardCRDConversionWebhook(crd *crdinternal.CustomResourceDefinition, tenantID string) error {
	clientConfig := crdConversionClientConfig(crd)
	if clientConfig == nil || clientConfig.Service == nil || len(clientConfig.Service.Namespace) == 0 {
		return nil
	}
	if !strings.HasPrefix(clientConfig.Service.Namespace, tenantID+"-") {
		return errors.Errorf("crd %s: conversion webhook points at namespace %s, which does not "+
			"belong to tenant %s", crd.Name, clientConfig.Service.Namespace, tenantID)
	}
	clientConfig.Service.Namespace = util.TrimTenantIDPrefix(tenantID, clientConfig.Service.Namespace)
	return nil
}

func crdConversionClientConfig(crd *crdinternal.CustomResourceDefinition) *crdinternal.WebhookClientConfig {
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.WebhookClientConfig == nil {
		return nil
	}
	return crd.Spec.Conversion.WebhookClientConfig
}
