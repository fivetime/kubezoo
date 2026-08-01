package convert

import (
	"testing"

	"github.com/fivetime/kubezoo-contract/pkg/util"
	authzinternal "k8s.io/kubernetes/pkg/apis/authorization"
)

func TestZZZAccessReviewInClusterNamespace(t *testing.T) {
	tr := NewAccessReviewTransformer()
	// what an in-cluster pod reads from /var/run/secrets/.../namespace is the UPSTREAM name
	inClusterNS := util.UpstreamNamespace("111111", "default") // 111111-default
	ssar := &authzinternal.SelfSubjectAccessReview{
		Spec: authzinternal.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzinternal.ResourceAttributes{
				Namespace: inClusterNS, Verb: "list", Resource: "secrets",
			},
		},
	}
	if _, err := tr.Forward(ssar, "111111"); err != nil {
		t.Fatal(err)
	}
	t.Logf("in-cluster ns %q -> SSAR asks upstream about %q", inClusterNS, ssar.Spec.ResourceAttributes.Namespace)
	t.Logf("meanwhile the object's own namespace would go to %q via UpstreamNamespace",
		util.UpstreamNamespace("111111", inClusterNS))

	lsar := &authzinternal.LocalSubjectAccessReview{
		Spec: authzinternal.SubjectAccessReviewSpec{
			ResourceAttributes: &authzinternal.ResourceAttributes{Namespace: inClusterNS, Verb: "list", Resource: "secrets"},
		},
	}
	lsar.Namespace = inClusterNS
	if _, err := tr.Forward(lsar, "111111"); err != nil {
		t.Fatal(err)
	}
	t.Logf("LocalSAR: metadata.ns stays %q (default convertor uses UpstreamNamespace), spec ns -> %q",
		util.UpstreamNamespace("111111", lsar.Namespace), lsar.Spec.ResourceAttributes.Namespace)
}
