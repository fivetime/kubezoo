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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
