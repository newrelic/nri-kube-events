package objects

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// pagedList serves n ConfigMaps in pages of the requested limit, with continue tokens, and
// records the limit and the most objects one response held.
type pagedList struct {
	limits  []int64
	n       int
	mu      sync.Mutex
	maxPage int
}

// pagingDynamic serves List of every resource from list, which sees the real ListOptions;
// the fake dynamic client's reactors drop Limit and Continue.
type pagingDynamic struct {
	dynamic.Interface
	list func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
}

func (d pagingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return pagingResource{NamespaceableResourceInterface: d.Interface.Resource(gvr), list: d.list}
}

type pagingResource struct {
	dynamic.NamespaceableResourceInterface
	list func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
}

func (r pagingResource) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return r.list(opts)
}

func pagingWatcher(t *testing.T, list func(metav1.ListOptions) (*unstructured.UnstructuredList, error)) (*Watcher, *Store, *recordingSink) {
	t.Helper()
	store := NewStore(newClient(), testNS)
	require.NoError(t, store.Load(context.Background()))
	sink := &recordingSink{}
	dyn := pagingDynamic{Interface: newDynamic(), list: list}
	w := NewWatcher(testConfig(), dyn, newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	w.track(cmGVR, "ConfigMap")
	return w, store, sink
}

func (p *pagedList) list(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = append(p.limits, opts.Limit)
	start := 0
	if opts.Continue != "" {
		start, _ = strconv.Atoi(opts.Continue)
	}
	end := min(p.n, start+int(opts.Limit))
	list := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMapList"}}
	for i := start; i < end; i++ {
		list.Items = append(list.Items, *configMap(fmt.Sprintf("cm-%d", i), fmt.Sprintf("uid-%d", i), "v"))
	}
	p.maxPage = max(p.maxPage, len(list.Items))
	if end < p.n {
		list.SetContinue(strconv.Itoa(end))
	}
	list.SetResourceVersion("100")
	return list, nil
}

// A kind is listed in pages of listPageSize, and every page is handled before the kind
// counts as listed.
func TestListKindPagesAndHandlesEveryPage(t *testing.T) {
	pages := &pagedList{n: 2*listPageSize + 7}
	w, _, sink := pagingWatcher(t, pages.list)
	s := w.newObjectStore(context.Background(), cmGVR)

	rv, err := w.listKind(context.Background(), cmGVR, s)
	require.NoError(t, err)
	assert.Equal(t, "100", rv)
	assert.Equal(t, []int64{listPageSize, listPageSize, listPageSize}, pages.limits)
	assert.Equal(t, listPageSize, pages.maxPage)
	assert.Len(t, sink.objectRecords(), 2*listPageSize+7)
	assert.True(t, cfgListed(w, cmGVR))
}

func cfgListed(w *Watcher, gvr schema.GroupVersionResource) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	return ok && st.listed
}

// A continue token that expires mid-list starts the list over; the objects of the first
// pages are not sent twice, because their hashes are already stored.
func TestListKindRestartsOnExpiredContinue(t *testing.T) {
	pages := &pagedList{n: listPageSize + 1}
	var expired atomic.Bool
	w, store, sink := pagingWatcher(t, func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		if opts.Continue != "" && expired.CompareAndSwap(false, true) {
			return nil, apierrors.NewResourceExpired("continue token expired")
		}
		return pages.list(opts)
	})
	_, err := w.listKind(context.Background(), cmGVR, w.newObjectStore(context.Background(), cmGVR))
	require.NoError(t, err)
	assert.Len(t, sink.objectRecords(), listPageSize+1)
	assert.Len(t, storeUIDs(store), listPageSize+1)
}

// At most listConcurrency kinds are listed at once.
func TestListConcurrencyIsBounded(t *testing.T) {
	var lists []*metav1.APIResourceList
	gvrs := map[schema.GroupVersionResource]string{}
	for i := 0; i < 6; i++ {
		g := fmt.Sprintf("g%d.example.com", i)
		lists = append(lists, &metav1.APIResourceList{GroupVersion: g + "/v1", APIResources: []metav1.APIResource{
			{Name: "things", Kind: "Thing", Namespaced: true, Verbs: watchVerbs},
		}})
		gvrs[schema.GroupVersionResource{Group: g, Version: "v1", Resource: "things"}] = "ThingList"
	}
	dyn := newDynamicWith(gvrs)
	var inFlight, peak atomic.Int32
	dyn.PrependReactor("list", "things", func(k8stesting.Action) (bool, runtime.Object, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "ThingList"}}, nil
	})
	store := NewStore(newClient(), testNS)
	w := NewWatcher(testConfig(), dyn, newTestDiscovery(lists...), store, &recordingSink{}, watcherKey)
	stop := runWatcher(t, w)
	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.gvrs) == 6 && w.allResolvedLocked()
	}, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, stop())
	assert.LessOrEqual(t, int(peak.Load()), listConcurrency)
	assert.Positive(t, int(peak.Load()))
}

// A watch that reports its resource version expired leads to a new list, which sends the
// change the watch could not deliver.
func TestExpiredWatchRelists(t *testing.T) {
	w, dyn, store, sink := newTestWatcher(t, testConfig(), nil, configMap("a", "uid-a", "v1"))
	fw := watch.NewFake()
	var watches atomic.Int32
	dyn.PrependWatchReactor("configmaps", func(k8stesting.Action) (bool, watch.Interface, error) {
		if watches.Add(1) == 1 {
			return true, fw, nil
		}
		return false, nil, nil
	})
	runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return watches.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	// The change lands while the watch is broken, so only a relist can see it.
	_, err := dyn.Resource(cmGVR).Namespace("default").Update(context.Background(), configMap("a", "uid-a", "v2"), metav1.UpdateOptions{})
	require.NoError(t, err)
	fw.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired, Message: "too old resource version"})
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "updated:uid-a", ops(sink.objectRecords())[1])
	_, hashV2 := prepareObject(cmGVR, "ConfigMap", configMap("a", "uid-a", "v2").Object, watcherKey)
	require.Eventually(t, func() bool {
		e, ok := store.Get("uid-a")
		return ok && e.Hash == hashV2
	}, 5*time.Second, 5*time.Millisecond)
}

// A list that fails is reported in carrier.failedKinds and tried again.
func TestFailedListIsReportedAndRetried(t *testing.T) {
	w, dyn, _, sink := newTestWatcher(t, testConfig(), nil, configMap("a", "uid-a", "v1"))
	var calls atomic.Int32
	dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) == 1 {
			return true, nil, apierrors.NewForbidden(cmGVR.GroupResource(), "", fmt.Errorf("no"))
		}
		return false, nil, nil
	})
	runWatcher(t, w)
	require.Eventually(t, func() bool {
		return w.HeartbeatRecord(time.Now()).Attributes["carrier.failedKinds"] == "/v1/configmaps"
	}, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "", w.HeartbeatRecord(time.Now()).Attributes["carrier.failedKinds"])
}

func newDynamicWith(kinds map[schema.GroupVersionResource]string) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
}
