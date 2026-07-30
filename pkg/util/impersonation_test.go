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

package util

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

// forwarding builds the context a forwarded request carries: who authenticated,
// which tenant they resolved to, and what they are asking for.
func forwarding(tenantID, userName string, groups []string, info *request.RequestInfo) context.Context {
	ctx := context.Background()
	extra := map[string][]string{}
	if tenantID != "" {
		extra[TenantIDKey] = []string{tenantID}
	}
	ctx = request.WithUser(ctx, &user.DefaultInfo{Name: userName, Groups: groups, Extra: extra})
	if info != nil {
		ctx = request.WithRequestInfo(ctx, info)
	}
	return ctx
}

func clusterRoleRequest(verb string) *request.RequestInfo {
	return &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              verb,
		APIGroup:          "rbac.authorization.k8s.io",
		Resource:          "clusterroles",
	}
}

// TestImpersonationGroups covers which requests carry a tenant's cluster-scoped
// permissions upstream, and which carry the exemption that lets a tenant write a
// ClusterRole. Each case is a way the isolation would come apart, so the names
// say what goes wrong rather than what the input is.
func TestImpersonationGroups(t *testing.T) {
	tests := []struct {
		what     string
		tenantID string
		userName string
		groups   []string
		info     *request.RequestInfo
		want     []string
	}{
		{
			what:     "the tenant's own credential carries the proxied group, or nothing it does works",
			tenantID: "111111",
			userName: "111111-admin",
			groups:   []string{"system:authenticated"},
			want:     []string{"system:authenticated", "kubezoo:proxied:111111"},
		},
		{
			what:     "a ServiceAccount does not, or every workload becomes cluster-scoped",
			tenantID: "111111",
			userName: "system:serviceaccount:111111-default:app",
			groups:   []string{"system:serviceaccounts"},
			want:     []string{"system:serviceaccounts"},
		},
		{
			what:     "a mis-issued credential does not get a kubezoo group forwarded",
			tenantID: "111111",
			userName: "system:serviceaccount:111111-default:app",
			groups:   []string{"system:serviceaccounts", "kubezoo:proxied:222222", "kubezoo:role-author"},
			want:     []string{"system:serviceaccounts"},
		},
		{
			what:     "and one carrying its own tenant's group gets exactly one copy",
			tenantID: "111111",
			userName: "111111-admin",
			groups:   []string{"kubezoo:proxied:111111"},
			want:     []string{"kubezoo:proxied:111111"},
		},
		{
			what:     "a request with no tenant carries nothing",
			tenantID: "",
			userName: "111111-admin",
			groups:   []string{"system:authenticated"},
			want:     []string{"system:authenticated"},
		},
		{
			what:     "writing a ClusterRole carries the escalate exemption",
			tenantID: "111111",
			userName: "111111-admin",
			info:     clusterRoleRequest("create"),
			want:     []string{"kubezoo:proxied:111111", "kubezoo:role-author"},
		},
		{
			what:     "so does patching one, or the same role could be written a second way",
			tenantID: "111111",
			userName: "111111-admin",
			info:     clusterRoleRequest("patch"),
			want:     []string{"kubezoo:proxied:111111", "kubezoo:role-author"},
		},
		{
			what:     "reading one does not, since the check runs on the way in",
			tenantID: "111111",
			userName: "111111-admin",
			info:     clusterRoleRequest("get"),
			want:     []string{"kubezoo:proxied:111111"},
		},
		{
			what:     "a ClusterRoleBinding never does -- that exemption is what keeps a role confined",
			tenantID: "111111",
			userName: "111111-admin",
			info: &request.RequestInfo{
				IsResourceRequest: true,
				Verb:              "create",
				APIGroup:          "rbac.authorization.k8s.io",
				Resource:          "clusterrolebindings",
			},
			want: []string{"kubezoo:proxied:111111"},
		},
		{
			what:     "nor does a ServiceAccount writing a ClusterRole",
			tenantID: "111111",
			userName: "system:serviceaccount:111111-default:app",
			info:     clusterRoleRequest("create"),
			want:     []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.what, func(t *testing.T) {
			ctx := forwarding(test.tenantID, test.userName, test.groups, test.info)
			got := ImpersonationGroups(ctx, test.userName, test.groups)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}
