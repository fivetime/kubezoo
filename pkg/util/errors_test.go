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
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTrimTenantIDFromErrorWithStatusError tests with status error.
func TestTrimTenantIDFromErrorWithStatusError(t *testing.T) {
	tenantId := "111111"
	msg := "unauthorized"
	statusErr := apierrors.NewUnauthorized(tenantId + "-" + msg)
	err := TrimTenantIDFromError(statusErr, tenantId)
	if err.Error() != msg {
		fmt.Printf("error message: %s", err)
		t.Errorf("unexpected trim tenant id from error")
	}
}

// TestTrimTenantIDFromErrorWithStatusError tests with non status error.
func TestTrimTenantIDFromErrorWithNonStatusError(t *testing.T) {
	tenantId := "111111"
	msg := "unauthorized"
	errIn := fmt.Errorf("%s-%s", tenantId, msg)
	err := TrimTenantIDFromError(errIn, tenantId)
	if err.Error() != msg {
		fmt.Printf("error message: %s", err)
		t.Errorf("unexpected trim tenant id from error")
	}
}

// TestPlatformDetailIsRemovedFromMessages -- the API server prefixes a webhook's
// message with the webhook's name, which is the service running it. That tells a
// tenant which policy engine the platform uses and where it lives, which is of no
// use to the tenant and of some use to anyone looking for a way past it. The
// policy's own message is what the tenant needs, and it survives.
func TestPlatformDetailIsRemovedFromMessages(t *testing.T) {
	status := metav1.Status{
		Message: `admission webhook "validate.kyverno.svc-fail" denied the request: ` +
			`resource Pod/111111-default/z2 was blocked due to the following policies\n\ntenant-scheduling:\n  deny-nodename: spec.nodeName is not available to tenants`,
		Details: &metav1.StatusDetails{
			Causes: []metav1.StatusCause{{
				Message: `admission webhook "validate.kyverno.svc-fail" denied the request: nope`,
			}},
		},
	}
	got := TrimTenantIDFromStatus(status, "111111")

	for _, unwanted := range []string{"kyverno", "admission webhook", "111111-"} {
		if strings.Contains(got.Message, unwanted) {
			t.Errorf("message still carries %q: %s", unwanted, got.Message)
		}
	}
	if strings.Contains(got.Details.Causes[0].Message, "kyverno") {
		t.Errorf("cause still carries the webhook name: %s", got.Details.Causes[0].Message)
	}
	// The half the tenant can act on has to survive, or this trade is a bad one.
	for _, wanted := range []string{"tenant-scheduling", "deny-nodename", "spec.nodeName"} {
		if !strings.Contains(got.Message, wanted) {
			t.Errorf("message lost %q, which is the part the tenant needs: %s", wanted, got.Message)
		}
	}
}
