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
	"github.com/fivetime/kubezoo-gateway/pkg/publishedclass"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	internal "k8s.io/kubernetes/pkg/apis/networking"
)

func ingressWithClass(class string, annotation string) *internal.Ingress {
	ingress := &internal.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "app"}}
	if class != "" {
		name := class
		ingress.Spec.IngressClassName = &name
	}
	if annotation != "" {
		ingress.Annotations = map[string]string{deprecatedIngressClassAnnotation: annotation}
	}
	return ingress
}

func classOf(t *testing.T, obj interface{}) string {
	t.Helper()
	ingress, ok := obj.(*internal.Ingress)
	if !ok || ingress.Spec.IngressClassName == nil {
		return ""
	}
	return *ingress.Spec.IngressClassName
}

// TestTenantClassIsPrefixedBothWays -- the IngressClass object a tenant creates
// is cluster-scoped and so gets the prefix. Without prefixing the reference too,
// the two never meet and a controller the tenant runs never matches its own
// Ingresses.
func TestTenantClassIsPrefixedBothWays(t *testing.T) {
	transformer := NewIngressTransformer(publishedclass.Static("ingressclass", nil))

	forward, err := transformer.Forward(ingressWithClass("internal", ""), "111111")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := classOf(t, forward); got != "111111-internal" {
		t.Errorf("inbound class = %q, want 111111-internal; unprefixed it cannot reach the tenant's own IngressClass", got)
	}

	backward, err := transformer.Backward(ingressWithClass("111111-internal", ""), "111111")
	if err != nil {
		t.Fatalf("backward: %v", err)
	}
	if got := classOf(t, backward); got != "internal" {
		t.Errorf("outbound class = %q, want internal", got)
	}
}

// TestPublicClassPassesThrough is how a tenant asks to be exposed: the
// platform's controller is the only thing wired to the outside, so naming it
// must reach it unchanged.
func TestPublicClassPassesThrough(t *testing.T) {
	transformer := NewIngressTransformer(publishedclass.Static("ingressclass", []string{"octavia"}))

	forward, err := transformer.Forward(ingressWithClass("octavia", ""), "111111")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := classOf(t, forward); got != "octavia" {
		t.Errorf("public class = %q, want octavia unchanged; prefixed, the tenant cannot be exposed at all", got)
	}
	backward, err := transformer.Backward(ingressWithClass("octavia", ""), "111111")
	if err != nil {
		t.Fatalf("backward: %v", err)
	}
	if got := classOf(t, backward); got != "octavia" {
		t.Errorf("public class came back as %q, want octavia", got)
	}
}

// TestBorrowingAnotherTenantsClassMatchesNothing -- writing another tenant's real
// upstream class used to be harmless only because no controller happened to be
// watching for it. Prefixed, it cannot name anything that exists.
func TestBorrowingAnotherTenantsClassMatchesNothing(t *testing.T) {
	transformer := NewIngressTransformer(publishedclass.Static("ingressclass", []string{"octavia"}))

	forward, err := transformer.Forward(ingressWithClass("222222-nginx", ""), "111111")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := classOf(t, forward); got != "111111-222222-nginx" {
		t.Errorf("borrowed class = %q, want it prefixed into something that matches nothing", got)
	}
}

// TestDeprecatedAnnotationIsRewrittenToo -- most controllers still honour it and
// it wins over the field, so rewriting only the field leaves a way round.
func TestDeprecatedAnnotationIsRewrittenToo(t *testing.T) {
	transformer := NewIngressTransformer(publishedclass.Static("ingressclass", []string{"octavia"}))

	forward, err := transformer.Forward(ingressWithClass("", "nginx"), "111111")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	ingress := forward.(*internal.Ingress)
	if got := ingress.Annotations[deprecatedIngressClassAnnotation]; got != "111111-nginx" {
		t.Errorf("annotation = %q, want 111111-nginx; left alone it overrides the field", got)
	}

	public, err := transformer.Forward(ingressWithClass("", "octavia"), "111111")
	if err != nil {
		t.Fatalf("forward public: %v", err)
	}
	if got := public.(*internal.Ingress).Annotations[deprecatedIngressClassAnnotation]; got != "octavia" {
		t.Errorf("public annotation = %q, want octavia unchanged", got)
	}
}
