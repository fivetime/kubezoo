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
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/apis/core"

	"github.com/kubewharf/kubezoo/pkg/common"
	"github.com/kubewharf/kubezoo/pkg/convert"
)

// carriesPodSpec reports whether a type embeds a core.PodSpec anywhere.
//
// Structural rather than calling PodSpecOf on a fresh object: a
// ReplicationController holds its template behind a nil pointer until it is
// populated, so that check would have said no. More to the point, asking the
// type means a newly served workload kind is caught even though PodSpecOf --
// the thing under test -- does not know about it yet.
func carriesPodSpec(t reflect.Type, seen map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return false
	}
	if t == reflect.TypeOf(core.PodSpec{}) {
		return true
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		if carriesPodSpec(t.Field(i).Type, seen) {
			return true
		}
	}
	return false
}

// TestEveryServedPodSpecIsCovered keeps the platform-owned field stripping in
// step with what is actually served.
//
// runtimeClassName and priorityClassName live in PodSpec, and PodSpec is
// embedded in nine served kinds. Covering Pod alone leaves a Deployment -- the
// path almost everything really takes -- untouched, while looking done. The Node
// exemption was in three places and only one of them was ever found by reading;
// this closes that loop from the side that can see both the served table and the
// convertor registry.
func TestEveryServedPodSpecIsCovered(t *testing.T) {
	covered := map[schema.GroupKind]bool{}
	for _, gk := range convert.PlatformFieldGroupKinds() {
		covered[gk] = true
	}

	var missing []string
	seen := map[schema.GroupKind]bool{}
	collect := func(g common.APIGroupConfig) {
		for _, resources := range g.StorageConfigs {
			for _, storageConfig := range resources {
				if storageConfig.NewFunc == nil || storageConfig.Subresource != "" {
					continue
				}
				obj := storageConfig.NewFunc()
				if !carriesPodSpec(reflect.TypeOf(obj), map[reflect.Type]bool{}) {
					continue
				}
				if convert.PodSpecOf(obj) == nil && storageConfig.Kind.Kind != "ReplicationController" {
					t.Errorf("%s carries a pod spec but PodSpecOf does not reach it",
						storageConfig.Kind.Kind)
				}
				gk := schema.GroupKind{Group: g.Group, Kind: storageConfig.Kind.Kind}
				if seen[gk] {
					continue
				}
				seen[gk] = true
				if !covered[gk] {
					missing = append(missing, gk.String())
				}
			}
		}
	}
	collect(legacyGroup)
	for _, g := range nonLegacyGroups {
		collect(g)
	}

	sort.Strings(missing)
	for _, gk := range missing {
		t.Errorf("%s is served and carries a pod spec, but its runtimeClassName and "+
			"priorityClassName are not dropped; a tenant can name any of the platform's "+
			"classes through it", gk)
	}

	// The other direction: an entry naming a kind that is not served would sit
	// here looking like coverage while doing nothing.
	for gk := range covered {
		if gk.Kind == "Ingress" {
			continue // covered for ingressClassName, not for a pod spec
		}
		if !seen[gk] {
			t.Errorf("%s is listed as covered but is not served with a pod spec", gk)
		}
	}
}
