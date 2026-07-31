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
	"k8s.io/apimachinery/pkg/runtime"
	internal "k8s.io/kubernetes/pkg/apis/networking"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// deprecatedIngressClassAnnotation still decides which controller serves an
// Ingress in most implementations, and it wins over the field. Rewriting only
// the field would leave the annotation as a way round it.
const deprecatedIngressClassAnnotation = "kubernetes.io/ingress.class"

// IngressTransformer rewrites the IngressClass an Ingress names.
//
// spec.ingressClassName references a cluster-scoped object, so it needs the same
// treatment as spec.volumeName on a PersistentVolumeClaim: kubezoo prefixes the
// IngressClass object itself, and without prefixing the reference the two never
// meet. Measured before this existed: a tenant's own IngressClass was stored as
// 111111-internal while its Ingress still said internal, so a controller the
// tenant ran never matched its own Ingresses -- and, worse, an unprefixed name
// is a name in the platform's own space, so writing "nginx" reached the
// platform's public controller directly.
//
// publicClasses are the names that pass through untouched. They are how a tenant
// asks for public traffic: the platform's ingress controller is the only thing
// wired to the outside (traffic arrives through Octavia and reaches it), so
// naming one of these is a deliberate request to be exposed, while naming
// anything else stays inside the tenant. Empty by default, which leaves every
// tenant Ingress internal.
//
// This also makes borrowing another tenant's class impossible rather than merely
// useless: 222222-nginx becomes 111111-222222-nginx and matches nothing. It used
// to be harmless only because no controller happened to be watching for it.
type IngressTransformer struct {
	publicClasses map[string]bool
}

var _ ObjectTransformer = &IngressTransformer{}

// NewIngressTransformer initiates an IngressTransformer which implements the
// ObjectTransformer interfaces.
func NewIngressTransformer(publicClasses []string) ObjectTransformer {
	allowed := make(map[string]bool, len(publicClasses))
	for _, name := range publicClasses {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			allowed[trimmed] = true
		}
	}
	return &IngressTransformer{publicClasses: allowed}
}

// Forward transforms tenant object reference to upstream object reference.
func (t *IngressTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	ingress, ok := obj.(*internal.Ingress)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of ingress")
	}
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		prefixed := t.toUpstream(tenantID, *ingress.Spec.IngressClassName)
		ingress.Spec.IngressClassName = &prefixed
	}
	if class, present := ingress.Annotations[deprecatedIngressClassAnnotation]; present && class != "" {
		ingress.Annotations[deprecatedIngressClassAnnotation] = t.toUpstream(tenantID, class)
	}
	return ingress, nil
}

// Backward transforms upstream object reference to tenant object reference.
func (t *IngressTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	ingress, ok := obj.(*internal.Ingress)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of ingress")
	}
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		trimmed := t.toTenant(tenantID, *ingress.Spec.IngressClassName)
		ingress.Spec.IngressClassName = &trimmed
	}
	if class, present := ingress.Annotations[deprecatedIngressClassAnnotation]; present && class != "" {
		ingress.Annotations[deprecatedIngressClassAnnotation] = t.toTenant(tenantID, class)
	}
	return ingress, nil
}

// toUpstream prefixes a class name unless it is one the platform publishes.
func (t *IngressTransformer) toUpstream(tenantID, class string) string {
	if t.publicClasses[class] {
		return class
	}
	return util.AddTenantIDPrefix(tenantID, class)
}

// toTenant undoes it.
//
// A public class comes back as the tenant wrote it. Anything else is trimmed,
// and a name that carries neither the tenant's prefix nor a public name is left
// alone rather than rejected: it can only have got there from outside kubezoo,
// and refusing to read the object would hide it from the tenant entirely.
func (t *IngressTransformer) toTenant(tenantID, class string) string {
	if t.publicClasses[class] {
		return class
	}
	return util.TrimTenantIDPrefix(tenantID, class)
}
