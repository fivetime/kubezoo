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
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlatformMetadataPattern is the one pattern that is always hidden and cannot be
// configured away: kubezoo's own namespace.
//
// ⭐ A tenant reading its own objects should see its own objects. Every
// kubezoo.io key it can see is a fact about the layer underneath -- that there
// IS a layer underneath, what it calls things, which of its objects the platform
// touched -- and that is the map somebody uses to go looking for the layer's
// edges.
const PlatformMetadataPattern = `^kubezoo\.io/`

// HiddenMetadata hides platform metadata from tenants in one place, for every
// object, in both directions.
//
// ⛔ Both directions, and the second one is the part that is easy to leave out.
// Hiding a key on read means the tenant never sends it back, so a
// read-modify-write would DELETE what the platform stored -- kubectl apply of a
// file produced by kubectl get, an operator round-tripping an object. Strip
// without Restore is a data-loss bug wearing the costume of a privacy feature.
//
// ⚠️ And the tenant's own value for such a key is dropped rather than refused.
// Refusing would break every one of those round-trips for a key the tenant never
// knowingly touched.
type HiddenMetadata struct {
	patterns []*regexp.Regexp
}

// NewHiddenMetadata compiles the platform pattern plus whatever an operator
// added.
//
// ⛔ Returns an error on a bad pattern rather than skipping it. A regexp that
// does not compile, silently dropped, is a rule an operator believes is in force
// and which matches nothing -- the exact shape of failure this codebase keeps
// producing, and one that would be invisible until someone audited what tenants
// can see.
func NewHiddenMetadata(extra ...string) (*HiddenMetadata, error) {
	h := &HiddenMetadata{}
	for _, p := range append([]string{PlatformMetadataPattern}, extra...) {
		if p == "" {
			continue
		}
		compiled, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("hidden metadata pattern %q: %w", p, err)
		}
		h.patterns = append(h.patterns, compiled)
	}
	return h, nil
}

func (h *HiddenMetadata) hides(key string) bool {
	if h == nil {
		return false
	}
	for _, p := range h.patterns {
		if p.MatchString(key) {
			return true
		}
	}
	return false
}

// Strip removes hidden keys from what a tenant is about to be shown.
//
// ⚠️ Annotations and labels both. A label is as much a statement about the
// platform as an annotation, and the resolver the platform runs inside a
// tenant's own namespace carries both.
func (h *HiddenMetadata) Strip(obj runtime.Object) {
	h.forEachObject(obj, func(o metav1.Object) {
		o.SetAnnotations(withoutHidden(h, o.GetAnnotations()))
		o.SetLabels(withoutHidden(h, o.GetLabels()))
	})
}

// Restore puts back what the platform stored, and drops whatever the tenant
// supplied under a hidden key.
//
// ⭐ The order matters and is the whole point: the tenant's value never survives,
// and the stored value always does. A tenant that has learned a key exists
// cannot use that knowledge to set it -- which is what keeps
// kubezoo.io/cluster-ip from being a way to make kubezoo report an address of
// the tenant's choosing, and will keep the next such key safe without anyone
// remembering to write a guard for it.
//
// old may be nil, which is a create: there is nothing to put back, and dropping
// is all that happens.
func (h *HiddenMetadata) Restore(obj, old runtime.Object) {
	var storedAnnotations, storedLabels map[string]string
	if old != nil {
		if accessor, ok := old.(metav1.Object); ok {
			storedAnnotations = accessor.GetAnnotations()
			storedLabels = accessor.GetLabels()
		}
	}
	h.forEachObject(obj, func(o metav1.Object) {
		o.SetAnnotations(restoreHidden(h, o.GetAnnotations(), storedAnnotations))
		o.SetLabels(restoreHidden(h, o.GetLabels(), storedLabels))
	})
}

// forEachObject applies fn to obj, or to every item if obj is a list.
//
// ⚠️ Lists are not covered by the single-object case, and they are the paths
// that matter most: `kubectl get`, an informer's initial LIST, an operator's
// cache fill. Leaving them out would hide the metadata everywhere except where
// it is read in bulk.
func (h *HiddenMetadata) forEachObject(obj runtime.Object, fn func(metav1.Object)) {
	if h == nil || obj == nil {
		return
	}
	if items, err := meta.ExtractList(obj); err == nil {
		for _, item := range items {
			if o, ok := item.(metav1.Object); ok {
				fn(o)
			}
		}
		return
	}
	if o, ok := obj.(metav1.Object); ok {
		fn(o)
	}
}

func withoutHidden(h *HiddenMetadata, in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	var out map[string]string
	for k, v := range in {
		if h.hides(k) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(in))
		}
		out[k] = v
	}
	return out
}

func restoreHidden(h *HiddenMetadata, submitted, stored map[string]string) map[string]string {
	out := withoutHidden(h, submitted)
	for k, v := range stored {
		if !h.hides(k) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}
