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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// maxFannedNamespaces bounds how many namespaces one tenant's cross-namespace
// list will span.
//
// The cursor carries an index into the namespace list, and the list has to be
// re-read on every page, so the work grows with it. The limit exists so that a
// tenant with an unreasonable number of namespaces gets an error naming the
// problem rather than a listing that is quietly short -- silent truncation of a
// read is the failure that gets believed.
const maxFannedNamespaces = 500

// fanoutContinue is our own cursor for a list spread over a tenant's namespaces.
//
// Deliberately the same shape as the apiserver's own continue token, with one
// more coordinate. Upstream's carries the revision it started at and where it
// got to; ours carries the revision, which namespace it got to, and where inside
// that namespace -- upstream's own cursor, passed straight back to it.
type fanoutContinue struct {
	APIVersion string `json:"v"`
	// ResourceVersion pins every read in the whole pagination to one snapshot,
	// which is what makes this equivalent to a single list rather than a
	// sequence of unrelated ones. Objects and namespaces created part way
	// through are not visible, exactly as they are not to a native list.
	ResourceVersion string `json:"rv"`
	NamespaceIndex  int    `json:"nsi"`
	// Inner is the upstream continue token for the namespace we stopped in,
	// empty when we stopped cleanly on a namespace boundary.
	Inner string `json:"inner,omitempty"`
}

const fanoutContinueVersion = "kubezoo/v1"

