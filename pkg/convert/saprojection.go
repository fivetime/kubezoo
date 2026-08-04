/*
Copyright 2024 The KubeZoo Authors.

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

package convert

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	core "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/utils/ptr"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

const (
	// TenantNamespaceAnnotation carries the tenant's own name for the namespace
	// a pod is in. It exists for one reader: the downward API projection below,
	// which has no way to write a literal.
	TenantNamespaceAnnotation = "kubezoo.io/tenant-namespace"

	// upstreamSAVolumePrefix is what upstream's ServiceAccount admission plugin
	// looks for before deciding to inject a volume of its own. Any volume whose
	// name starts with this makes it leave the volume alone and only wire the
	// mounts (ServiceAccountVolumeName, plugin/pkg/admission/serviceaccount).
	//
	// ⚠️ Load-bearing string. If upstream ever renames it, kubezoo's volume
	// stops suppressing upstream's, upstream adds a second one, and the FIRST
	// volume with the prefix wins the mount -- which would be upstream's, with
	// metadata.namespace back. TestVolumeNameSuppressesUpstreamInjection pins it
	// against the vendored constant.
	upstreamSAVolumePrefix = "kube-api-access"

	// kubezooSAVolumeName is the name kubezoo injects under. Suffixed, because
	// the prefix alone is not a legal match for upstream's HasPrefix test.
	kubezooSAVolumeName = upstreamSAVolumePrefix + "-kubezoo"

	// rootCAConfigMap and the paths below mirror upstream's TokenVolumeSource.
	// Only the third source differs.
	rootCAConfigMap = "kube-root-ca.crt"

	// saTokenExpirationSeconds matches upstream's WarnOnlyBoundTokenExpiration.
	saTokenExpirationSeconds = 3607
)

// SATokenNamespaceTransformer makes a pod's own idea of its namespace agree with
// the one its API server answers in.
//
// A pod learns its namespace from /var/run/secrets/kubernetes.io/serviceaccount
// /namespace, which upstream's ServiceAccount admission plugin fills from a
// downward API selector on metadata.namespace. That is the UPSTREAM name,
// <tid>-default, because the pod really does live there.
//
// util.UpstreamNamespace already makes requests carrying that name work -- it
// reads a name that already has the tenant's prefix as one already resolved. So
// the request succeeds. What it cannot fix is the spelling of the ANSWER: the
// objects come back saying "default", and a client that indexes them by their
// own namespace and then looks them up by the name it read from that file finds
// nothing. controller-runtime does exactly this, so an operator that watches its
// own namespace gets a cache that never matches: no error, no log, no events.
//
// ⭐ Why not the policy layer, which is where this was going to be done: a
// Kyverno mutation that does not run is silent (failurePolicy Ignore is the
// documented hazard, and Fail trades it for an outage), and this is the same
// tenant-to-upstream name translation kubezoo already owns everywhere else. It
// belongs in the layer that cannot be absent.
//
// ⭐ Why an annotation rather than a per-namespace ConfigMap: the downward API
// accepts metadata.annotations['key'] (validation.go's
// validVolumeDownwardAPIFieldPathExpressions), so no second object has to be
// created, kept in sync with the namespace, or garbage-collected. The tenant can
// edit the annotation on its own pods, and the worst it buys is a pod that
// misaddresses its own namespace: UpstreamNamespace re-prefixes with the
// REQUESTING tenant's id, so a forged value still resolves inside the tenant.
type SATokenNamespaceTransformer struct{}

var _ ObjectTransformer = &SATokenNamespaceTransformer{}

// NewSATokenNamespaceTransformer initiates a SATokenNamespaceTransformer.
func NewSATokenNamespaceTransformer() ObjectTransformer { return &SATokenNamespaceTransformer{} }

// Forward projects the tenant's namespace name into every pod the template will
// produce.
func (t *SATokenNamespaceTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	meta, spec, err := podTemplateOf(obj)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return obj, nil
	}
	if _, isPod := obj.(*core.Pod); isPod {
		// ⛔ Same reason placement skips a live Pod, and a sharper one: a pod's
		// spec.volumes is IMMUTABLE after creation. Injecting here would make
		// upstream refuse every update to a pod that predates this, and the
		// tenant could no longer touch its own running pod at all. Pods are done
		// on CREATE only, by tenantProxy.Create, through ProjectPodNamespace.
		return obj, nil
	}

	// The namespace on obj is already the upstream one: the default convertor
	// runs before every transformer.
	if accessor, ok := obj.(metav1.Object); ok {
		project(meta, spec, util.TrimTenantIDPrefix(tenantID, accessor.GetNamespace()))
	}
	return obj, nil
}

// Backward takes the annotation back out.
//
// The tenant never wrote it and nothing it runs reads it -- the pod's own
// kubelet does, from the stored object, which keeps it. Leaving it in the answer
// would put a platform-internal field into what a tenant reads back and then
// re-applies, and the injected volume is enough of a visible difference already.
//
// ⚠️ The VOLUME deliberately stays. Real Kubernetes puts a kube-api-access
// volume in every pod, so hiding it would make a pod read back as if it had no
// service account at all, and a tenant debugging a token problem would be
// looking at a spec that is not the one running.
func (t *SATokenNamespaceTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	meta, spec, err := podTemplateOf(obj)
	if err != nil {
		return nil, err
	}
	if spec == nil || meta == nil {
		return obj, nil
	}
	delete(meta.Annotations, TenantNamespaceAnnotation)
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}
	return obj, nil
}

// ProjectPodNamespace is Forward for a live Pod.
//
// ⚠️ Exported for tenantProxy.Create, the one caller that knows a write is a
// create. See the comment in Forward for why an update must not do this.
func ProjectPodNamespace(pod *core.Pod, tenantID string) {
	project(&pod.ObjectMeta, &pod.Spec, util.TrimTenantIDPrefix(tenantID, pod.Namespace))
}

// project stamps the annotation and makes sure a service account volume reads it.
func project(meta *metav1.ObjectMeta, spec *core.PodSpec, tenantNamespace string) {
	if tenantNamespace == "" {
		// A pod with no namespace is not something this can answer for; leaving
		// the annotation off means the projection below is not installed either,
		// and the pod keeps upstream's behaviour rather than getting an empty
		// namespace file.
		return
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	// ⭐ Unconditional. A tenant-supplied value here is an input to a platform
	// decision, and the rule everywhere else in kubezoo is that those are
	// overwritten, not honoured.
	meta.Annotations[TenantNamespaceAnnotation] = tenantNamespace

	if ptr.Deref(spec.AutomountServiceAccountToken, true) == false {
		// The tenant asked for no token. Injecting a volume no container mounts
		// would still make kubelet mint one.
		//
		// ⚠️ Only the POD-level setting is visible from here. A ServiceAccount
		// with automountServiceAccountToken: false and a pod that says nothing
		// gets the volume injected and never mounted -- harmless, since an
		// unmounted volume is unreachable, but it is why this is not a complete
		// mirror of upstream's shouldAutomount.
		return
	}

	if existing := saVolume(spec); existing != nil {
		// Either kubezoo's own volume coming back through a read-modify-write,
		// or one the tenant wrote by hand. Rewriting the one that is there is
		// what keeps upstream's "first volume with the prefix wins" from
		// deciding between two.
		retargetNamespaceSource(existing)
		return
	}
	spec.Volumes = append(spec.Volumes, core.Volume{
		Name:         kubezooSAVolumeName,
		VolumeSource: core.VolumeSource{Projected: tenantNamespaceTokenVolume()},
	})
}

// saVolume returns the volume upstream's plugin would consider already present.
//
// ⚠️ Must match upstream's search exactly: first by slice order, prefix match.
// Picking a different one than upstream mounts would leave kubezoo rewriting a
// volume nobody reads.
func saVolume(spec *core.PodSpec) *core.Volume {
	for i := range spec.Volumes {
		if strings.HasPrefix(spec.Volumes[i].Name, upstreamSAVolumePrefix+"-") {
			return &spec.Volumes[i]
		}
	}
	return nil
}

// retargetNamespaceSource points an existing volume's namespace file at the
// annotation, leaving everything else about it alone.
func retargetNamespaceSource(v *core.Volume) {
	if v.Projected == nil {
		// A tenant can name a volume kube-api-access-foo and make it an emptyDir.
		// Upstream will mount that at the service account path and the pod gets
		// no token at all -- their own doing, and not something to repair here.
		return
	}
	for i := range v.Projected.Sources {
		down := v.Projected.Sources[i].DownwardAPI
		if down == nil {
			continue
		}
		for j := range down.Items {
			if down.Items[j].Path == "namespace" {
				down.Items[j].FieldRef = tenantNamespaceFieldRef()
			}
		}
	}
}

func tenantNamespaceFieldRef() *core.ObjectFieldSelector {
	return &core.ObjectFieldSelector{
		APIVersion: "v1",
		FieldPath:  "metadata.annotations['" + TenantNamespaceAnnotation + "']",
	}
}

// tenantNamespaceTokenVolume mirrors upstream's TokenVolumeSource with one
// source changed. The other two are copied rather than referenced because the
// upstream constructor builds them from the internal API types this package
// cannot reach without importing the admission plugin.
func tenantNamespaceTokenVolume() *core.ProjectedVolumeSource {
	return &core.ProjectedVolumeSource{
		DefaultMode: ptr.To[int32](corev1.ProjectedVolumeSourceDefaultMode),
		Sources: []core.VolumeProjection{
			{
				ServiceAccountToken: &core.ServiceAccountTokenProjection{
					Path:              "token",
					ExpirationSeconds: saTokenExpirationSeconds,
				},
			},
			{
				ConfigMap: &core.ConfigMapProjection{
					LocalObjectReference: core.LocalObjectReference{Name: rootCAConfigMap},
					Items:                []core.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
			{
				DownwardAPI: &core.DownwardAPIProjection{
					Items: []core.DownwardAPIVolumeFile{
						{Path: "namespace", FieldRef: tenantNamespaceFieldRef()},
					},
				},
			},
		},
	}
}
