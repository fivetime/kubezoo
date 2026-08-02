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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apps "k8s.io/kubernetes/pkg/apis/apps"
	autoscaling "k8s.io/kubernetes/pkg/apis/autoscaling"
	batch "k8s.io/kubernetes/pkg/apis/batch"
	core "k8s.io/kubernetes/pkg/apis/core"
	policy "k8s.io/kubernetes/pkg/apis/policy"

	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// tenantChosenPlacement is what a tenant would write if it were trying to reach
// another tenant's nodes -- every field the transformer has to deal with, all
// pointing somewhere it may not go.
func tenantChosenPlacement() core.PodSpec {
	return core.PodSpec{
		NodeSelector: map[string]string{
			util.NodePoolLabelKey: "222222",
			// ⚠️ The second key is what makes the wholesale replacement
			// load-bearing rather than decorative. Overwriting only the pool key
			// leaves this one in place, and a nodeSelector is ANDed: the pod then
			// needs a node that is in this tenant's pool AND carries a label only
			// the platform's own nodes have. A fixture with one key cannot tell
			// merging apart from replacing -- this one has been checked to fail
			// when the code merges.
			"node-role.kubernetes.io/control-plane": "",
		},
		SchedulerName: "someone-elses-scheduler",
		Tolerations: []core.Toleration{{
			Key: util.NodePoolLabelKey, Operator: core.TolerationOpEqual,
			Value: "222222", Effect: core.TaintEffectNoSchedule,
		}},
		Affinity: &core.Affinity{NodeAffinity: &core.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
				NodeSelectorTerms: []core.NodeSelectorTerm{{
					MatchExpressions: []core.NodeSelectorRequirement{{
						Key: util.NodePoolLabelKey, Operator: core.NodeSelectorOpIn,
						Values: []string{"222222"},
					}},
				}},
			},
		}},
		TopologySpreadConstraints: []core.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: util.NodePoolLabelKey,
		}},
	}
}

func assertPlaced(t *testing.T, what string, spec *core.PodSpec, tenantID string) {
	t.Helper()
	if got := spec.NodeSelector[util.NodePoolLabelKey]; got != tenantID {
		t.Errorf("%s: nodeSelector pool is %q, want %q", what, got, tenantID)
	}
	if len(spec.NodeSelector) != 1 {
		t.Errorf("%s: nodeSelector is %v; it must be replaced wholesale, or an extra "+
			"key narrows it onto a node that only exists in another pool",
			what, spec.NodeSelector)
	}
	if spec.SchedulerName != util.TenantSchedulerName {
		t.Errorf("%s: schedulerName is %q, want %q", what, spec.SchedulerName, util.TenantSchedulerName)
	}
	// ⚠️ Both can express "run me over there" in ways one nodeSelector cannot
	// override: a nodeAffinity term is ORed with others, and a topology
	// constraint can push a pod off the pool it was selected onto. There is no
	// safe value to substitute, so they go.
	if spec.Affinity != nil {
		t.Errorf("%s: affinity survived", what)
	}
	if spec.TopologySpreadConstraints != nil {
		t.Errorf("%s: topologySpreadConstraints survived", what)
	}
	var pool, notReady, unreachable bool
	for _, toleration := range spec.Tolerations {
		switch toleration.Key {
		case util.NodePoolLabelKey:
			if toleration.Value != tenantID {
				t.Errorf("%s: tolerating pool %q, want %q", what, toleration.Value, tenantID)
			}
			pool = true
		case "node.kubernetes.io/not-ready":
			notReady = true
		case "node.kubernetes.io/unreachable":
			unreachable = true
		}
	}
	if !pool {
		t.Errorf("%s: no toleration for its own pool -- with the pool tainted, the pod "+
			"cannot land anywhere at all", what)
	}
	// ⚠️ The toleration list is replaced wholesale, so these have to be written
	// back. Kubernetes' own defaulting adds them; dropping them would evict every
	// tenant pod the instant a node blipped.
	if !notReady || !unreachable {
		t.Errorf("%s: lost the not-ready/unreachable tolerations (%v/%v); every pod would "+
			"be evicted the moment a node blipped", what, notReady, unreachable)
	}
}

