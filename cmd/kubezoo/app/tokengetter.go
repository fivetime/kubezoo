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

package app

import (
	"context"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// upstreamTokenGetter resolves the objects a ServiceAccount token is bound to.
//
// Without one of these kubezoo cannot authenticate a ServiceAccount token at
// all. Every token the kubelet projects has been bound to a pod since 1.21, and
// validating a bound token means fetching the pod, the service account, and
// sometimes the node it names; with no getter the validator fails outright and
// the request comes back 401 with "authentication failed unexpectedly". That is
// what a tenant's own workload got when it tried to reach kubezoo instead of
// upstream, which is the whole point of accepting these tokens: a workload
// pointed at kubezoo sees the tenant's view, with API groups unprefixed, which
// is the view its code was written against.
//
// The names in a token are already upstream names -- namespace 111111-default,
// service account default -- so this reads through a plain upstream client and
// does no translation. Using kubezoo's own conversion here would prefix them a
// second time and find nothing.
//
// Upstream kube-apiserver wires serviceaccountcontroller.NewGetterFromClient
// with listers from its informers. kubezoo runs no informers over the upstream
// cluster, and that helper dereferences its listers without a nil check, so this
// is the same thing without the cache: every lookup is a live GET. Token
// validation is on the authentication path of every request from a tenant
// workload, so if that ever becomes the bottleneck, the fix is informers here
// rather than a different design.
type upstreamTokenGetter struct {
	client kubernetes.Interface
}

var _ serviceaccount.ServiceAccountTokenGetter = &upstreamTokenGetter{}

func newUpstreamTokenGetter(client kubernetes.Interface) serviceaccount.ServiceAccountTokenGetter {
	return &upstreamTokenGetter{client: client}
}

func (g *upstreamTokenGetter) GetServiceAccount(ctx context.Context, namespace, name string) (*corev1.ServiceAccount, error) {
	return g.client.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (g *upstreamTokenGetter) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return g.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (g *upstreamTokenGetter) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return g.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (g *upstreamTokenGetter) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return g.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

func (g *upstreamTokenGetter) GetValidatingWebhookConfiguration(ctx context.Context, name string) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	return g.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, name, metav1.GetOptions{})
}

func (g *upstreamTokenGetter) GetMutatingWebhookConfiguration(ctx context.Context, name string) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	return g.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, name, metav1.GetOptions{})
}
