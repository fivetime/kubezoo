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
	"encoding/base64"
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// clusterScopedContinueVersion identifies this cursor, so that upstream's own
// token and a cursor from the namespace fan-out are both rejected rather than
// misread.
const clusterScopedContinueVersion = "kubezoo.io/cluster-scoped-v1"

// clusterScopedContinue wraps upstream's continue token for a cluster-scoped
// list.
//
// ⚠️ Upstream's token used to be handed to the tenant verbatim, and it is not
// opaque: for a cluster-scoped resource it carries the name of the object the
// page stopped at, base64 of JSON, in the clear. A tenant listing
// persistentvolumes with limit=1 got back zero items -- the prefix filter drops
// the object -- and a token reading
// {"v":"meta.k8s.io/v1","rv":4242,"start":"222222-payroll-db\x00"}. client-go
// follows a continue token automatically, so an ordinary paged list walked the
// entire cluster one object at a time and returned every other tenant's
// cluster-scoped object names, and with them the live tenant ids and CRD group
// names.
//
// Wrapping rather than blanking, because these lists filter AFTER fetching: a
// page can legitimately come back empty with more to read, and dropping the
// token there would silently truncate the answer, which is worse than leaking
// it. design-list-fanout-cn.md §3.1③ already states the rule -- kubezoo's own
// cursor must be distinguishable from upstream's and must never be passed
// through -- it had simply never been applied to the response side of the
// cluster-scoped path.
type clusterScopedContinue struct {
	APIVersion string `json:"v"`
	// Upstream is the token upstream gave us, which the tenant never sees.
	Upstream string `json:"u"`
}

func encodeClusterScopedContinue(upstreamToken string) (string, error) {
	raw, err := json.Marshal(clusterScopedContinue{
		APIVersion: clusterScopedContinueVersion,
		Upstream:   upstreamToken,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeClusterScopedContinue reads back a cursor this proxy issued. Anything
// else is refused rather than forwarded: upstream's own token means a position
// in the whole cluster's range, and passing one on is how the leak worked.
func decodeClusterScopedContinue(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", apierrors.NewBadRequest("the continue parameter is not one this server issued")
	}
	var c clusterScopedContinue
	if err := json.Unmarshal(raw, &c); err != nil || c.APIVersion != clusterScopedContinueVersion {
		return "", apierrors.NewBadRequest("the continue parameter is not one this server issued")
	}
	return c.Upstream, nil
}

// hideUpstreamListCursor rewrites the list metadata a cluster-scoped read hands
// back: the continue token is wrapped, and remainingItemCount is dropped because
// it counts objects across every tenant.
func hideUpstreamListCursor(list *unstructured.UnstructuredList) error {
	if list == nil {
		return nil
	}
	unstructured.RemoveNestedField(list.Object, "metadata", "remainingItemCount")

	token := list.GetContinue()
	if token == "" {
		return nil
	}
	wrapped, err := encodeClusterScopedContinue(token)
	if err != nil {
		return err
	}
	list.SetContinue(wrapped)
	return nil
}
