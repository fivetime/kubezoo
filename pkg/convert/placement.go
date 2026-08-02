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

package convert

import (
	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apps "k8s.io/kubernetes/pkg/apis/apps"
	autoscaling "k8s.io/kubernetes/pkg/apis/autoscaling"
	batch "k8s.io/kubernetes/pkg/apis/batch"
	core "k8s.io/kubernetes/pkg/apis/core"
	policy "k8s.io/kubernetes/pkg/apis/policy"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// PlacementTransformer decides where a tenant's workloads may run, by rewriting
// the placement fields of every pod template that passes through kubezoo.
//
// ⭐ A second copy of what the Kyverno tenant-placement policy already does, and
// the duplication is the entire point. Placement isolation is the load-bearing
// part of tenancy here -- the injected nodeSelector is what keeps one tenant's
// pods off another's nodes -- and until now it existed in exactly one place: a
// webhook. docs/operations-cn.md records the day Kyverno's own webhook never
// registered, three policies never became ready, pods went through unguarded,
// and the only symptom on screen was READY=<none>. A webhook that was never
// registered does not fail, so failurePolicy: Fail does not cover it.
//
// ⭐ Rewriting the TEMPLATE is what makes this work at all, and it is worth
// being precise about why. kubezoo never sees most tenant pods: a Deployment's
// pods are created by kube-controller-manager against the upstream apiserver and
// pass through nothing of ours. What kubezoo does see is the template the tenant
// wrote. So a template stored with the right pool keeps producing correctly
// placed pods for as long as the outage lasts, which is the only protection
// available for pods kubezoo cannot intercept.
//
// ⚠️ Applied on UPDATE as well as CREATE, unlike the policy, which matches
// operations: [CREATE] only. The policy gets away with that because a pod is
// always a CREATE and its own rule overwrites whatever the template said. Here
// there is no such backstop -- the whole point is behaving correctly when the
// pod-level rule is not running -- so a template edited after the fact has to be
// rewritten too. It also keeps the stored object honest rather than storing a
// nodeSelector that only something else's mutation makes untrue.
//
// The values come from kubezoo-contract, checked against the policy YAML by
// TestPlacementMatchesThePolicy, because a disagreement here is invisible: the
// policy runs after kubezoo and wins whenever it is there, so kubezoo's copy
// only starts deciding placement on the day the webhook is gone.
type PlacementTransformer struct{}

var _ ObjectTransformer = &PlacementTransformer{}

// NewPlacementTransformer initiates a PlacementTransformer.
func NewPlacementTransformer() ObjectTransformer { return &PlacementTransformer{} }

// Forward pins the workload to the tenant's own node pool.
func (t *PlacementTransformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	spec, err := podSpecOf(obj)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		// A subresource -- a scale, a status -- carries no pod template. Nothing
		// to place, and nothing wrong with that.
		return obj, nil
	}
	if _, isPod := obj.(*core.Pod); isPod {
		// ⛔ A live Pod is placed on CREATE and never again -- by
		// tenantProxy.Create, which is the one caller that knows it is a create.
		//
		// Rewriting these on an update does not merely waste work, it BREAKS the
		// pod. spec.nodeSelector and spec.schedulerName are immutable on a pod
		// update, and validateOnlyAddedTolerations requires every toleration the
		// stored pod already has to still be there. So writing the canonical
		// placement over a pod whose stored placement differs -- one created
		// before this existed, above all -- makes upstream refuse the update, and
		// the tenant can no longer touch its own running pod at all. Kyverno never
		// had this problem because its rules match operations: [CREATE].
		//
		// The lab's "a bound pod stays writable" assertion did not catch it: the
		// pod it writes to was itself created with the canonical placement, so the
		// rewrite was a no-op. Only a pod predating the change would have failed.
		return obj, nil
	}
	place(spec, tenantID)
	// ⭐ nodeName in a TEMPLATE is always the tenant's own doing: nothing in
	// Kubernetes ever writes it there. It goes around the scheduler entirely
	// -- kubelet takes the pod because the pod names the node, and a
	// nodeSelector it fails is caught only later, by kubelet, after the pod
	// is already assigned. Cleared here rather than refused so that a
	// Deployment already carrying one keeps reconciling instead of failing
	// every write from now on.
	//
	// ⚠️ NOT done for a live Pod, and that is the trap this side-steps: the
	// scheduler writes spec.nodeName on every pod it binds, so from then on
	// every update to that pod -- a label, an annotation -- carries it.
	// Clearing it there would unbind running pods, and refusing would fail
	// every later write. tenantProxy.Create refuses it on the one operation
	// where it can only have come from the tenant.
	spec.NodeName = ""
	return obj, nil
}

// Backward leaves the object alone: a tenant sees where its pods actually run,
// which is what it has always seen, since the policy's mutation was never hidden
// either. Hiding it would leave a tenant unable to explain its own scheduling.
func (t *PlacementTransformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	return obj, nil
}

