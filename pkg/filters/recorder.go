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

package filters

import (
	"bytes"
	"net/http"

	"k8s.io/klog"
)

// responseRecorder captures what a handler would have written, so a filter can
// combine it with something else before answering.
//
// The OpenAPI v3 index needs this: half of it is kubezoo's own, produced by the
// handler further down the chain, and half comes from upstream. Asking the
// chain and then editing the answer keeps kubezoo's half authoritative without
// this filter having to know how it is built.
type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

var _ http.ResponseWriter = &responseRecorder{}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header: http.Header{},
		body:   &bytes.Buffer{},
		status: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *responseRecorder) WriteHeader(status int) { r.status = status }

// replay writes the captured response through unchanged, for when the filter
// decides it has nothing to add -- or nothing it can trust.
func (r *responseRecorder) replay(w http.ResponseWriter) {
	for name, values := range r.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(r.status)
	if _, err := w.Write(r.body.Bytes()); err != nil {
		klog.Errorf("failed to replay recorded response: %v", err)
	}
}

func recordResponse(handler http.Handler, r *http.Request) *responseRecorder {
	recorder := newResponseRecorder()
	handler.ServeHTTP(recorder, r)
	return recorder
}
