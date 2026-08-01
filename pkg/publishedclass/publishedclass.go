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

// Package publishedclass answers which of the platform's own cluster-scoped
// class objects are offered to tenants.
package publishedclass

import (
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

// Set reports what the platform publishes, and in which state.
//
// ⚠️ Read from a label on the object, not from a flag read at startup. A flag
// meant that publishing a new storage class required restarting the gateway --
// which, on the shipped single-replica StatefulSet, interrupts EVERY tenant's
// API access and breaks every tenant operator's watch, to change one line of
// configuration. It also meant a typo in the flag was completely silent: the
// name simply never appeared, with no error and no log.
//
// The label lives on the object, so the truth cannot drift from what is
// deployed, a misspelling is impossible because the object has to exist to carry
// it, and two replicas cannot disagree.
type Set interface {
	// Visible reports whether a tenant may see this class -- and, for storage
	// classes, whether it may name one in a new PersistentVolumeClaim. Those are
	// the same answer on purpose: publication is authorization, not only
	// discovery, so a name learned out of band buys nothing.
	//
	// ⚠️ Which makes REMOVING A LABEL a breaking act, where it used to be a safe
	// and reversible one. See tenantProxy.refuseUnpublishedStorageClass.
	Visible(name string) bool
	// Retired reports whether the class is published but on the way out. Visible
	// stays true for these, which is the whole difference from removing the
	// label: both stop new claims, and only this one leaves the tenant able to
	// see the class and understand why its own claim references it.
	//
	// ⚠️ Read by tenantProxy.refuseUnpublishedStorageClass on the CREATE path ONLY,
	// and that restriction is load-bearing. A PVC's spec.storageClassName is
	// immutable once bound, so a tenant cannot edit its way off a retired class;
	// checking on update would make every later write to an existing claim fail,
	// including a GitOps controller reapplying a manifest it has not changed,
	// turning a retirement into a reconcile loop with no way out.
	Retired(name string) bool
	// Names lists every visible class.
	Names() []string
	// HasSynced reports whether the backing cache has been populated at least
	// once. Until it has, this Set answers only from the static names.
	HasSynced() bool
}

// labelledSet reads the label off objects in an informer's store, and unions
// that with a fixed list.
type labelledSet struct {
	label string
	// store holds the objects the informer selected -- which, because the
	// informer is label-selected, is exactly the published ones.
	store  cache.Store
	synced cache.InformerSynced
	// static are names given on the command line. They are kept because dropping
	// the flag would make an upgrade publish nothing until an operator went and
	// labelled things, which is a silent behaviour change at exactly the wrong
	// moment. A name here is always Visible and never Retired.
	static map[string]bool
	// resource names the thing being published, for log lines.
	resource string
}

var _ Set = &labelledSet{}

// New builds a Set over an informer's store.
func New(resource, label string, store cache.Store, synced cache.InformerSynced, static []string) Set {
	names := make(map[string]bool, len(static))
	for _, name := range static {
		if name != "" {
			names[name] = true
		}
	}
	return &labelledSet{
		label:    label,
		store:    store,
		synced:   synced,
		static:   names,
		resource: resource,
	}
}

// Static builds a Set with no informer behind it, for tests and for a build with
// no upstream client.
func Static(resource string, static []string) Set {
	return New(resource, "", nil, func() bool { return true }, static)
}

func (s *labelledSet) HasSynced() bool {
	return s.synced == nil || s.synced()
}

func (s *labelledSet) value(name string) (string, bool) {
	if s.store == nil || !s.HasSynced() {
		return "", false
	}
	object, exists, err := s.store.GetByKey(name)
	if err != nil {
		// A store lookup does not reach the network; an error here is a
		// programming fault, not a transient one.
		klog.Errorf("reading the published %s %s from the cache: %v", s.resource, name, err)
		return "", false
	}
	if !exists {
		return "", false
	}
	accessor, ok := object.(interface{ GetLabels() map[string]string })
	if !ok {
		return "", false
	}
	published, present := accessor.GetLabels()[s.label]
	return published, present
}

func (s *labelledSet) Visible(name string) bool {
	if s.static[name] {
		return true
	}
	published, present := s.value(name)
	// Any value counts as published. An operator who writes something other than
	// the two known values has still said "offer this"; treating an unrecognised
	// value as unpublished would make a typo hide a class silently, which is the
	// failure this whole mechanism exists to remove.
	return present && published != ""
}

func (s *labelledSet) Retired(name string) bool {
	if s.static[name] {
		return false
	}
	published, present := s.value(name)
	return present && published == common.PublishedDeprecated
}

func (s *labelledSet) Names() []string {
	seen := make(map[string]bool, len(s.static))
	names := make([]string, 0, len(s.static))
	for name := range s.static {
		seen[name] = true
		names = append(names, name)
	}
	if s.store == nil || !s.HasSynced() {
		return names
	}
	for _, object := range s.store.List() {
		accessor, ok := object.(interface {
			GetName() string
			GetLabels() map[string]string
		})
		if !ok {
			continue
		}
		if accessor.GetLabels()[s.label] == "" {
			continue
		}
		if name := accessor.GetName(); !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// PublishedSelector is the selector an informer uses so that its store holds the
// published objects and nothing else.
func PublishedSelector(label string) labels.Selector {
	requirement, err := labels.NewRequirement(label, selection.Exists, nil)
	if err != nil {
		// The label keys are constants; this cannot fail.
		panic(err)
	}
	return labels.NewSelector().Add(*requirement)
}

// IsNotFound is re-exported so callers need not import both packages to tell a
// missing class from a real failure.
func IsNotFound(err error) bool { return errors.IsNotFound(err) }
