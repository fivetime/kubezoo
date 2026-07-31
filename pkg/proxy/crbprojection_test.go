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

package proxy

import (
	"testing"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/fields"
)

// TestProjectedFieldSelectorNamesTheRecord guards the one call site the
// ListOptionScope fix structurally could not reach.
//
// ⚠️ The field selector went through untouched. util.ConvertInternalListOptions
// is handed the INNER storage's scope -- namespaced RoleBinding -- so it
// correctly leaves metadata.name alone, while the object the client asked about
// is a cluster-scoped ClusterRoleBinding whose record is named
// ProjectedBindingName(name). Both failures were silent:
//
//   - `metadata.name=my-crb` matched nothing, so a single-object informer watched
//     an empty world forever while `kubectl get clusterrolebinding my-crb` worked.
//   - `metadata.name!=keepme` matched EVERYTHING including keepme's own record,
//     because no record is literally named keepme -- so a DeleteCollection
//     carrying it deleted the object it was told to spare.
func TestProjectedFieldSelectorNamesTheRecord(t *testing.T) {
	cases := []struct {
		name     string
		selector string
		want     string
		wantErr  bool
	}{
		{
			name:     "the single-object informer's selector",
			selector: "metadata.name=my-crb",
			want:     "metadata.name=kubezoo:clusterrolebinding:my-crb",
		},
		{
			name:     "the negative form, which used to match the object it excludes",
			selector: "metadata.name!=keepme",
			want:     "metadata.name!=kubezoo:clusterrolebinding:keepme",
		},
		{
			name:     "a namespace on a cluster-scoped object means nothing",
			selector: "metadata.namespace=default",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := fields.ParseSelector(tc.selector)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.selector, err)
			}
			scoped, err := withProjectedSelector(&metainternalversion.ListOptions{FieldSelector: selector})
			if tc.wantErr {
				if err == nil {
					t.Errorf("accepted %q, which cannot be true of any record", tc.selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("rewriting %q: %v", tc.selector, err)
			}
			if got := scoped.FieldSelector.String(); got != tc.want {
				t.Errorf("field selector = %q, want %q", got, tc.want)
			}
			// The label narrowing must survive the rewrite.
			if scoped.LabelSelector == nil || scoped.LabelSelector.Empty() {
				t.Error("the record label selector was lost")
			}
		})
	}
}
