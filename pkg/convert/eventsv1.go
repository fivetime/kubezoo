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
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/runtime"
	internal "k8s.io/kubernetes/pkg/apis/core"
)

// EventsV1Transformer rewrites the object references of an events.k8s.io Event.
//
// ⚠️ There was no convertor for this group at all, so its references went through
// untouched while the event's own namespace was prefixed. The result was an
// Event in <tid>-default whose regarding.namespace said "default" -- the
// platform's namespace, not the tenant's. Two consequences, both silent:
// upstream accepts it (validation stopped requiring the two to agree once
// eventTime is set), and because kube-apiserver serves core Events and
// events.k8s.io Events over the same stored objects, reading the tenant's
// core/v1 events then failed for the whole namespace -- the core Event
// transformer refuses a reference whose namespace carries no tenant prefix, and
// a list returns on the first item that fails.
//
// This is the same rewriting the core Event transformer does; the group is
// served with the versioned type rather than the internal one, so the references
// are corev1.ObjectReference and have to be lifted across.
type EventsV1Transformer struct {
	objectRefTransformer ObjectReferenceTransformer
}

var _ ObjectTransformer = &EventsV1Transformer{}

// NewEventsV1Transformer initiates a transformer for events.k8s.io Events.
func NewEventsV1Transformer(ort ObjectReferenceTransformer) ObjectTransformer {
	return &EventsV1Transformer{objectRefTransformer: ort}
}

// Forward transforms tenant object references to upstream object references.
func (t *EventsV1Transformer) Forward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	return t.rewrite(obj, tenantID, t.objectRefTransformer.Forward)
}

// Backward transforms upstream object references to tenant object references.
//
// ⚠️ Tolerant on the way back, deliberately. The reference transformer errors on
// a namespace that carries no tenant prefix, and a list returns on the first item
// that fails -- so one such event would make `kubectl get events` fail wholesale
// for the namespace and terminate every watch on it, with the object impossible
// to delete because it could not be listed. That is the same "one object poisons
// the whole list" shape already fixed for RoleBinding subjects and roleRefs. An
// event written before this convertor existed, or by anything that reached
// upstream another way, is exactly such an object. Reading it back as it stands
// shows the tenant what is really there; refusing hides everything else too.
func (t *EventsV1Transformer) Backward(obj runtime.Object, tenantID string) (runtime.Object, error) {
	out, err := t.rewrite(obj, tenantID, t.objectRefTransformer.Backward)
	if err != nil {
		return obj, nil
	}
	return out, nil
}

type refRewriter func(*internal.ObjectReference, string) (*internal.ObjectReference, error)

func (t *EventsV1Transformer) rewrite(obj runtime.Object, tenantID string, rewrite refRewriter) (runtime.Object, error) {
	event, ok := obj.(*eventsv1.Event)
	if !ok {
		return nil, errors.Errorf("fail to assert the runtime object to an events.k8s.io event")
	}
	regarding, err := rewriteVersionedRef(&event.Regarding, tenantID, rewrite)
	if err != nil {
		return nil, err
	}
	event.Regarding = *regarding
	if event.Related != nil {
		related, err := rewriteVersionedRef(event.Related, tenantID, rewrite)
		if err != nil {
			return nil, err
		}
		event.Related = related
	}
	return event, nil
}

// rewriteVersionedRef lifts a versioned reference into the internal shape the
// reference transformer works on, and puts the answer back. The field set is
// small and has been stable since v1; copying it explicitly keeps this
// independent of whether the events conversions happen to be installed in the
// scheme this binary builds.
func rewriteVersionedRef(ref *corev1.ObjectReference, tenantID string,
	rewrite refRewriter) (*corev1.ObjectReference, error) {

	lifted := &internal.ObjectReference{
		Kind:            ref.Kind,
		Namespace:       ref.Namespace,
		Name:            ref.Name,
		UID:             ref.UID,
		APIVersion:      ref.APIVersion,
		ResourceVersion: ref.ResourceVersion,
		FieldPath:       ref.FieldPath,
	}
	rewritten, err := rewrite(lifted, tenantID)
	if err != nil {
		return nil, err
	}
	if rewritten == nil {
		return ref, nil
	}
	return &corev1.ObjectReference{
		Kind:            rewritten.Kind,
		Namespace:       rewritten.Namespace,
		Name:            rewritten.Name,
		UID:             rewritten.UID,
		APIVersion:      rewritten.APIVersion,
		ResourceVersion: rewritten.ResourceVersion,
		FieldPath:       rewritten.FieldPath,
	}, nil
}
