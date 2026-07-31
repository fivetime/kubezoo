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
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// watchMux presents one watch stream to the client while holding one upstream
// watch per namespace the tenant owns.
//
// The tenant has no cluster to watch, so a request without a namespace becomes
// as many watches as it has namespaces. This is the other half of the
// cross-namespace list: the list hands back a revision that is a real snapshot,
// and every stream here starts from it, so the contract an informer relies on --
// list, then watch from the revision the list returned -- holds.
//
// Namespaces created after the watch opened get their own stream as they appear.
// Without that a tenant creating a namespace mid-watch would never see anything
// in it, and an informer would carry on believing it was up to date. Silently
// missing objects is the failure worth spending code on; the alternative is a
// cache that is wrong and says nothing.
type watchMux struct {
	resource  kubezoodynamic.NamespaceableResourceInterface
	options   metav1.ListOptions
	tenantID  string
	result    chan watch.Event
	stop      chan struct{}
	stopOnce  sync.Once
	forwarder sync.WaitGroup

	mu      sync.Mutex
	watched map[string]watch.Interface
}

var _ watch.Interface = &watchMux{}

// newWatchMux opens a watch on each namespace and one on the namespace list
// itself, and merges them.
func newWatchMux(ctx context.Context, tp *tenantProxy, tenantID string,
	options metav1.ListOptions, namespaces []string) (*watchMux, error) {

	gv := tp.kind.GroupVersion()
	if tp.isCustomResource {
		gv.Group = util.AddTenantIDPrefix(tenantID, gv.Group)
	}

	m := &watchMux{
		resource: tp.dynamicClient.Resource(gv.WithResource(tp.resource)),
		options:  options,
		tenantID: tenantID,
		result:   make(chan watch.Event),
		stop:     make(chan struct{}),
		watched:  make(map[string]watch.Interface),
	}

	for _, namespace := range namespaces {
		if err := m.add(ctx, namespace, options.ResourceVersion); err != nil {
			// Opening even one of them failed, so the stream would be short
			// without saying so. Tear down what did open and let the caller
			// report it.
			m.Stop()
			return nil, err
		}
	}

	if err := m.followNamespaces(ctx, tp); err != nil {
		m.Stop()
		return nil, err
	}

	go func() {
		m.forwarder.Wait()
		close(m.result)
	}()
	return m, nil
}

// add opens one namespace's watch and starts forwarding it.
func (m *watchMux) add(ctx context.Context, namespace, resourceVersion string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, already := m.watched[namespace]; already {
		return nil
	}

	options := m.options
	options.ResourceVersion = resourceVersion
	// A watch is a stream, not a page; the list's cursor means nothing here and
	// upstream refuses the two together.
	options.Continue = ""
	options.Limit = 0

	w, err := m.resource.Namespace(namespace).Watch(ctx, options)
	if err != nil {
		return err
	}
	m.watched[namespace] = w

	m.forwarder.Add(1)
	go m.forward(namespace, w)
	return nil
}

// forget drops a namespace whose stream has ended, so that a later event for it
// can open a new one.
//
// Deliberately not re-opening from here. This forwarder does not know why its
// stream ended, and reconnecting on a namespace that was deleted would spin.
// followNamespaces sees an Added event if the namespace comes back, and that
// path already waits out the window where the tenant cannot read it yet.
func (m *watchMux) forget(namespace string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watched, namespace)
}

// forward pumps one upstream stream into the merged one.
func (m *watchMux) forward(namespace string, w watch.Interface) {
	defer m.forwarder.Done()
	defer w.Stop()
	for {
		select {
		case <-m.stop:
			return
		case event, open := <-w.ResultChan():
			if !open {
				// One namespace's stream ended. The others are still good, so
				// this forwarder retires rather than ending the client's watch --
				// but it has to forget the namespace on the way out, or the
				// namespace is gone from the merged stream for good.
				//
				// ⚠️ This used to just return. add() refuses a namespace already
				// in the map and nothing ever removed one, so the first time a
				// single stream ended for any reason other than the whole mux
				// stopping -- the apiserver's own randomised watch timeout, an
				// upstream restart, a transient error, or the namespace being
				// deleted and recreated under the same name -- that namespace
				// stopped appearing and the client's watch stayed open reporting
				// nothing wrong. A cache that is wrong and says nothing, which is
				// the failure this file exists to avoid.
				m.forget(namespace)
				return
			}
			select {
			case m.result <- event:
			case <-m.stop:
				return
			}
		}
	}
}

