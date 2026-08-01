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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestFanoutContinueRoundTrips -- the cursor is the whole correctness argument
// for paging across namespaces, so it has to come back exactly as it went in.
func TestFanoutContinueRoundTrips(t *testing.T) {
	for _, want := range []fanoutContinue{
		{ResourceVersion: "12345", NamespaceIndex: 0},
		{ResourceVersion: "12345", NamespaceIndex: 7, Inner: "eyJ2IjoibWV0YS5rOHMuaW8vdjEifQ"},
		{ResourceVersion: "1", NamespaceIndex: 499},
	} {
		token, err := encodeFanoutContinue(want)
		if err != nil {
			t.Fatalf("encoding %+v: %v", want, err)
		}
		got, err := decodeFanoutContinue(token)
		if err != nil {
			t.Fatalf("decoding %q: %v", token, err)
		}
		want.APIVersion = fanoutContinueVersion
		if got != want {
			t.Errorf("round trip changed the cursor:\n got %+v\nwant %+v", got, want)
		}
	}
}

// TestForeignContinueIsRefused -- upstream's continue token means a position in
// one cluster-wide range, ours means a namespace and a position inside it.
// Passing one on as the other would list the wrong thing and say nothing about
// it, so anything we did not issue is refused.
func TestForeignContinueIsRefused(t *testing.T) {
	for _, name := range []string{
		"upstream's own token",
		"not base64 at all",
		"valid base64, wrong contents",
		"ours but with no revision",
	} {
		var token string
		switch name {
		case "upstream's own token":
			// {"v":"meta.k8s.io/v1","rv":123,"start":"/pods/x"}
			token = "eyJ2IjoibWV0YS5rOHMuaW8vdjEiLCJydiI6MTIzLCJzdGFydCI6Ii9wb2RzL3gifQ"
		case "not base64 at all":
			token = "!!!!"
		case "valid base64, wrong contents":
			token = "eyJoZWxsbyI6IndvcmxkIn0"
		case "ours but with no revision":
			token, _ = encodeFanoutContinue(fanoutContinue{NamespaceIndex: 3})
		}
		t.Run(name, func(t *testing.T) {
			if _, err := decodeFanoutContinue(token); err == nil {
				t.Error("accepted a continue token this server did not issue")
			} else if !apierrors.IsBadRequest(err) {
				t.Errorf("refused it, but not as a bad request: %v", err)
			}
		})
	}
}

// TestClusterScopedCursorHidesUpstreamsToken guards a cross-tenant enumeration.
//
// ⚠️ A cluster-scoped list filters AFTER fetching, so it reads over the whole
// cluster's key range -- and upstream's continue token is not opaque: it carries
// the name of the object the page stopped at, base64 of JSON, in the clear.
// `GET /api/v1/persistentvolumes?limit=1` returned zero items and a token
// decoding to {"v":"meta.k8s.io/v1","rv":4242,"start":"222222-payroll-db\x00"}.
// client-go follows a continue token automatically, so an ordinary paged list
// walked the whole cluster one object per request and returned every other
// tenant's cluster-scoped object names, live tenant ids and CRD group names
// included.
func TestClusterScopedCursorHidesUpstreamsToken(t *testing.T) {
	const upstreamToken = `eyJ2IjoibWV0YS5rOHMuaW8vdjEiLCJydiI6NDI0Miwic3RhcnQiOiIyMjIyMjItcGF5cm9sbC1kYiJ9`

	list := &unstructured.UnstructuredList{Object: map[string]interface{}{}}
	list.SetContinue(upstreamToken)
	unstructured.SetNestedField(list.Object, int64(918), "metadata", "remainingItemCount")

	if err := hideUpstreamListCursor(list); err != nil {
		t.Fatalf("hiding the cursor: %v", err)
	}

	handed := list.GetContinue()
	if handed == upstreamToken {
		t.Fatal("upstream's continue token was handed to the tenant verbatim; it names " +
			"another tenant's object and client-go will follow it across the whole cluster")
	}
	if strings.Contains(handed, upstreamToken) {
		t.Error("the token was wrapped but upstream's is still readable inside it")
	}
	if _, found, _ := unstructured.NestedInt64(list.Object, "metadata", "remainingItemCount"); found {
		t.Error("remainingItemCount survived; it counts every tenant's objects")
	}

	// And it has to round trip, or a legitimate paged read is truncated -- which
	// is a worse failure than the leak.
	back, err := decodeClusterScopedContinue(handed)
	if err != nil {
		t.Fatalf("the cursor did not decode: %v", err)
	}
	if back != upstreamToken {
		t.Errorf("round trip changed the upstream token: %q != %q", back, upstreamToken)
	}

	// Upstream's own token, presented by a client that got it some other way, is
	// refused rather than forwarded.
	if _, err := decodeClusterScopedContinue(upstreamToken); err == nil {
		t.Error("upstream's own token was accepted and would have been forwarded")
	}
}
