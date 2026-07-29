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

package controllers

import (
	"context"
	goerrors "errors"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	pkgadmission "k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/resourcequota"
	"k8s.io/apiserver/pkg/authentication/user"
	quotageneric "k8s.io/apiserver/pkg/quota/v1/generic"
	quotainstall "k8s.io/kubernetes/pkg/quota/v1/install"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	quotav1alpha1 "github.com/kubewharf/kubezoo/pkg/apis/quota/v1alpha1"
)

type Admission struct {
	evaluator resourcequota.Evaluator
	decoder   admission.Decoder
	logger    logr.Logger
}

// NewAdmission builds the quota admission handler.
//
// controller-runtime used to hand the decoder to the handler through
// DecoderInjector, which was removed; the decoder is now built here from the
// client's scheme. admission.Decoder also became an interface rather than a
// struct pointer.
func NewAdmission(ctx context.Context, client client.Client) (*Admission, error) {
	accessor := &quotaAccessor{client: client}

	// NewQuotaConfigurationForAdmission grew an informer factory and an
	// APIResourceConfigSource. Both are safe to leave nil here: the factory is
	// only dereferenced by the DRA evaluators, and only when both
	// DynamicResourceAllocation and DRAExtendedResource are on, and a nil
	// resource config means every resource is enabled. Upstream likewise passes
	// a nil lister func on the admission path, because admission measures the
	// incoming object rather than recomputing usage from a lister.
	quotaConfiguration, err := quotainstall.NewQuotaConfigurationForAdmission(nil, nil)
	if err != nil {
		return nil, err
	}

	return &Admission{
		decoder: admission.NewDecoder(client.Scheme()),
		evaluator: resourcequota.NewQuotaEvaluator(
			accessor,
			nil,
			quotageneric.NewRegistry(quotaConfiguration.Evaluators()),
			nil,
			nil,
			10,
			ctx.Done(),
		),
	}, nil
}

// InjectDecoder injects the decoder into a validatingHandler.
func (a *Admission) InjectLogger(logger logr.Logger) error {
	a.logger = logger
	return nil
}

func (a *Admission) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation == admissionv1.Create {
		var obj corev1.Pod
		err := a.decoder.Decode(req, &obj)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		req.Object.Object = &obj
		attributes := CreateAdmissionAttributes(req.AdmissionRequest)
		err = a.evaluator.Evaluate(attributes)
		if err != nil {
			var apiStatus errors.APIStatus
			if goerrors.As(err, &apiStatus) {
				return validationResponseFromStatus(false, apiStatus.Status())
			}
			return admission.Denied(err.Error())
		}
	}
	return admission.Allowed("")
}

func CreateAdmissionAttributes(req admissionv1.AdmissionRequest) pkgadmission.Attributes {
	info := &user.DefaultInfo{
		UID:    req.UserInfo.UID,
		Name:   req.UserInfo.Username,
		Groups: req.UserInfo.Groups,
		Extra:  map[string][]string{},
	}
	for k := range req.UserInfo.Extra {
		info.Extra[k] = []string(req.UserInfo.Extra[k])
	}

	return pkgadmission.NewAttributesRecord(
		req.Object.Object,
		req.OldObject.Object,
		schema.GroupVersionKind(req.Kind),
		req.Namespace,
		req.Name,
		schema.GroupVersionResource(req.Resource),
		req.SubResource,
		pkgadmission.Operation(req.Operation),
		req.Options.Object,
		*req.DryRun,
		info,
	)
}

type quotaAccessor struct {
	client client.Client
}

func (a *quotaAccessor) GetQuotas(namespace string) ([]corev1.ResourceQuota, error) {
	var quotaList corev1.ResourceQuotaList
	selector := labels.Set{
		LabelClusterResourceQuotaAutoUpdate: "true",
	}.AsSelector()
	err := a.client.List(context.TODO(), &quotaList, &client.ListOptions{Namespace: namespace, LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	quotas := []corev1.ResourceQuota{}
	for i := range quotaList.Items {
		quota := &quotaList.Items[i]
		owner := metav1.GetControllerOf(quota)
		if owner.Kind != ClusterResourceQuotaKind {
			continue
		}
		var clusterquota quotav1alpha1.ClusterResourceQuota
		err := a.client.Get(context.TODO(), types.NamespacedName{Name: owner.Name}, &clusterquota)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		// use cluster resource quota usage
		quota.Status = clusterquota.Status.ResourceQuotaStatus
		// change quota name for debug
		quota.Name = fmt.Sprintf("clusterresourcequota/%s", clusterquota.Name)
		quotas = append(quotas, *quota)
	}
	return quotas, nil
}

func (a *quotaAccessor) UpdateQuotaStatus(newQuota *corev1.ResourceQuota) error {
	return nil
}

// validationResponseFromStatus returns a response for admitting a request with provided Status object.
func validationResponseFromStatus(allowed bool, status metav1.Status) admission.Response {
	resp := admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: allowed,
			Result:  &status,
		},
	}
	return resp
}