// followNamespaces watches the tenant's namespaces so that one created after
// this watch opened still gets a stream.
func (m *watchMux) followNamespaces(ctx context.Context, tp *tenantProxy) error {
	options := metav1.ListOptions{
		LabelSelector:   m.options.LabelSelector,
		ResourceVersion: m.options.ResourceVersion,
	}
	options.LabelSelector = tenantNamespaceSelector(m.tenantID)

	// As the tenant, for the same reason the list enumerates them that way: which
	// namespaces exist is kubezoo's business, and the streams opened on them are
	// still the caller's.
	w, err := tp.dynamicClient.Resource(namespaceGVR).Watch(asTenantAdmin(ctx, m.tenantID), options)
	if err != nil {
		return err
	}

	m.forwarder.Add(1)
	go func() {
		defer m.forwarder.Done()
		defer w.Stop()
		for {
			select {
			case <-m.stop:
				return
			case event, open := <-w.ResultChan():
				if !open {
					return
				}
				if event.Type != watch.Added {
					continue
				}
				accessor, ok := event.Object.(interface{ GetName() string })
				if !ok {
					continue
				}
				// From the namespace's own revision: it is new, so it holds
				// nothing older, and starting at the watch's original revision
				// could be far enough back to be compacted.
				meta, hasRV := event.Object.(interface{ GetResourceVersion() string })
				startAt := ""
				if hasRV {
					startAt = meta.GetResourceVersion()
				}
				// Joined on its own goroutine, and with retries, because the
				// namespace arrives before the tenant may read it. The
				// RoleBinding is written by the controller and then has to
				// reach the authorizer's cache -- measured at roughly 170ms to
				// exist and 310ms to take effect -- so the first attempt is
				// reliably Forbidden. Retrying here keeps the follower free to
				// notice the next namespace meanwhile.
				go m.join(ctx, accessor.GetName(), startAt)
			}
		}
	}()
	return nil
}

// join adds a namespace to the merged stream, waiting out the moment where it
// exists but the tenant cannot yet read it.
//
// Giving up in silence would be the bad outcome: the client keeps a stream that
// looks healthy and never mentions the namespace again. So a failure that
// outlasts the retries is reported, and the message says what the client has to
// do about it.
func (m *watchMux) join(ctx context.Context, namespace, resourceVersion string) {
	// The budget is what a new namespace costs to become readable, not a guess.
	// The tenant controller has to notice the namespace, write the RoleBinding,
	// and then the authorizer's cache has to catch up -- measured at roughly
	// 170ms and 310ms when that controller ran inside this process and had one
	// job. It is its own process now and does more per namespace, and the same
	// wait was measured at about six seconds.
	//
	// ⚠️ Three seconds was not merely tight, it was silent: join gave up, logged,
	// and the client kept a stream that looked healthy and never mentioned the
	// namespace again. Thirty seconds is generous on purpose -- the cost of
	// waiting too long is a late event, and the cost of waiting too little is a
	// cache that is wrong and says nothing.
	const attempts = 60
	backoff := 500 * time.Millisecond
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = m.add(ctx, namespace, resourceVersion); err == nil {
			return
		}
		if !apierrors.IsForbidden(err) {
			break
		}
		select {
		case <-m.stop:
			return
		case <-time.After(backoff):
		}
	}
	klog.Warningf("tenant %s created namespace %s during a cross-namespace watch and it could "+
		"not be joined to the stream; the client will not see objects in it until it re-lists: %v",
		m.tenantID, namespace, err)
}

// ResultChan implements watch.Interface.
func (m *watchMux) ResultChan() <-chan watch.Event {
	return m.result
}

// Stop implements watch.Interface.
func (m *watchMux) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, w := range m.watched {
			w.Stop()
		}
	})
}

func tenantNamespaceSelector(tenantID string) string {
	return fmt.Sprintf("%s=%s", common.TenantNamespaceLabelKey, tenantID)
}
