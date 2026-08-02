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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

// TestEveryServedPodCarrierIsPlaced walks what the server actually installs and
// checks that everything carrying a pod template is registered for placement.
//
// ⚠️ This is the check the other two only look like. TestEveryPodCarrierIsPlaced
// walks a list written in its own file, and TestPlacementCoversWhatIsRegistered
// compares PodCarryingKinds against a map written in its own file -- neither of
// them ever looks at apigroups.go. A kind served but left out of
// PodCarryingKinds keeps whatever placement the tenant asked for, silently: no
// error, no log, nothing that fails to build.
//
// ⭐ The pod template is found by REFLECTION rather than by asking
// pkg/convert, deliberately. Reusing the type switch that decides placement to
// decide what needs placing is circular -- a newly served kind missing from the
// switch would be reported as carrying no template, and the test would agree with
// the bug. Walking the struct is independent of it.
func TestEveryServedPodCarrierIsPlaced(t *testing.T) {
	placed := map[schema.GroupKind]bool{}
	for _, gk := range convert.PodCarryingKinds {
		placed[gk] = true
	}

	groups := append([]apiconfig.APIGroupConfig{legacyGroup}, nonLegacyGroups...)
	checked := 0
	for _, group := range groups {
		for _, byResource := range group.StorageConfigs {
			for resource, config := range byResource {
				if config.NewFunc == nil || config.Subresource != "" {
					// A subresource carries its parent's kind; the parent is what
					// gets registered, and pkg/convert lets the payload types
					// through explicitly.
					continue
				}
				if !carriesPodSpec(reflect.TypeOf(config.NewFunc()), 0) {
					continue
				}
				checked++
				gk := schema.GroupKind{Group: config.Kind.Group, Kind: config.Kind.Kind}
				if !placed[gk] {
					t.Errorf("%s (resource %q) carries a pod template and is served, but is not "+
						"in convert.PodCarryingKinds -- every pod it produces keeps whatever "+
						"placement the tenant asked for, and nothing anywhere says so",
						gk, resource)
				}
			}
		}
	}

	// ⚠️ A reflection walk that finds nothing would pass this test in silence,
	// which is the failure mode it exists to prevent.
	if checked < len(convert.PodCarryingKinds) {
		t.Errorf("only %d served kinds were found to carry a pod template, but %d are "+
			"registered for placement; the walk is not reaching what it should",
			checked, len(convert.PodCarryingKinds))
	}
}

// carriesPodSpec reports whether a core.PodSpec is reachable from this type.
//
// Depth-bounded because the object graph has cycles through pointers and the
// answer is always within a few levels: Pod.Spec, Deployment.Spec.Template.Spec,
// CronJob.Spec.JobTemplate.Spec.Template.Spec.
func carriesPodSpec(t reflect.Type, depth int) bool {
	if depth > 5 || t == nil {
		return false
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == reflect.TypeOf(core.PodSpec{}) {
		return true
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		if carriesPodSpec(t.Field(i).Type, depth+1) {
			return true
		}
	}
	return false
}
