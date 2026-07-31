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
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	pkgadmission "k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/resourcequota"
	"k8s.io/apiserver/pkg/authentication/user"
	quotageneric "k8s.io/apiserver/pkg/quota/v1/generic"
	master "k8s.io/kubernetes/pkg/controlplane"
	quotainstall "k8s.io/kubernetes/pkg/quota/v1/install"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	quotav1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/quota/v1alpha1"
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
	// APIResourceConfigSource in 1.36.
	//
	// The factory was left nil here on the assumption that only the DRA
	// evaluators dereference it, and only with both DynamicResourceAllocation
	// and DRAExtendedResource enabled. The first half was right and the second
	// was not: DynamicResourceAllocation is GA and locked on since 1.35, and
	// DRAExtendedResource went beta and on by default in 1.36. The branch was
	// therefore always taken, and the component crashed on startup the first
	// time it was deployed.
	//
	// Rather than feed it a cluster-wide pod informer it has no other use for,
	// tell it what kubezoo actually serves. Tenants have no access to
	// resource.k8s.io at all -- apigroups.go does not expose it -- so there is
	// no DRA usage to charge anyone for, and disabling it skips the branch.
	//
	// The nil lister func is deliberate and matches upstream: admission measures
	// the incoming object rather than recomputing usage from a lister.
	resourceConfig := master.DefaultAPIResourceConfigSource()
	resourceConfig.DisableVersions(resourcev1.SchemeGroupVersion)
	quotaConfiguration, err := quotainstall.NewQuotaConfigurationForAdmission(nil, resourceConfig)
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