func encodeFanoutContinue(c fanoutContinue) (string, error) {
	c.APIVersion = fanoutContinueVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeFanoutContinue reads back a cursor this proxy issued.
//
// A token we do not recognise is refused rather than passed upstream. Upstream's
// own token means something different -- a position in a single cluster-wide
// range -- and handing it on would silently list the wrong thing.
func decodeFanoutContinue(token string) (fanoutContinue, error) {
	var c fanoutContinue
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return c, apierrors.NewBadRequest("the continue parameter is not one this server issued")
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.APIVersion != fanoutContinueVersion {
		return c, apierrors.NewBadRequest("the continue parameter is not one this server issued")
	}
	if c.ResourceVersion == "" || c.NamespaceIndex < 0 {
		return c, apierrors.NewBadRequest("the continue parameter is not one this server issued")
	}
	return c, nil
}

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// asTenantAdmin returns a context that enumerates the tenant's namespaces as the
// tenant rather than as whoever asked.
//
// Which namespaces a tenant has is kubezoo's own business: it is how a request
// with no namespace gets turned into requests that have one. Doing it as the
// caller made it the caller's business, and a ServiceAccount does not hold
// namespaces at cluster scope -- measured, an operator's cluster-wide list of
// secrets came back "cannot list resource namespaces at the cluster scope",
// naming an object it never asked for.
//
// This grants the caller nothing. Every read of the objects themselves still
// goes up as the caller, so a namespace it may not read is refused, and the
// refusal fails the whole list exactly as a cluster-wide list it is not entitled
// to would fail upstream. The tenant's namespace names are all it could learn,
// and it is inside that tenant.
func asTenantAdmin(ctx context.Context, tenantID string) context.Context {
	caller, ok := request.UserFrom(ctx)
	if !ok {
		return ctx
	}
	extra := map[string][]string{}
	for key, value := range caller.GetExtra() {
		extra[key] = value
	}
	extra[util.TenantIDKey] = []string{tenantID}
	return request.WithUser(ctx, &user.DefaultInfo{
		Name:  util.TenantAdminUser(tenantID),
		Extra: extra,
	})
}

// tenantNamespaces returns the tenant's namespaces in a stable order, together
// with the revision they were read at.
//
// Pinning this read matters as much as pinning the object reads. Left unpinned,
// the second page could see a namespace that did not exist when the first page
// was served, and the cursor's index would point at a different namespace than
// it did when it was written -- which is the way this actually goes wrong, not
// the objects themselves.
func (tp *tenantProxy) tenantNamespaces(ctx context.Context, tenantID, atRevision string) ([]string, string, error) {
	ctx = asTenantAdmin(ctx, tenantID)
	options := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", common.TenantNamespaceLabelKey, tenantID),
	}
	if atRevision != "" {
		options.ResourceVersion = atRevision
		options.ResourceVersionMatch = metav1.ResourceVersionMatchExact
	}
	list, err := tp.dynamicClient.Resource(namespaceGVR).List(ctx, options)
	if err != nil {
		return nil, "", err
	}
	if len(list.Items) > maxFannedNamespaces {
		return nil, "", apierrors.NewBadRequest(fmt.Sprintf(
			"listing across %d namespaces at once is more than this server will do (limit %d); "+
				"narrow the request to one namespace",
			len(list.Items), maxFannedNamespaces))
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	sort.Strings(names)
	return names, list.GetResourceVersion(), nil
}

// listAcrossNamespaces serves a list with no namespace in the request by reading
// each of the tenant's namespaces in turn.
//
// The alternative, and what this replaces, is one list of every object of this
// kind in the cluster followed by throwing away the ones that belong to other
// tenants. That is correct and costs the whole cluster on every request from
// every tenant. Here the cost is the tenant's own namespaces, and each read is
// scoped, which the watch cache indexes.
//
// Equivalence with a native list rests entirely on the pinned revision. Native
// pagination is a snapshot -- the continue token carries the revision and the
// store refuses a chunk from any other -- so the same pinning here gives the
// same guarantees: nothing created mid-pagination appears, the returned
// resourceVersion is real and a watch can start from it, and an expired snapshot
// surfaces as the 410 clients already handle.
func (tp *tenantProxy) listAcrossNamespaces(ctx context.Context, options *metav1.ListOptions,
	tenantID string) (*unstructured.UnstructuredList, error) {

	cursor := fanoutContinue{}
	if options.Continue != "" {
		var err error
		if cursor, err = decodeFanoutContinue(options.Continue); err != nil {
			return nil, err
		}
	}

	namespaces, revision, err := tp.tenantNamespaces(ctx, tenantID, cursor.ResourceVersion)
	if err != nil {
		return nil, err
	}
	if cursor.ResourceVersion == "" {
		cursor.ResourceVersion = revision
	}
	if cursor.NamespaceIndex > len(namespaces) {
		return nil, apierrors.NewBadRequest("the continue parameter points past the end of this tenant's namespaces")
	}

	gv := tp.upstreamGroupVersion(tenantID)
	resource := tp.dynamicClient.Resource(gv.WithResource(tp.resource))

	gathered := &unstructured.UnstructuredList{Items: make([]unstructured.Unstructured, 0)}
	remaining := options.Limit
	inner := cursor.Inner

	for index := cursor.NamespaceIndex; index < len(namespaces); index++ {
		perNamespace := *options
		perNamespace.Continue = inner
		if inner == "" {
			// Starting this namespace fresh, so pin it to the snapshot.
			perNamespace.ResourceVersion = cursor.ResourceVersion
			perNamespace.ResourceVersionMatch = metav1.ResourceVersionMatchExact
		} else {
			// Resuming inside a namespace. The apiserver refuses a continue
			// token alongside resourceVersionMatch -- "resourceVersionMatch is
			// forbidden when continue is provided" -- because the token already
			// carries the revision it was issued at. Which is the same snapshot,
			// so the pinning holds either way.
			perNamespace.ResourceVersion = ""
			perNamespace.ResourceVersionMatch = ""
		}
		if options.Limit > 0 {
			perNamespace.Limit = remaining
		}

		page, err := resource.Namespace(namespaces[index]).List(ctx, perNamespace)
		if err != nil {
			// A 410 for an expired snapshot travels back untouched: it means
			// the same thing it means for a native list, and clients restart.
			return nil, err
		}
		if gathered.Object == nil {
			gathered.Object = page.Object
		}
		gathered.Items = append(gathered.Items, page.Items...)
		inner = page.GetContinue()

		if options.Limit <= 0 {
			continue
		}
		remaining = options.Limit - int64(len(gathered.Items))
		if remaining > 0 {
			// This namespace is exhausted; carry on into the next one with a
			// fresh cursor.
			inner = ""
			continue
		}
		// Full page. Stop where we are, and say where that is: still inside
		// this namespace if it has more, otherwise at the start of the next.
		next := fanoutContinue{ResourceVersion: cursor.ResourceVersion, NamespaceIndex: index, Inner: inner}
		if inner == "" {
			next.NamespaceIndex = index + 1
		}
		if next.NamespaceIndex >= len(namespaces) && inner == "" {
			break // nothing left after all
		}
		token, err := encodeFanoutContinue(next)
		if err != nil {
			return nil, err
		}
		gathered.SetContinue(token)
		gathered.SetResourceVersion(cursor.ResourceVersion)
		return gathered, nil
	}

	gathered.SetContinue("")
	gathered.SetResourceVersion(cursor.ResourceVersion)
	return gathered, nil
}
