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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"

	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"
)

// fakeWatchResource hands out one controllable watch per namespace. Only Watch
// is implemented; anything else this test reaches would be a test bug, and the
// embedded nil interface says so loudly.
type fakeWatchResource struct {
	kubezoodynamic.NamespaceableResourceInterface
	watchers map[string]*watch.FakeWatcher
}

type fakeNamespacedResource struct {
	kubezoodynamic.ResourceInterface
	watcher *watch.FakeWatcher
}

func (f *fakeWatchResource) Namespace(namespace string) kubezoodynamic.ResourceInterface {
	return &fakeNamespacedResource{watcher: f.watchers[namespace]}
}

func (f *fakeNamespacedResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return f.watcher, nil
}

// newTestMux builds a mux over the given namespaces without the namespace
// follower, which needs a whole tenantProxy and is not what these tests are
// about.
func newTestMux(t *testing.T, options metav1.ListOptions, namespaces ...string) (*watchMux, map[string]*watch.FakeWatcher) {
	t.Helper()
	watchers := map[string]*watch.FakeWatcher{}
	for _, namespace := range namespaces {
		watchers[namespace] = watch.NewFake()
	}
	m := &watchMux{
		resource:      &fakeWatchResource{watchers: watchers},
		options:       options,
		tenantID:      "111111",
		result:        make(chan watch.Event),
		stop:          make(chan struct{}),
		watched:       map[string]watch.Interface{},
		streams:       map[string]*streamProgress{},
		bookmarks:     options.AllowWatchBookmarks,
		initialEvents: options.SendInitialEvents != nil && *options.SendInitialEvents,
	}
	for _, namespace := range namespaces {
		if err := m.add(context.Background(), namespace, options.ResourceVersion); err != nil {
			t.Fatalf("opening %s: %v", namespace, err)
		}
	}
	go func() {
		m.forwarder.Wait()
		close(m.result)
	}()
	t.Cleanup(m.Stop)
	return m, watchers
}

func objectAt(revision string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{}}
	object.SetAPIVersion("v1")
	object.SetKind("ConfigMap")
	object.SetResourceVersion(revision)
	return object
}

// TestStreamEndingIsLoud is the guard on the failure this file has now had twice.
//
// One namespace's upstream stream ends -- an apiserver's randomised watch
// timeout, one instance of an HA set restarting, a reset connection -- while the
// namespace itself still exists. Both earlier versions carried on serving the
// remaining namespaces and told the client nothing, so an informer's cache was
// silently missing everything in that namespace until something else ended the
// stream, which is the one outcome this package exists to prevent.
func TestStreamEndingIsLoud(t *testing.T) {
	m, watchers := newTestMux(t, metav1.ListOptions{ResourceVersion: "1000"}, "111111-a", "111111-b")

	// The other namespace is healthy and stays healthy: it is what used to hold
	// the merged stream open and looking fine.
	go watchers["111111-b"].Add(objectAt("1001"))
	select {
	case event := <-m.ResultChan():
		if event.Type != watch.Added {
			t.Fatalf("expected the healthy namespace's event, got %v", event.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event from the healthy namespace")
	}

	watchers["111111-a"].Stop()

	select {
	case event, open := <-m.ResultChan():
		if !open {
			t.Fatal("the merged stream closed without telling the client why; " +
				"a client that is told nothing does not re-list")
		}
		if event.Type != watch.Error {
			t.Fatalf("a namespace stopped being covered and the client was sent a %v, not an error", event.Type)
		}
		status, ok := event.Object.(*metav1.Status)
		if !ok {
			t.Fatalf("error event carried a %T, not a Status", event.Object)
		}
		if !apierrors.IsResourceExpired(&apierrors.StatusError{ErrStatus: *status}) {
			t.Errorf("the error was not a 410, so the client will not re-list: %+v", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a namespace stopped being covered and the merged stream carried on in silence")
	}

	select {
	case _, open := <-m.ResultChan():
		if open {
			t.Error("the merged stream went on delivering after it expired")
		}
	case <-time.After(5 * time.Second):
		t.Error("the merged stream did not end after expiring")
	}
}

// TestBookmarkIsTheMinimumAcrossStreams pins the resume point.
//
// Every stream's bookmark carries the cluster's current revision, not that
// namespace's, so forwarding one verbatim moves the client's remembered revision
// past events another stream has not delivered yet. A reconnect from there skips
// them for good, and reflectors re-watch from the last revision they saw without
// re-listing.
func TestBookmarkIsTheMinimumAcrossStreams(t *testing.T) {
	m, watchers := newTestMux(t,
		metav1.ListOptions{ResourceVersion: "1000", AllowWatchBookmarks: true},
		"111111-a", "111111-b")

	// b has delivered through 1200 and may hold anything above it.
	go watchers["111111-b"].Modify(objectAt("1200"))
	if event := <-m.ResultChan(); event.Type != watch.Modified {
		t.Fatalf("expected b's event, got %v", event.Type)
	}

	// a now sends a bookmark. The cacher stamps every bookmark with the
	// cluster's current revision, not the namespace's, so this one says 5000
	// while saying nothing at all about b.
	go watchers["111111-a"].Action(watch.Bookmark, objectAt("5000"))

	select {
	case event := <-m.ResultChan():
		if event.Type != watch.Bookmark {
			t.Fatalf("expected a bookmark, got %v", event.Type)
		}
		got := event.Object.(*unstructured.Unstructured).GetResourceVersion()
		if got == "5000" {
			t.Error("a's bookmark went through verbatim: the client now remembers 5000 and would " +
				"resume there, skipping everything b holds between 1200 and 5000")
		}
		if got != "1200" {
			t.Errorf("bookmark handed the client revision %s, want the minimum across streams, 1200", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no bookmark reached the client")
	}
}

// TestExpireAfterStopDoesNotPanic covers the window a failing join opens.
//
// A join retries for thirty seconds and ends the merged stream if it never
// succeeds, so it has something to say long after the rest of the mux may have
// retired. m.result is closed once the forwarders are done, and a send on a
// closed channel is a panic rather than a lost message -- which is why join is
// counted in the same WaitGroup, and why expire checks before it speaks.
func TestExpireAfterStopDoesNotPanic(t *testing.T) {
	m, _ := newTestMux(t, metav1.ListOptions{ResourceVersion: "1000"}, "111111-a")

	m.Stop()
	// Everything has retired and m.result is closed by now.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, open := <-m.ResultChan(); !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the merged stream never closed after Stop")
		}
	}

	m.expire("a join that outlived the stream")
}
