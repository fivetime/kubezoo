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
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog"

	"github.com/fivetime/kubezoo-contract/pkg/common"
	kubezoodynamic "github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/util"
)

// initialEventsEndAnnotation marks the bookmark that says a WatchList has
// finished replaying the world.
const initialEventsEndAnnotation = "k8s.io/initial-events-end"

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
// in it, and an informer would carry on believing it was up to date.
//
// The rule the whole file is written to is that this stream never quietly stops
// covering part of the tenant. Whenever it cannot go on covering all of it, it
// says so with a 410 and ends: a client told the revision is gone re-lists and
// repairs itself, and a client told nothing does not. See expire.
//
// What is left, stated rather than papered over: merging N streams means events
// reach the client in per-stream order but not in revision order, so between one
// merged bookmark and the next the client's remembered revision can sit above an
// event another stream has not delivered yet. If the client ends the request in
// exactly that window -- its own timeout, not anything this file can see -- and
// resumes from that revision, the undelivered event is missed. Bookmarks are
// what bound it: onBookmark hands out the minimum across the streams, so the
// client's resume point is dragged back to a safe one roughly once a minute.
// Closing it entirely would mean holding every event back until every other
// stream has reported past it, which costs the bookmark interval in latency on
// every event, and is not worth it.
type watchMux struct {
	resource  kubezoodynamic.NamespaceableResourceInterface
	options   metav1.ListOptions
	tenantID  string
	result    chan watch.Event
	stop      chan struct{}
	stopOnce  sync.Once
	expiring  sync.Once
	forwarder sync.WaitGroup

	// bookmarks reports whether the client asked for them, which is what decides
	// whether this stream can hand out a safe resume point at all.
	bookmarks bool
	// initialEvents reports whether this is a WatchList, whose end-of-replay
	// bookmark has to be held back until every stream has sent its own.
	initialEvents bool

	mu       sync.Mutex
	watched  map[string]watch.Interface
	streams  map[string]*streamProgress
	emitted  uint64
	replayed bool
}

// streamProgress is what one namespace's stream has said about itself.
type streamProgress struct {
	// revision is the highest revision this stream has reported, from an event
	// or a bookmark. The merged stream's own progress is the *minimum* of these:
	// a revision from one namespace says nothing about another.
	revision uint64
	// endOfReplay records that this stream has finished sending initial events.
	endOfReplay bool
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
		resource:      tp.dynamicClient.Resource(gv.WithResource(tp.resource)),
		options:       options,
		tenantID:      tenantID,
		result:        make(chan watch.Event),
		stop:          make(chan struct{}),
		watched:       make(map[string]watch.Interface),
		streams:       make(map[string]*streamProgress),
		bookmarks:     options.AllowWatchBookmarks,
		initialEvents: options.SendInitialEvents != nil && *options.SendInitialEvents,
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
	// Deliberately not the client's timeout. Every stream here would then expire
	// at the same instant as the client's own request, one of them a moment
	// first -- and since a stream ending is now a 410, that would turn every
	// reflector's ordinary five-to-ten-minute cycle into a full re-list of the
	// tenant's every namespace. Left unset, upstream applies its own
	// minRequestTimeout, which floors at half an hour, so these outlive the
	// request that opened them and are torn down with it by Stop(). A 410 then
	// means something actually went wrong, which is what makes it worth being
	// loud about.
	options.TimeoutSeconds = nil

	w, err := m.resource.Namespace(namespace).Watch(ctx, options)
	if err != nil {
		return err
	}
	m.watched[namespace] = w
	// A stream that has said nothing yet has made no progress past where it was
	// opened, so the merged watermark cannot move past that either.
	m.streams[namespace] = &streamProgress{revision: parseRevision(resourceVersion)}

	m.forwarder.Add(1)
	go m.forward(namespace, w)
	return nil
}