// place rewrites the placement fields of one pod spec.
func place(spec *core.PodSpec, tenantID string) {
	pool := util.NodePoolFor(tenantID)

	// Replaced wholesale, not merged. A tenant adding one more key would
	// otherwise narrow the selector to a node that only exists in another pool.
	spec.NodeSelector = map[string]string{util.NodePoolLabelKey: pool}

	// ⚠️ Also replaced wholesale, which is why the not-ready and unreachable
	// tolerations have to be written back: Kubernetes' own defaulting adds them,
	// and dropping them would evict every tenant pod the instant a node blipped.
	seconds := int64(util.UnreachableTolerationSeconds)
	spec.Tolerations = []core.Toleration{
		{
			Key:      util.NodePoolLabelKey,
			Operator: core.TolerationOpEqual,
			Value:    pool,
			Effect:   core.TaintEffectNoSchedule,
		},
		{
			Key:               corev1.TaintNodeNotReady,
			Operator:          core.TolerationOpExists,
			Effect:            core.TaintEffectNoExecute,
			TolerationSeconds: &seconds,
		},
		{
			Key:               corev1.TaintNodeUnreachable,
			Operator:          core.TolerationOpExists,
			Effect:            core.TaintEffectNoExecute,
			TolerationSeconds: &seconds,
		},
	}

	spec.SchedulerName = util.TenantSchedulerName

	// ⚠️ Dropped rather than rewritten. Both can express "run me on that node"
	// in ways a single nodeSelector cannot override -- a nodeAffinity term is
	// ORed with others, and a topology constraint can push a pod off the pool it
	// was selected onto. There is no safe value to substitute, so there is none.
	spec.Affinity = nil
	spec.TopologySpreadConstraints = nil
}

// podSpecOf finds the pod template inside whatever the tenant wrote.
//
// ⚠️ Every kind that carries one has to be here. A kind that is served but
// missing from this switch silently keeps whatever placement the tenant asked
// for -- there is no error, no log, and nothing that fails to build.
// TestEveryServedPodCarrierIsPlaced is what makes that impossible to miss.
func podSpecOf(obj runtime.Object) (*core.PodSpec, error) {
	switch typed := obj.(type) {
	case *core.Pod:
		return &typed.Spec, nil
	case *core.PodTemplate:
		return &typed.Template.Spec, nil
	case *core.ReplicationController:
		if typed.Spec.Template == nil {
			return nil, nil
		}
		return &typed.Spec.Template.Spec, nil
	case *apps.Deployment:
		return &typed.Spec.Template.Spec, nil
	case *apps.StatefulSet:
		return &typed.Spec.Template.Spec, nil
	case *apps.DaemonSet:
		return &typed.Spec.Template.Spec, nil
	case *apps.ReplicaSet:
		return &typed.Spec.Template.Spec, nil
	case *batch.Job:
		return &typed.Spec.Template.Spec, nil
	case *batch.CronJob:
		return &typed.Spec.JobTemplate.Spec.Template.Spec, nil

	// ⚠️ Subresources of the kinds above, which reach this convertor because the
	// map is keyed by GroupKind and a subresource storage carries its PARENT's
	// kind. They are legitimately here and carry no template.
	//
	// Found by the lab, not by a unit test: pods/binding arrives as a Binding
	// under Kind "Pod", hit the error below, and the binding was refused -- by
	// this, instead of by the ValidatingAdmissionPolicy that exists to refuse it.
	// The assertion still saw a refusal and only the reason gave it away. A guard
	// that masks the guard behind it is worse than no guard.
	case *core.Binding, *policy.Eviction, *autoscaling.Scale:
		return nil, nil
	}
	return nil, errors.Errorf("placement was asked to handle %T, which carries no pod template; "+
		"either it should not be registered in InitConvertors or podSpecOf is missing a case", obj)
}

// PodCarryingKinds are every kind kubezoo serves that carries a pod template.
//
// ⚠️ Exported so a test can check it against what the server actually installs.
// A kind that is served but missing here keeps whatever placement the tenant
// asked for, silently: no error, no log, nothing that fails to build.
var PodCarryingKinds = []schema.GroupKind{
	{Group: "", Kind: "Pod"},
	{Group: "", Kind: "PodTemplate"},
	{Group: "", Kind: "ReplicationController"},
	{Group: "apps", Kind: "Deployment"},
	{Group: "apps", Kind: "StatefulSet"},
	{Group: "apps", Kind: "DaemonSet"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
	{Group: "batch", Kind: "CronJob"},
}

// PlacePod puts a pod in its tenant's node pool.
//
// ⚠️ Exported for tenantProxy.Create, which is the one caller that knows a write
// is a CREATE. A pod's placement cannot be rewritten afterwards -- nodeSelector
// and schedulerName are immutable on a pod update, and every toleration the
// stored pod already carries has to still be there -- so doing this on an update
// would make upstream refuse it and leave the tenant unable to touch its own
// running pod. Templates go through Forward instead, where there is no such
// restriction and no need for a verb.
func PlacePod(pod *core.Pod, tenantID string) { place(&pod.Spec, tenantID) }
