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

package proxy

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	core "k8s.io/kubernetes/pkg/apis/core"

	"github.com/fivetime/kubezoo-gateway/pkg/convert"
)

const probeHostDetail = "a probe or lifecycle handler is dialled BY THE KUBELET, from the node's " +
	"network and not from your pod's, so an address here reaches whatever the platform's nodes can " +
	"reach. Leave it unset and the kubelet uses the pod's own address, which is what a health check " +
	"is for"

// refuseProbeHost stops a tenant pointing a probe at an address of its choosing.
//
// ⛔ MEASURED. probe/http/request.go: `host := httpGet.Host`, with the upstream
// comment "When httpGet.Host is empty, podIP will be used instead" -- so a
// non-empty host is dialled verbatim, and the dial happens IN THE KUBELET, from
// the node. The tenant's pods live on a per-tenant OVN/Neutron network; the node
// does not. Nothing else looks at this field: kubezoo's convertors and guards
// have no mention of probes, and neither does a single policy in
// kubezoo-contract/config/policy.
//
// It is two of the three categories at once:
//   - REACH: the connection originates on the platform's node network, which the
//     tenant cannot reach itself. No tenant NetworkPolicy and no OVN ACL applies
//     to a kubelet-originated probe.
//   - VOLUME: periodSeconds is the tenant's, and so is the number of pods and
//     containers. A one-second probe across a deployment is a scheduled scanner
//     the platform runs on the tenant's behalf.
//
// ⚠️ And it answers back. A probe that fails shows in the pod's Ready condition
// and in Events, both of which the tenant reads -- so open-versus-closed is
// legible even without a response body. One bit per address per period is a port
// scanner of the platform network.
//
// ⭐ Refusing a non-empty host costs almost nothing real: a health check is
// meant to test THIS container, and leaving the field unset is how you say that.
// A tenant wanting to reach something else can dial it from inside its own pod,
// where its own network rules apply -- which is the whole distinction.
//
// ⚠️ Covers initContainers as well as containers. A restartable init container
// carries the same probes, and a rule applied to one list and not the other is
// this repository's most repeated bug.
func (tp *tenantProxy) refuseProbeHost(obj runtime.Object) error {
	spec, err := convert.PodSpecOf(obj)
	if err != nil || spec == nil {
		return nil
	}
	lists := []struct {
		name       string
		containers []core.Container
	}{
		{"containers", spec.Containers},
		{"initContainers", spec.InitContainers},
	}
	for _, l := range lists {
		for i := range l.containers {
			c := &l.containers[i]
			base := field.NewPath("spec", l.name).Index(i)
			for _, p := range []struct {
				name  string
				probe *core.Probe
			}{
				{"livenessProbe", c.LivenessProbe},
				{"readinessProbe", c.ReadinessProbe},
				{"startupProbe", c.StartupProbe},
			} {
				if err := refuseHandlerHost(obj, base.Child(p.name), probeHandler(p.probe)); err != nil {
					return err
				}
			}
			if c.Lifecycle != nil {
				if err := refuseHandlerHost(obj, base.Child("lifecycle", "postStart"),
					lifecycleHandler(c.Lifecycle.PostStart)); err != nil {
					return err
				}
				if err := refuseHandlerHost(obj, base.Child("lifecycle", "preStop"),
					lifecycleHandler(c.Lifecycle.PreStop)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type handlerHosts struct {
	httpGet   string
	tcpSocket string
}

func probeHandler(p *core.Probe) handlerHosts {
	if p == nil {
		return handlerHosts{}
	}
	return handlerHosts{httpGet: httpHost(p.HTTPGet), tcpSocket: tcpHost(p.TCPSocket)}
}

func lifecycleHandler(h *core.LifecycleHandler) handlerHosts {
	if h == nil {
		return handlerHosts{}
	}
	return handlerHosts{httpGet: httpHost(h.HTTPGet), tcpSocket: tcpHost(h.TCPSocket)}
}

func httpHost(a *core.HTTPGetAction) string {
	if a == nil {
		return ""
	}
	return a.Host
}

func tcpHost(a *core.TCPSocketAction) string {
	if a == nil {
		return ""
	}
	return a.Host
}

func refuseHandlerHost(obj runtime.Object, base *field.Path, h handlerHosts) error {
	for _, k := range []struct {
		child string
		host  string
	}{{"httpGet", h.httpGet}, {"tcpSocket", h.tcpSocket}} {
		if k.host == "" {
			continue
		}
		name := ""
		if a, ok := obj.(interface{ GetName() string }); ok {
			name = a.GetName()
		}
		return apierrors.NewInvalid(
			schema.GroupKind{Kind: obj.GetObjectKind().GroupVersionKind().Kind}, name,
			field.ErrorList{field.Forbidden(base.Child(k.child, "host"), probeHostDetail)})
	}
	return nil
}