// expire ends the merged stream with a 410, and is how this file keeps its one
// promise.
//
// ⚠️ Two earlier versions did something quieter and both lost data the same way.
// The first left a namespace whose upstream stream had ended in the map forever,
// so nothing ever reopened it. The second removed it from the map -- necessary,
// but not sufficient, and the comment claiming it was sufficient was wrong: the
// only thing that reopens a namespace is an Added event from the namespace
// follower, and a namespace that was never deleted never produces another Added.
// So for three of the four ways a stream ends -- the apiserver's own randomised
// watch timeout, an upstream restart, a transient close -- nothing changed: that
// namespace silently stopped appearing while the client's watch stayed open,
// healthy, and wrong.
//
// Reconnecting here instead would not be enough either, and the reason is
// subtler. The revision an informer remembers is the last one it was handed, and
// a revision from one namespace's stream says nothing about another's, so
// resuming from it can skip events a slower stream had not delivered yet. Ending
// with a 410 is the one outcome with no such gap: every client re-lists, exactly
// as it does for a compacted revision, and the fan-out starts again from a single
// consistent snapshot.
func (m *watchMux) expire(format string, args ...interface{}) {
	m.expiring.Do(func() {
		select {
		case <-m.stop:
			// Already torn down, so the client has either been told or has gone.
			// Sending here would be a send on a channel the forwarders' Wait may
			// already have closed, which is a panic rather than a lost message.
			return
		default:
		}
		reason := fmt.Sprintf(format, args...)
		klog.V(2).Infof("ending tenant %s's cross-namespace watch so that it re-lists: %s",
			m.tenantID, reason)
		status := apierrors.NewResourceExpired(reason).ErrStatus
		select {
		case m.result <- watch.Event{Type: watch.Error, Object: &status}:
		case <-m.stop:
		}
		m.Stop()
	})
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
				// This namespace is no longer covered, and no part of the tenant
				// may go uncovered in silence. See expire.
				m.expire("the upstream watch on namespace %s ended", namespace)
				return
			}
			if event.Type == watch.Bookmark {
				// Held back and replaced by one carrying the merged watermark: a
				// bookmark carries the cluster's current revision, so forwarding
				// one verbatim would move the client's resume point past events
				// another stream has not sent yet -- with no activity in this
				// namespace at all.
				m.onBookmark(namespace, event)
				continue
			}
			m.recordProgress(namespace, event)
			select {
			case m.result <- event:
			case <-m.stop:
				return
			}
		}
	}
}

// recordProgress notes how far one stream has got.
func (m *watchMux) recordProgress(namespace string, event watch.Event) {
	revision := eventRevision(event)
	if revision == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, ok := m.streams[namespace]; ok && revision > stream.revision {
		stream.revision = revision
	}
}

