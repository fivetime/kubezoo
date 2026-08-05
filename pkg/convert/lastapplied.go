/*
Copyright 2024 The KubeZoo Authors.

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
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/meta"
	corev1 "k8s.io/api/core/v1"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// rewriteLastApplied translates the object kubectl serialised into an
// annotation, which is the one place a whole object hides inside a string.
//
// ⛔ Found by sweeping a tenant's entire view for its own upstream prefix. Every
// field of every kind came back clean except this, on an object the PLATFORM had
// applied into a tenant namespace:
//
//	metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]
//	  = {..., "metadata": {"name": "platform-converge", "namespace": "111111-default"}, ...}
//
// ⚠️ The information it leaks is worth nothing -- the tenant's own namespace,
// which it can derive from the tenant ID in its own certificate. The reason to
// fix it is that this annotation is INPUT: `kubectl apply` three-way merges
// against it, so a tenant applying over such an object diffs "namespace:
// 111111-default" against a live object in "default" and computes a patch that
// moves the namespace. That is the shape of the bug where a PATCH carrying the
// upstream namespace name came back BadRequest.
//
// ⭐ Only objects the platform writes DIRECTLY upstream carry the upstream
// spelling. When a tenant applies, kubectl builds this annotation client-side
// from the tenant's own YAML, so it is already in the tenant's spelling -- and
// the forward direction here keeps it consistent with the namespace the object
// actually lands in.
//
// ⚠️ A value that will not parse is left exactly as it is, and no error is
// returned. The annotation is free-form: anything may write anything there, and
// failing the conversion would fail the whole object -- which fails the whole
// LIST, which is how a tenant loses sight of everything of that kind. Same rule
// as trimIfAttributable.
func rewriteLastApplied(obj interface{}, tenantID string, isNamespaceScoped, forward bool) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	annotations := accessor.GetAnnotations()
	raw, ok := annotations[corev1.LastAppliedConfigAnnotation]
	if !ok || len(raw) == 0 {
		return
	}
	var applied map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		return
	}
	metadata, ok := applied["metadata"].(map[string]interface{})
	if !ok {
		return
	}

	// ⚠️ Namespaced objects carry the tenant in the NAMESPACE, cluster-scoped ones
	// in the NAME -- the same split the convertor around this makes. Handling only
	// one of the two is how a rule ends up applied to one of the two places it
	// holds, which this repository has now met more than once in a day.
	key := "namespace"
	if !isNamespaceScoped {
		key = "name"
	}
	value, ok := metadata[key].(string)
	if !ok || value == "" {
		return
	}

	var rewritten string
	switch {
	case forward && isNamespaceScoped:
		// Idempotent: an in-cluster client may have applied a manifest that already
		// spells the namespace the upstream way.
		rewritten = util.UpstreamNamespace(tenantID, value)
	case forward:
		rewritten = util.AddTenantIDPrefix(tenantID, value)
	default:
		// TrimTenantIDPrefix leaves a value that does not carry the prefix alone,
		// which is what an object applied before this change looks like.
		rewritten = util.TrimTenantIDPrefix(tenantID, value)
	}
	if rewritten == value {
		return
	}
	metadata[key] = rewritten

	encoded, err := json.Marshal(applied)
	if err != nil {
		return
	}
	// ⚠️ Copied before writing: GetAnnotations may hand back the object's own map,
	// and for a cluster-scoped object shared between tenants' views that would
	// mutate what another reader sees.
	updated := make(map[string]string, len(annotations))
	for k, v := range annotations {
		updated[k] = v
	}
	updated[corev1.LastAppliedConfigAnnotation] = string(encoded)
	accessor.SetAnnotations(updated)
}