// TestEveryPodCarrierIsPlaced walks every kind the transformer claims to handle
// and checks the pod template inside it actually got rewritten.
//
// ⚠️ A kind that is served but not placed keeps whatever the tenant asked for,
// silently: no error, no log, nothing that fails to build. There is no way to
// notice except by looking.
func TestEveryPodCarrierIsPlaced(t *testing.T) {
	const tenantID = "111111"
	template := func() core.PodTemplateSpec {
		return core.PodTemplateSpec{Spec: tenantChosenPlacement()}
	}
	meta := metav1.ObjectMeta{Name: "workload", Namespace: "team"}

	for _, tc := range []struct {
		kind string
		obj  runtime.Object
		spec func(runtime.Object) *core.PodSpec
	}{
		{"Pod", &core.Pod{ObjectMeta: meta, Spec: tenantChosenPlacement()},
			func(o runtime.Object) *core.PodSpec { return &o.(*core.Pod).Spec }},
		{"PodTemplate", &core.PodTemplate{ObjectMeta: meta, Template: template()},
			func(o runtime.Object) *core.PodSpec { return &o.(*core.PodTemplate).Template.Spec }},
		{"ReplicationController", func() runtime.Object {
			tmpl := template()
			return &core.ReplicationController{ObjectMeta: meta,
				Spec: core.ReplicationControllerSpec{Template: &tmpl}}
		}(), func(o runtime.Object) *core.PodSpec {
			return &o.(*core.ReplicationController).Spec.Template.Spec
		}},
		{"Deployment", &apps.Deployment{ObjectMeta: meta,
			Spec: apps.DeploymentSpec{Template: template()}},
			func(o runtime.Object) *core.PodSpec { return &o.(*apps.Deployment).Spec.Template.Spec }},
		{"StatefulSet", &apps.StatefulSet{ObjectMeta: meta,
			Spec: apps.StatefulSetSpec{Template: template()}},
			func(o runtime.Object) *core.PodSpec { return &o.(*apps.StatefulSet).Spec.Template.Spec }},
		{"DaemonSet", &apps.DaemonSet{ObjectMeta: meta,
			Spec: apps.DaemonSetSpec{Template: template()}},
			func(o runtime.Object) *core.PodSpec { return &o.(*apps.DaemonSet).Spec.Template.Spec }},
		{"ReplicaSet", &apps.ReplicaSet{ObjectMeta: meta,
			Spec: apps.ReplicaSetSpec{Template: template()}},
			func(o runtime.Object) *core.PodSpec { return &o.(*apps.ReplicaSet).Spec.Template.Spec }},
		{"Job", &batch.Job{ObjectMeta: meta, Spec: batch.JobSpec{Template: template()}},
			func(o runtime.Object) *core.PodSpec { return &o.(*batch.Job).Spec.Template.Spec }},
		{"CronJob", &batch.CronJob{ObjectMeta: meta, Spec: batch.CronJobSpec{
			JobTemplate: batch.JobTemplateSpec{Spec: batch.JobSpec{Template: template()}}}},
			func(o runtime.Object) *core.PodSpec {
				return &o.(*batch.CronJob).Spec.JobTemplate.Spec.Template.Spec
			}},
	} {
		if _, err := NewPlacementTransformer().Forward(tc.obj, tenantID); err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		assertPlaced(t, tc.kind, tc.spec(tc.obj), tenantID)
	}
}

// TestPlacementCoversWhatIsRegistered pins the list against itself: every kind
// in PodCarryingKinds must be one podSpecOf can actually reach.
func TestPlacementCoversWhatIsRegistered(t *testing.T) {
	byKind := map[string]runtime.Object{
		"Pod": &core.Pod{}, "PodTemplate": &core.PodTemplate{},
		"ReplicationController": &core.ReplicationController{},
		"Deployment":            &apps.Deployment{}, "StatefulSet": &apps.StatefulSet{},
		"DaemonSet": &apps.DaemonSet{}, "ReplicaSet": &apps.ReplicaSet{},
		"Job": &batch.Job{}, "CronJob": &batch.CronJob{},
	}
	for _, gk := range PodCarryingKinds {
		obj, known := byKind[gk.Kind]
		if !known {
			t.Errorf("%s is registered for placement but this test has no object for it, "+
				"so nothing checks that podSpecOf can reach its template", gk)
			continue
		}
		if _, err := podSpecOf(obj); err != nil {
			t.Errorf("%s is registered for placement but podSpecOf refuses it: %v", gk, err)
		}
	}
	if len(PodCarryingKinds) != len(byKind) {
		t.Errorf("PodCarryingKinds has %d entries and this test knows %d; a kind added to "+
			"one and not the other is a kind nobody places", len(PodCarryingKinds), len(byKind))
	}
}

// TestPlacementRefusesWhatItCannotPlace -- the transformer is registered per
// kind, so being handed something else means the registration and the type
// switch disagree. Silently returning the object unchanged would be a kind whose
// placement is whatever the tenant asked for.
func TestPlacementRefusesWhatItCannotPlace(t *testing.T) {
	if _, err := NewPlacementTransformer().Forward(&core.Service{}, "111111"); err == nil {
		t.Error("a Service was accepted; a kind registered by mistake would silently keep " +
			"the tenant's own placement")
	}
}

// TestPlacementLetsSubresourcesThrough is the case a unit test could not have
// predicted and the lab caught.
//
// ⚠️ The convertor map is keyed by GroupKind, and a subresource storage carries
// its PARENT's kind -- so pods/binding arrives here as a Binding under Kind
// "Pod". Erroring on it did refuse the binding, but by THIS instead of by the
// ValidatingAdmissionPolicy that exists to refuse it. The lab assertion still saw
// a refusal; only the reason gave it away. A guard that masks the guard behind it
// is worse than no guard.
func TestPlacementLetsSubresourcesThrough(t *testing.T) {
	for _, obj := range []runtime.Object{
		&core.Binding{},      // pods/binding
		&policy.Eviction{},   // pods/eviction
		&autoscaling.Scale{}, // deployments|statefulsets|replicasets|rc /scale
	} {
		spec, err := podSpecOf(obj)
		if err != nil {
			t.Errorf("%T is a subresource payload of a placed kind and must pass "+
				"through, not be refused: %v", obj, err)
		}
		if spec != nil {
			t.Errorf("%T reported a pod spec it does not have", obj)
		}
	}
}