// onBookmark folds one stream's bookmark into the merged watermark and emits a
// bookmark of its own if the whole fan-out has moved.
//
// The watermark is the minimum over the streams, which is the only revision a
// client can safely resume this merged stream from. For a WatchList the
// end-of-replay bookmark is held back the same way, until every stream has
// finished replaying -- otherwise the client would start work on a cache holding
// one namespace out of several, and be told it was complete.
func (m *watchMux) onBookmark(namespace string, event watch.Event) {
	revision := eventRevision(event)
	endOfReplay := hasInitialEventsEnd(event)

	m.mu.Lock()
	stream, known := m.streams[namespace]
	if !known {
		m.mu.Unlock()
		return
	}
	if revision > stream.revision {
		stream.revision = revision
	}
	if endOfReplay {
		stream.endOfReplay = true
	}

	watermark := uint64(0)
	allReplayed := true
	first := true
	for _, s := range m.streams {
		if first || s.revision < watermark {
			watermark = s.revision
			first = false
		}
		if !s.endOfReplay {
			allReplayed = false
		}
	}
	announceReplayEnd := m.initialEvents && allReplayed && !m.replayed
	if announceReplayEnd {
		m.replayed = true
	}
	advanced := watermark > m.emitted
	if advanced {
		m.emitted = watermark
	}
	m.mu.Unlock()

	if !m.bookmarks || watermark == 0 {
		return
	}
	if !advanced && !announceReplayEnd {
		return
	}

	merged, ok := bookmarkAt(event, watermark, announceReplayEnd)
	if !ok {
		return
	}
	select {
	case m.result <- merged:
	case <-m.stop:
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
					// ⚠️ This used to return in silence, and it is the worst
					// place in the file to do that: this goroutine is the only
					// thing that ever joins a new namespace, so once it retired
					// every namespace the tenant created afterwards was invisible
					// -- not even the join warning fired, because no join was
					// ever attempted.
					m.expire("the upstream watch on this tenant's namespaces ended")
					return
				}
				if event.Type == watch.Error {
					// Most often a 410 on the namespace watch. Swallowing it left
					// a stream that could no longer learn about new namespaces
					// and did not know it.
					m.expire("the watch on this tenant's namespaces reported: %v", event.Object)
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
				metaObj, hasRV := event.Object.(interface{ GetResourceVersion() string })
				startAt := ""
				if hasRV {
					startAt = metaObj.GetResourceVersion()
				}
				// Joined on its own goroutine, and with retries, because the
				// namespace arrives before the tenant may read it. The
				// RoleBinding is written by the controller and then has to
				// reach the authorizer's cache -- measured at roughly 170ms to
				// exist and 310ms to take effect -- so the first attempt is
				// reliably Forbidden. Retrying here keeps the follower free to
				// notice the next namespace meanwhile.
				//
				// Counted in the same WaitGroup as the forwarders, because a
				// join that fails ends the merged stream and so has something
				// to say on m.result. Uncounted, the last forwarder could retire
				// and close that channel while this goroutine was still holding
				// the news -- a panic, not a lost message. Added here rather than
				// inside, while this goroutine's own count keeps the group above
				// zero.
				m.forwarder.Add(1)
				go func(namespace, startAt string) {
					defer m.forwarder.Done()
					m.join(ctx, namespace, startAt)
				}(accessor.GetName(), startAt)
			}
		}
	}()
	return nil
}

// join adds a namespace to the merged stream, waiting out the moment where it
// exists but the tenant cannot yet read it.
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
	// waiting too long is a late event.
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
		"not be joined to the stream: %v", m.tenantID, namespace, err)
	// A log line is not enough on its own: the client would keep a stream that
	// looks healthy and never mentions the namespace again.
	m.expire("namespace %s could not be joined to this watch", namespace)
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

// parseRevision reads a resourceVersion as the number it is upstream. Anything
// that is not one -- "", "0" -- reads as no progress, which keeps the merged
// watermark where it is rather than moving it somewhere unearned.
func parseRevision(resourceVersion string) uint64 {
	revision, err := strconv.ParseUint(resourceVersion, 10, 64)
	if err != nil {
		return 0
	}
	return revision
}

func eventRevision(event watch.Event) uint64 {
	accessor, err := meta.Accessor(event.Object)
	if err != nil {
		return 0
	}
	return parseRevision(accessor.GetResourceVersion())
}

func hasInitialEventsEnd(event watch.Event) bool {
	accessor, err := meta.Accessor(event.Object)
	if err != nil {
		return false
	}
	return accessor.GetAnnotations()[initialEventsEndAnnotation] == "true"
}

// bookmarkAt restamps a bookmark with the merged watermark.
func bookmarkAt(event watch.Event, revision uint64, endOfReplay bool) (watch.Event, bool) {
	copied := event.Object.DeepCopyObject()
	accessor, err := meta.Accessor(copied)
	if err != nil {
		return watch.Event{}, false
	}
	accessor.SetResourceVersion(strconv.FormatUint(revision, 10))
	annotations := accessor.GetAnnotations()
	if endOfReplay {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[initialEventsEndAnnotation] = "true"
	} else if annotations != nil {
		delete(annotations, initialEventsEndAnnotation)
	}
	accessor.SetAnnotations(annotations)
	return watch.Event{Type: watch.Bookmark, Object: copied}, true
}
