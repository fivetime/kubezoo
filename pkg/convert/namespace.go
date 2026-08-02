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
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	internal "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

type NamespaceTransformer struct {
}

var _ ObjectTransformer = &NamespaceTransformer{}

func NewNamespaceTransformer() *NamespaceTransformer {
	return &NamespaceTransformer{}
}

func (t *NamespaceTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	ns, ok := obj.(*internal.Namespace)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to the internal version of namesapce")
	}
	// The default convertor has already put the tenant's prefix on the name, so
	// trimming one gets back to what the tenant asked for. A tenant-chosen name
	// that itself begins with the prefix is refused: an in-cluster workload
	// addresses its namespace by the upstream name, kubezoo reads a name
	// carrying the prefix as one already resolved, and a namespace stored under
	// the doubled prefix could never be reached again.
	if err := util.NamespaceNameForTenant(tenantID, util.TrimTenantIDPrefix(tenantID, ns.Name)); err != nil {
		return nil, err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	if v, ok := ns.Labels[common.TenantNamespaceLabelKey]; ok && v != tenantID {
		return nil, errors.Errorf("namespace label %s is protected by kubezoo, can not be modified", common.TenantNamespaceLabelKey)
	} else {
		ns.Labels[common.TenantNamespaceLabelKey] = tenantID
	}
	pinPodSecurity(ns)
	return ns, nil
}

// pinPodSecurity stamps the Pod Security Admission level on a tenant namespace
// and refuses a tenant's attempt to weaken it.
//
// ⭐ Stamped HERE, in Go, rather than left to the Kyverno policy that also
// stamps it. Pod Security Admission runs inside the apiserver -- no webhook, no
// single point -- so a namespace carrying this label refuses hostNetwork,
// hostPID, privileged and hostPath even with every webhook in the cluster gone.
// It is the only real depth in the whole pod surface.
//
// ⛔ But until now the label was put there by a Kyverno mutate, which meant the
// second layer was installed by the first. A namespace created while Kyverno's
// webhook was not registered carried no label and got no enforcement at all.
// failurePolicy: Fail does not cover that -- it covers a webhook which fails,
// not one which was never registered, and the latter is the failure that
// actually happened: three policies never became ready, pods went through
// unguarded, and the only symptom was READY=<none>. Stamping it on the way
// through kubezoo needs nothing outside this process to be alive.
//
// A tenant owns its namespaces and can label them, and one setting
// pod-security.kubernetes.io/enforce: privileged on its own is why
// config/policy/README.md exists. Overwriting is what stops that.
//
// ⚠️ OVERWRITTEN, not refused, which is the opposite of what the tenant label
// above does and is deliberate. Refusing reads stricter but is worse here: a
// namespace that did end up with a weaker value -- written during the very
// outage this hardening is for -- would become one its tenant could never write
// to again, fixable only by an administrator. Overwriting repairs it on the next
// write instead. It also keeps this identical to the Kyverno mutate rather than
// a second, differently-behaving copy. The tenant label is refused because
// changing it changes who OWNS the namespace; this one is only a level, and
// putting it back loses nothing.
func pinPodSecurity(ns *internal.Namespace) {
	ns.Labels[common.PodSecurityEnforceLabelKey] = common.PodSecurityLevel
	ns.Labels[common.PodSecurityEnforceVersionLabelKey] = common.PodSecurityVersion
}

func (t *NamespaceTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	return obj, nil
}
