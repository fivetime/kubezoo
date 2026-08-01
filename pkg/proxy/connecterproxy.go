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
	"context"
	"net/http"
	"net/url"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/proxy"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/pkg/registry/rest"
	api "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

var upgradeableMethods = []string{"GET", "POST"}

const (
	LogSubresource = "log"
	Namespace      = "namespaces"
)

type ConnecterProxy struct {
	transport      http.RoundTripper
	upstreamMaster *url.URL
}

var _ = rest.Connecter(&ConnecterProxy{})

func (cp *ConnecterProxy) New() runtime.Object {
	return &api.Pod{}
}

// Connect proxy the connection to the upstream server if shoud proxy.
func (cp *ConnecterProxy) Connect(ctx context.Context, id string, options runtime.Object, r rest.Responder) (http.Handler, error) {
	// TODO (chao.zheng): validate the input args?
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestInfo, ok := apirequest.RequestInfoFrom(req.Context())
		if !ok {
			http.Error(w, "invalid request info", http.StatusInternalServerError)
			return
		}

		// connect is either a pods/log subresource request or
		// a upgrade stream request
		if requestInfo.Subresource != LogSubresource &&
			!httpstream.IsUpgradeRequest(req) {
			http.Error(w, "connect resource request is not upgrade request",
				http.StatusInternalServerError)
			return
		}

		cp.connect(req, w)
	}), nil
}

// connect implement the converting of path and redirect the connection to upstream server.
func (cp *ConnecterProxy) connect(req *http.Request, w http.ResponseWriter) {
	// extract tenant from context
	tenant, ok := util.TenantFrom(req.Context())
	if !ok {
		http.Error(w, "invalid tenant info", http.StatusInternalServerError)
		return
	}
	// extract userInfo from context
	userInfo, ok := apirequest.UserFrom(req.Context())
	if !ok {
		http.Error(w, "no User found in context", http.StatusInternalServerError)
		return
	}
	u := *req.URL
	var namespaceTransformed bool

	// transform namespace for pod
	paths := strings.Split(u.Path, "/")
	for i, p := range paths {
		if p == Namespace && i+1 < len(paths) {
			// Idempotent, which AddTenantIDPrefix is not: an in-cluster workload
			// addresses its own namespace by whatever its projected
			// serviceaccount/namespace file says, and that carries the upstream
			// name. Prefixing it a second time turned logs and exec into a
			// NotFound for 111111-111111-default.
			paths[i+1] = util.UpstreamNamespace(tenant, paths[i+1])
			namespaceTransformed = true
			break
		}
	}
	if !namespaceTransformed {
		http.Error(w, "namespace not contained in proxy connect url",
			http.StatusInternalServerError)
		return
	}
	u.Path = strings.Join(paths, "/")

	// set proxy upstream server
	u.Host = cp.upstreamMaster.Host
	u.Scheme = cp.upstreamMaster.Scheme

	// need transform request host also, or it will keep original request host. upstream apiserver can't handle correctly
	req.Host = cp.upstreamMaster.Host

	// set impersonate header
	if req.Header == nil {
		req.Header = make(map[string][]string)
	}
	// Anything the client sent goes first; only what kubezoo sets may reach
	// upstream. See util.DropClientImpersonation.
	util.DropClientImpersonation(req.Header)
	req.Header[authenticationv1.ImpersonateUserHeader] = []string{userInfo.GetName()}
	req.Header[authenticationv1.ImpersonateGroupHeader] = util.ImpersonationGroups(
		req.Context(), userInfo.GetName(), userInfo.GetGroups())

	// Decorate the response writer to record status and length, then hand it to
	// the wrapper that puts back the interfaces the decoration hides.
	//
	// This used to pick between two decorators by testing the inner writer for
	// CloseNotifier, Flusher and Hijacker together, and the branch that fired
	// when all three were present did not stream: `kubectl logs -f` produced
	// nothing at all while the same follow against upstream streamed normally,
	// and a snapshot `logs` through the same path was fine. `kubectl exec` was
	// unaffected, since it upgrades the connection rather than streaming a body.
	//
	// The most likely culprit is that decorator's CloseNotify, which forwards a
	// deprecated interface the apiserver's writer chain no longer means the same
	// way, and the proxy reads it as the client having gone away. Not chased
	// further, because the fix is not to repair that branch: WrapForHTTP1Or2 is
	// upstream's answer to exactly this problem, preserving Flusher and
	// CloseNotifier for both HTTP/1 and HTTP/2 and adding Hijacker only when the
	// inner writer really has it.
	rw := responsewriter.WrapForHTTP1Or2(&ResponseWriterDelegator{ResponseWriter: w})
	// proxy logic
	proxyHandler := proxy.NewUpgradeAwareHandler(&u, cp.transport, false, false, &responder{w: w})
	proxyHandler.ServeHTTP(rw, req)

}

// ResponseWriterDelegator interface wraps http.ResponseWriter to additionally record content-length, status-code, etc.
type ResponseWriterDelegator struct {
	http.ResponseWriter

	status      int
	written     int64
	wroteHeader bool
}

// WriteHeader sends an HTTP response header with the provided status code.
func (r *ResponseWriterDelegator) WriteHeader(code int) {
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

// Write writes the data to the connection as part of an HTTP reply.
func (r *ResponseWriterDelegator) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap returns the writer this one decorates, which is what
// responsewriter.WrapForHTTP1Or2 needs in order to work out which of Flusher,
// CloseNotifier and Hijacker the underlying connection actually supports.
func (r *ResponseWriterDelegator) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Status return the http response status.
func (r *ResponseWriterDelegator) Status() int {
	return r.status
}

// ContentLength return the length of http response content.
func (r *ResponseWriterDelegator) ContentLength() int {
	return int(r.written)
}

// responder implements rest.Responder for assisting a connector in writing
// objects or errors.
type responder struct {
	w http.ResponseWriter
}

func (r *responder) Error(_ http.ResponseWriter, _ *http.Request, err error) {
	http.Error(r.w, err.Error(), http.StatusInternalServerError)
}

func (cp *ConnecterProxy) NewConnectOptions() (runtime.Object, bool, string) {
	return &api.PodExecOptions{}, false, ""
}

func (cp *ConnecterProxy) ConnectMethods() []string {
	return upgradeableMethods
}

// Destroy releases resources held by the storage. rest.Storage grew this method
// in Kubernetes 1.26; the connecter holds no resources of its own.
func (cp *ConnecterProxy) Destroy() {}

func NewConnecterProxy(transport http.RoundTripper, upstreamMaster *url.URL) (rest.Storage, error) {
	return &ConnecterProxy{
		transport:      transport,
		upstreamMaster: upstreamMaster,
	}, nil
}
