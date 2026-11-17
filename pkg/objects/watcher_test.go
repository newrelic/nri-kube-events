package objects

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const (
	testCluster = "test-cluster"
	testNS      = "newrelic"
)

// cmGVR is declared in record_test.go.
var (
	secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	widgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	crdGVR    = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// recordingSink keeps every record sent to it. When fail is set, Send returns an error and
// records nothing.
type recordingSink struct {
	recs  []Record
	mu    sync.Mutex
	calls int
	fail  bool
}

func (s *recordingSink) Send(_ context.Context, recs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail {
		return errors.New("sink down")
	}
	s.recs = append(s.recs, recs...)
	return nil
}

func (s *recordingSink) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *recordingSink) records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.recs...)
}

func (s *recordingSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = nil
	s.calls = 0
}

// objectRecords returns the object records and tombstones, leaving out heartbeats.
func (s *recordingSink) objectRecords() []Record {
	var out []Record
	for _, r := range s.records() {
		if r.Attributes["type"] == ObjectType {
			out = append(out, r)
		}
	}
	return out
}

// testDiscovery serves a resource list that a test can change while the watcher runs.
// FakeDiscovery reads its Resources field without a lock, so every call works on a snapshot.
type testDiscovery struct {
	*discoveryfake.FakeDiscovery
	lists []*metav1.APIResourceList
	mu    sync.Mutex
}

func newTestDiscovery(lists ...*metav1.APIResourceList) *testDiscovery {
	return &testDiscovery{FakeDiscovery: &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{}}, lists: lists}
}

func (d *testDiscovery) set(lists ...*metav1.APIResourceList) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lists = lists
}

func (d *testDiscovery) snapshot() *discoveryfake.FakeDiscovery {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{Resources: append([]*metav1.APIResourceList(nil), d.lists...)}}
}

func (d *testDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return d.snapshot().ServerGroups()
}

func (d *testDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	return d.snapshot().ServerResourcesForGroupVersion(gv)
}

// client-go v0.37 discovery calls the WithContext methods, which the embedded
// FakeDiscovery would otherwise answer from its own empty Resources.
func (d *testDiscovery) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	return d.snapshot().ServerGroupsWithContext(ctx)
}

func (d *testDiscovery) ServerResourcesForGroupVersionWithContext(ctx context.Context, gv string) (*metav1.APIResourceList, error) {
	return d.snapshot().ServerResourcesForGroupVersionWithContext(ctx, gv)
}

var watchVerbs = metav1.Verbs{"get", "list", "watch"}

func coreList(resources ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "v1", APIResources: resources}
}

func cmResource() metav1.APIResource {
	return metav1.APIResource{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: watchVerbs}
}

func secretResource() metav1.APIResource {
	return metav1.APIResource{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: watchVerbs}
}

func crdList() *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
		{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition", Verbs: watchVerbs},
	}}
}

func widgetList() *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
		{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: watchVerbs},
	}}
}

func newDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cmGVR:     "ConfigMapList",
		secretGVR: "SecretList",
		widgetGVR: "WidgetList",
		crdGVR:    "CustomResourceDefinitionList",
	}, objs...)
}

func configMap(name, uid, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "default",
			"uid":             uid,
			"resourceVersion": "1",
		},
		"data": map[string]any{"k": value},
	}}
}

func widget(name, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": "default", "uid": uid},
		"spec":       map[string]any{"size": int64(1)},
	}}
}

func widgetCRD(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "widgets.example.com", "uid": uid},
		"spec": map[string]any{
			"group":    "example.com",
			"names":    map[string]any{"plural": "widgets", "kind": "Widget"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
		},
	}}
}

func testConfig() Config {
	return Config{
		ClusterName:       testCluster,
		Namespace:         testNS,
		DiscoveryInterval: time.Hour,
		HeartbeatInterval: time.Hour,
		FlushInterval:     time.Hour,
	}
}

var watcherKey = []byte("0123456789abcdef0123456789abcdef")

func newTestWatcher(t *testing.T, cfg Config, disc *testDiscovery, objs ...runtime.Object) (*Watcher, *dynamicfake.FakeDynamicClient, *Store, *recordingSink) {
	t.Helper()
	dyn := newDynamic(objs...)
	store := NewStore(newClient(), testNS)
	sink := &recordingSink{}
	if disc == nil {
		disc = newTestDiscovery(coreList(cmResource()))
	}
	return NewWatcher(cfg, dyn, disc, store, sink, watcherKey), dyn, store, sink
}

// runWatcher starts w.Run and returns a function that stops it and returns Run's error.
// watcherOnly is newTestWatcher for tests that need only the Watcher.
func watcherOnly(t *testing.T, cfg Config, disc *testDiscovery) *Watcher {
	t.Helper()
	w, _, _, _ := newTestWatcher(t, cfg, disc) //nolint:dogsled // the one place that drops the other three
	return w
}

func runWatcher(t *testing.T, w *Watcher) func() error {
	t.Helper()
	// main loads the store before Run, as here.
	require.NoError(t, w.store.Load(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancel")
			}
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// waitForWatch waits until the fake dynamic client has seen a watch on gvr, so objects
// created afterwards reach the reflector as watch events.
func waitForWatch(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "watch" && a.GetResource() == gvr {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "no watch on %s", GVRString(gvr))
}

func ops(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Attributes["k8s.op"].(string)+":"+r.Attributes["k8s.uid"].(string))
	}
	return out
}

func TestFirstSeenObjectSendsInitialRecord(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	w.now = func() time.Time { return time.Unix(1700000000, 0) }

	cm := configMap("a", "uid-a", "v1")
	require.NoError(t, w.HandleObject(context.Background(), cmGVR, OpInitial, cm.Object))

	recs := sink.records()
	require.Len(t, recs, 1)
	assert.Equal(t, "initial", recs[0].Attributes["k8s.op"])
	assert.Equal(t, "uid-a", recs[0].Attributes["k8s.uid"])

	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, Entry{
		GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "a",
		Hash: recs[0].Attributes["object.hash"].(string), LastSent: 1700000000,
	}, e)
}

func TestWatcherRunSendsInitialListAndWatchEvents(t *testing.T) {
	w, dyn, store, sink := newTestWatcher(t, testConfig(), nil, configMap("a", "uid-a", "v1"))
	stop := runWatcher(t, w)

	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"initial:uid-a"}, ops(sink.objectRecords()))

	waitForWatch(t, dyn, cmGVR)
	_, err := dyn.Resource(cmGVR).Namespace("default").Create(context.Background(), configMap("b", "uid-b", "v1"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "added:uid-b", ops(sink.objectRecords())[1])

	require.NoError(t, stop())
	_, ok := store.Get("uid-b")
	assert.True(t, ok)
}

func TestRelistOfUnchangedObjectsSendsNothing(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	s := w.newObjectStore(context.Background(), cmGVR)

	items := func() []any { return []any{configMap("a", "uid-a", "v1"), configMap("b", "uid-b", "v1")} }
	require.NoError(t, s.Replace(items(), "10"))
	assert.Equal(t, []string{"initial:uid-a", "initial:uid-b"}, sortedOps(sink.records()))

	sink.reset()
	require.NoError(t, s.Replace(items(), "20"))
	assert.Empty(t, sink.records())

	// A relist that finds a changed object sends it as an update.
	changed := configMap("a", "uid-a", "v2")
	require.NoError(t, s.Replace([]any{changed, configMap("b", "uid-b", "v1")}, "30"))
	assert.Equal(t, []string{"updated:uid-a"}, ops(sink.records()))
}

func sortedOps(recs []Record) []string {
	out := ops(recs)
	sort.Strings(out)
	return out
}

func TestUpdateWithOnlyManagedFieldsChangeSendsNothing(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	s := w.newObjectStore(context.Background(), cmGVR)
	require.NoError(t, s.Replace([]any{configMap("a", "uid-a", "v1")}, "1"))
	sink.reset()

	cm := configMap("a", "uid-a", "v1")
	meta := cm.Object["metadata"].(map[string]any)
	meta["resourceVersion"] = "2"
	meta["managedFields"] = []any{map[string]any{"manager": "kubectl", "operation": "Update"}}
	require.NoError(t, s.Update(cm))
	assert.Empty(t, sink.records())

	cm2 := configMap("a", "uid-a", "v2")
	require.NoError(t, s.Update(cm2))
	assert.Equal(t, []string{"updated:uid-a"}, ops(sink.records()))
}

func TestDeleteSendsTombstoneAndForgets(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	s := w.newObjectStore(context.Background(), cmGVR)
	require.NoError(t, s.Replace([]any{configMap("a", "uid-a", "v1"), configMap("b", "uid-b", "v1")}, "1"))
	sink.reset()

	require.NoError(t, s.Delete(configMap("a", "uid-a", "v1")))
	recs := sink.records()
	require.Len(t, recs, 1)
	assert.Equal(t, "deleted", recs[0].Attributes["k8s.op"])
	assert.Equal(t, "uid-a", recs[0].Attributes["k8s.uid"])
	_, ok := store.Get("uid-a")
	assert.False(t, ok)

	// A final state the reflector lost carries no object: the stored identity names it.
	sink.reset()
	require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: "default/b"}))
	recs = sink.records()
	require.Len(t, recs, 1)
	assert.Equal(t, "deleted", recs[0].Attributes["k8s.op"])
	assert.Equal(t, "uid-b", recs[0].Attributes["k8s.uid"])
	assert.Equal(t, 0, recs[0].Attributes["object.parts"])
	_, ok = store.Get("uid-b")
	assert.False(t, ok)

	// A failed tombstone keeps the entry, so a later list still names the delete.
	require.NoError(t, s.Replace([]any{configMap("c", "uid-c", "v1")}, "2"))
	sink.setFail(true)
	require.NoError(t, s.Delete(configMap("c", "uid-c", "v1")))
	_, ok = store.Get("uid-c")
	assert.True(t, ok)
}

func TestStartupTombstoneForObjectGoneWhileDown(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource()))
	w, _, store, sink := newTestWatcher(t, testConfig(), disc, configMap("a", "uid-a", "v1"))

	// The previous carrier stored uid-gone and flushed it; this carrier loads it at start.
	store.Put("uid-gone", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "gone", Hash: "0000000000000000", LastSent: 1})
	require.NoError(t, store.Flush(context.Background()))
	store.Delete("uid-gone")

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())

	assert.Equal(t, []string{"deleted:uid-gone", "initial:uid-a"}, sortedOps(sink.objectRecords()))
	for _, r := range sink.objectRecords() {
		if r.Attributes["k8s.uid"] == "uid-gone" {
			assert.Equal(t, "gone", r.Attributes["k8s.name"])
			assert.Equal(t, "ConfigMap", r.Attributes["k8s.kind"])
			assert.Equal(t, 0, r.Attributes["object.parts"])
		}
	}
	_, ok := store.Get("uid-gone")
	assert.False(t, ok)
}

func TestStartupNoTombstoneWhenListFails(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource()), widgetList())
	w, dyn, store, sink := newTestWatcher(t, testConfig(), disc, configMap("a", "uid-a", "v1"))
	dyn.PrependReactor("list", "widgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "example.com", Resource: "widgets"}, "", errors.New("no"))
	})
	store.Put("uid-w", Entry{GVR: "example.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "w", Hash: "0000000000000000", LastSent: 1})
	require.NoError(t, store.Flush(context.Background()))

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 10*time.Millisecond)
	// Give the failing kind time to retry its list.
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, stop())

	assert.Equal(t, []string{"initial:uid-a"}, ops(sink.objectRecords()))
	_, ok := store.Get("uid-w")
	assert.True(t, ok)

	// The heartbeat lists the configmaps but not the kind whose list failed.
	hb := w.HeartbeatRecord(time.Now())
	assert.Equal(t, "~ConfigMap=1,", hb.Attributes["carrier.kinds.p000"])
	assert.NotZero(t, hb.Attributes["lastFullListTime"])
}

func TestFailedSendKeepsOldHash(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	ctx := context.Background()
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	before, _ := store.Get("uid-a")

	sink.setFail(true)
	err := w.HandleObject(ctx, cmGVR, OpUpdated, configMap("a", "uid-a", "v2").Object)
	require.Error(t, err)
	after, _ := store.Get("uid-a")
	assert.Equal(t, before, after)
	assert.Equal(t, int64(1), w.HeartbeatRecord(time.Now()).Attributes["failedSends"])

	sink.setFail(false)
	sink.reset()
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpUpdated, configMap("a", "uid-a", "v2").Object))
	assert.Equal(t, []string{"updated:uid-a"}, ops(sink.records()))
	now, _ := store.Get("uid-a")
	assert.NotEqual(t, before.Hash, now.Hash)
}

func TestEventsResourceNotWatched(t *testing.T) {
	disc := newTestDiscovery(
		coreList(
			cmResource(),
			metav1.APIResource{Name: "events", Kind: "Event", Namespaced: true, Verbs: watchVerbs},
			metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: watchVerbs},
			metav1.APIResource{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
			metav1.APIResource{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: metav1.Verbs{"create"}},
			metav1.APIResource{Name: "componentstatuses", Kind: "ComponentStatus", Verbs: metav1.Verbs{"get", "list"}},
		),
		&metav1.APIResourceList{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: watchVerbs},
		}},
		&metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: watchVerbs},
		}},
	)
	w := watcherOnly(t, testConfig(), disc)

	found, failed, err := w.discover()
	require.NoError(t, err)
	assert.Empty(t, failed)
	assert.Equal(t, map[schema.GroupVersionResource]string{
		cmGVR:                             "ConfigMap",
		{Version: "v1", Resource: "pods"}: "Pod",
		{Group: "apps", Version: "v1", Resource: "deployments"}: "Deployment",
	}, found)
}

func TestSkippedResourceNotWatched(t *testing.T) {
	disc := newTestDiscovery(coreList(
		cmResource(),
		metav1.APIResource{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: watchVerbs},
	))
	cfg := testConfig()
	cfg.Skip = map[schema.GroupVersionResource]bool{secretsGVR: true}
	w := watcherOnly(t, cfg, disc)

	found, _, err := w.discover()
	require.NoError(t, err)
	assert.Equal(t, map[schema.GroupVersionResource]string{cmGVR: "ConfigMap"}, found)
}

func TestNewCRDGetsWatchAfterRediscovery(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource()), crdList())
	w, dyn, store, sink := newTestWatcher(t, testConfig(), disc)
	runWatcher(t, w)
	waitForWatch(t, dyn, crdGVR)

	// The CRD appears in discovery, then its object arrives on the CRD watch.
	disc.set(coreList(cmResource()), crdList(), widgetList())
	_, err := dyn.Resource(crdGVR).Create(context.Background(), widgetCRD("uid-crd"), metav1.CreateOptions{})
	require.NoError(t, err)

	waitForWatch(t, dyn, widgetGVR)
	_, err = dyn.Resource(widgetGVR).Namespace("default").Create(context.Background(), widget("w1", "uid-w1"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for _, r := range sink.objectRecords() {
			if r.Attributes["k8s.uid"] == "uid-w1" {
				return r.Attributes["k8s.kind"] == "Widget" && r.Attributes["k8s.op"] == "added"
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	_, ok := store.Get("uid-w1")
	require.True(t, ok)
}

func TestRemovedCRDTombstonesOnlyAfterCRDDelete(t *testing.T) {
	cfg := testConfig()
	cfg.DiscoveryInterval = 20 * time.Millisecond
	disc := newTestDiscovery(coreList(cmResource()), crdList(), widgetList())
	w, dyn, store, sink := newTestWatcher(t, cfg, disc, widgetCRD("uid-crd"), widget("w1", "uid-w1"))
	runWatcher(t, w)
	require.Eventually(t, func() bool { _, ok := store.Get("uid-w1"); return ok }, 5*time.Second, 10*time.Millisecond)
	waitForWatch(t, dyn, crdGVR)

	// The kind leaves discovery: its watch stops, and its objects stay in the store.
	disc.set(coreList(cmResource()), crdList())
	require.Eventually(t, func() bool { return !w.watching(widgetGVR) }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	_, ok := store.Get("uid-w1")
	require.True(t, ok)
	for _, r := range sink.objectRecords() {
		require.NotEqual(t, "deleted", r.Attributes["k8s.op"])
	}

	// The CRD's own delete tombstones the objects of its kind.
	require.NoError(t, dyn.Resource(crdGVR).Delete(context.Background(), "widgets.example.com", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { _, ok := store.Get("uid-w1"); return !ok }, 5*time.Second, 10*time.Millisecond)
	var deleted []string
	for _, r := range sink.objectRecords() {
		if r.Attributes["k8s.op"] == "deleted" {
			deleted = append(deleted, r.Attributes["k8s.uid"].(string))
		}
	}
	assert.ElementsMatch(t, []string{"uid-crd", "uid-w1"}, deleted)
}

func TestStartupTombstonesObjectsOfCRDDeletedWhileDown(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource()), crdList())
	w, _, store, sink := newTestWatcher(t, testConfig(), disc)
	store.Put("uid-crd", Entry{GVR: "apiextensions.k8s.io/v1/customresourcedefinitions", Kind: "CustomResourceDefinition", Name: "widgets.example.com", Hash: "0000000000000000", LastSent: 1})
	store.Put("uid-w1", Entry{GVR: "example.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "w1", Hash: "0000000000000000", LastSent: 1})
	store.Put("uid-other", Entry{GVR: "other.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "o", Hash: "0000000000000000", LastSent: 1})
	require.NoError(t, store.Flush(context.Background()))

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, stop())
	assert.Equal(t, []string{"deleted:uid-crd", "deleted:uid-w1"}, sortedOps(sink.objectRecords()))
	_, ok := store.Get("uid-other")
	assert.True(t, ok)
}

func TestHeartbeatRecordShape(t *testing.T) {
	w, _, store, _ := newTestWatcher(t, testConfig(), nil)
	start := time.UnixMilli(1700000000000)
	w.now = func() time.Time { return start }
	w.startTime = start

	ctx := context.Background()
	cms := w.newObjectStore(ctx, cmGVR)
	widgets := w.newObjectStore(ctx, widgetGVR)
	w.track(cmGVR, "ConfigMap")
	w.track(widgetGVR, "Widget")
	w.track(secretGVR, "Secret") // never listed: left out of the kinds

	hb := w.HeartbeatRecord(start)
	assert.Equal(t, int64(0), hb.Attributes["lastFullListTime"])

	w.now = func() time.Time { return start.Add(time.Second) }
	require.NoError(t, cms.Replace([]any{configMap("a", "uid-a", "v"), configMap("b", "uid-b", "v")}, "1"))
	require.NoError(t, widgets.Replace(nil, "1"))
	w.listFailed(secretGVR, nil)
	require.Equal(t, 2, len(storeUIDs(store)))

	now := start.Add(time.Minute)
	hb = w.HeartbeatRecord(now)
	assert.Equal(t, map[string]any{
		"type":               HeartbeatType,
		"timestamp":          now.Unix(),
		"k8s.cluster.name":   testCluster,
		"carrier.startTime":  start.UnixMilli(),
		"lastFullListTime":   start.Add(time.Second).UnixMilli(),
		"failedSends":        int64(0),
		"carrier.kindCount":  2,
		"carrier.kinds.p000": "~ConfigMap=2,Widget=0,",
		// The secrets list failed: the carrier is partial.
		"carrier.failedKindCount": 1,
		"carrier.failedKinds":     "/v1/secrets",
		"carrier.unsyncedObjects": 0,
		"carrier.accountFlag":     AccountFlagNotRequired,
		"carrier.resyncCursor":    "0000000000000000",
		// The store was never loaded, so its history has no start yet.
		"carrier.historyStart": int64(0),
		// testConfig leaves the resend off.
		"carrier.resendAfter": int64(0),
	}, hb.Attributes)
	assert.Equal(t, "carrier heartbeat: 2 kinds", hb.Summary)
	assert.Equal(t, Category, hb.Category)

	// A later successful list of the failed kind makes the carrier complete again.
	require.NoError(t, w.newObjectStore(ctx, secretGVR).Replace(nil, "2"))
	assert.Equal(t, 0, w.HeartbeatRecord(now).Attributes["carrier.failedKindCount"])
	// A relist that fails again counts the kind once more.
	w.listFailed(secretGVR, errors.New("forbidden"))
	assert.Equal(t, 1, w.HeartbeatRecord(now).Attributes["carrier.failedKindCount"])
	assert.Equal(t, []string{GVRString(secretGVR)}, w.FailedGVRs())
}

func TestHeartbeatPublishesResendAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resendAfter time.Duration
		want        int64
	}{
		{"default 24h", 24 * time.Hour, 86400},
		{"resend off", 0, 0},
		{"non-default 6h30m", 6*time.Hour + 30*time.Minute, 23400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ResendAfter = tc.resendAfter
			w, _, _, _ := newTestWatcher(t, cfg, nil)
			assert.Equal(t, tc.want, w.HeartbeatRecord(time.Unix(1700000000, 0)).Attributes["carrier.resendAfter"])
		})
	}
}

func TestCanceledSendIsNotCountedAsFailed(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	sink.setFail(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Error(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	w.sendTombstone(ctx, "uid-x", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Name: "x"}, time.Now())
	w.sendHeartbeat(ctx)
	assert.Equal(t, int64(0), w.HeartbeatRecord(time.Now()).Attributes["failedSends"])
	_, ok := store.Get("uid-a")
	assert.False(t, ok)

	// The same failures with a live context are counted.
	require.Error(t, w.HandleObject(context.Background(), cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	assert.Equal(t, int64(1), w.HeartbeatRecord(time.Now()).Attributes["failedSends"])
}

// TestRecreatedCRDWhileDownKeepsLiveObjects covers a CRD deleted and recreated under the
// same name while the carrier was down. Its old uid is missing from the CRD list, but the
// kind still exists, so only the kind's own list decides which objects are gone. Both
// orders of the two lists are checked.
func TestRecreatedCRDWhileDownKeepsLiveObjects(t *testing.T) {
	for _, crdFirst := range []bool{false, true} {
		name := "kind list first"
		if crdFirst {
			name = "crd list first"
		}
		t.Run(name, func(t *testing.T) {
			w, _, store, sink := newTestWatcher(t, testConfig(), nil)
			old := "0000000000000000"
			store.Put("uid-crd-A", Entry{GVR: "apiextensions.k8s.io/v1/customresourcedefinitions", Kind: "CustomResourceDefinition", Name: "widgets.example.com", Hash: old, LastSent: 1})
			store.Put("uid-old-gone", Entry{GVR: "example.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "gone", Hash: old, LastSent: 1})
			store.Put("uid-old-kept", Entry{GVR: "example.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "kept", Hash: old, LastSent: 1})

			ctx := context.Background()
			crds := w.newObjectStore(ctx, crdGVR)
			widgets := w.newObjectStore(ctx, widgetGVR)
			listCRDs := func() { require.NoError(t, crds.Replace([]any{widgetCRD("uid-crd-B")}, "1")) }
			listWidgets := func() {
				require.NoError(t, widgets.Replace([]any{widget("kept", "uid-old-kept"), widget("new", "uid-new")}, "1"))
			}
			if crdFirst {
				listCRDs()
				listWidgets()
			} else {
				listWidgets()
				listCRDs()
			}

			var deleted []string
			for _, r := range sink.records() {
				if r.Attributes["k8s.op"] == "deleted" {
					deleted = append(deleted, r.Attributes["k8s.uid"].(string))
				}
			}
			sort.Strings(deleted)
			assert.Equal(t, []string{"uid-crd-A", "uid-old-gone"}, deleted)
			for _, uid := range []string{"uid-crd-B", "uid-old-kept", "uid-new"} {
				_, ok := store.Get(uid)
				assert.True(t, ok, uid)
			}
			for _, uid := range []string{"uid-crd-A", "uid-old-gone"} {
				_, ok := store.Get(uid)
				assert.False(t, ok, uid)
			}
		})
	}
}

func TestHeartbeatKindsSplitIntoParts(t *testing.T) {
	w := watcherOnly(t, testConfig(), nil)
	ctx := context.Background()
	var want strings.Builder
	var kinds []string
	for i := 0; i < 400; i++ {
		kinds = append(kinds, "Kind"+strings.Repeat("x", 10)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	for _, k := range kinds {
		gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: "r" + k}
		w.track(gvr, k)
		require.NoError(t, w.newObjectStore(ctx, gvr).Replace(nil, "1"))
	}
	sorted := append([]string(nil), kinds...)
	sort.Strings(sorted)
	for _, k := range sorted {
		want.WriteString(k + "=0,")
	}
	hb := w.HeartbeatRecord(time.Now())
	var parts []string
	for i := 0; ; i++ {
		p, ok := hb.Attributes[fmt.Sprintf("carrier.kinds.p%03d", i)].(string)
		if !ok {
			break
		}
		parts = append(parts, p)
	}
	require.Greater(t, len(parts), 1)
	assert.Equal(t, want.String(), Join(parts))
	assert.Equal(t, 400, hb.Attributes["carrier.kindCount"])
}

func storeUIDs(s *Store) []string {
	entries := s.entriesWhere(func(Entry) bool { return true })
	out := make([]string, 0, len(entries))
	for uid := range entries {
		out = append(out, uid)
	}
	return out
}

func TestWatcherRunSendsHeartbeatAndFlushesOnShutdown(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	client := newClient()
	dyn := newDynamic(configMap("a", "uid-a", "v1"))
	store := NewStore(client, testNS)
	sink := &recordingSink{}
	w := NewWatcher(cfg, dyn, newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	stop := runWatcher(t, w)

	require.Eventually(t, func() bool {
		n := 0
		for _, r := range sink.records() {
			if r.Attributes["type"] == HeartbeatType {
				n++
			}
		}
		return n >= 2
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { _, ok := store.Get("uid-a"); return ok }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())

	// A new store over the same ConfigMaps sees the entry only if shutdown flushed it.
	reloaded := NewStore(client, testNS)
	require.NoError(t, reloaded.Load(context.Background()))
	_, ok := reloaded.Get("uid-a")
	assert.True(t, ok)
}

func TestConfigDefaults(t *testing.T) {
	w := NewWatcher(Config{ClusterName: "c"}, newDynamic(), newTestDiscovery(), NewStore(newClient(), testNS), &recordingSink{}, watcherKey)
	assert.Equal(t, 5*time.Minute, w.cfg.DiscoveryInterval)
	assert.Equal(t, 60*time.Second, w.cfg.HeartbeatInterval)
	assert.Equal(t, 30*time.Second, w.cfg.FlushInterval)
}

func TestSecretRedactedBeforeSend(t *testing.T) {
	const canary = "canary-7f3a9c-do-not-ship"
	encoded := base64.StdEncoding.EncodeToString([]byte(canary))
	lastApplied, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "s", "namespace": "default"},
		"data":     map[string]any{"password": encoded},
	})
	require.NoError(t, err)
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": "s", "namespace": "default", "uid": "uid-s",
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": string(lastApplied)},
		},
		"data":       map[string]any{"password": encoded},
		"stringData": map[string]any{"token": canary},
	}

	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	require.NoError(t, w.HandleObject(context.Background(), secretGVR, OpInitial, secret))

	recs := sink.records()
	require.NotEmpty(t, recs)
	for _, r := range recs {
		b, err := json.Marshal(r)
		require.NoError(t, err)
		assert.NotContains(t, string(b), canary)
		assert.NotContains(t, string(b), encoded)
	}
	// The stored hash is the hash of the redacted object that was sent.
	e, _ := store.Get("uid-s")
	assert.Equal(t, recs[0].Attributes["object.hash"], e.Hash)
	// The watch's own object is untouched.
	assert.Equal(t, encoded, secret["data"].(map[string]any)["password"])

	// A changed Secret value changes the hash through its hmac, so it is sent again.
	sink.reset()
	secret2 := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "s", "namespace": "default", "uid": "uid-s", "annotations": secret["metadata"].(map[string]any)["annotations"]},
		"data":       map[string]any{"password": base64.StdEncoding.EncodeToString([]byte("other"))},
		"stringData": map[string]any{"token": canary},
	}
	require.NoError(t, w.HandleObject(context.Background(), secretGVR, OpUpdated, secret2))
	assert.Len(t, sink.records(), 1)
}

// ownStoreObjects returns the carrier's own hash-store objects in namespace ns: a shard
// ConfigMap, the pointer ConfigMap and the HMAC Secret. uidPrefix keeps uids distinct
// between namespaces.
func ownStoreObjects(ns, uidPrefix string) map[schema.GroupVersionResource][]*unstructured.Unstructured {
	shard := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": shardName(1, 0), "namespace": ns, "uid": uidPrefix + "-shard",
			"labels": map[string]any{storeLabelKey: storeLabelValue, generationLabelKey: "1"},
		},
		"data": map[string]any{"uid-x": "entry"},
	}}
	pointer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": pointerName, "namespace": ns, "uid": uidPrefix + "-pointer"},
		"data":       map[string]any{pointerGeneration: "1", pointerShards: "1"},
	}}
	hmac := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": HMACSecretName, "namespace": ns, "uid": uidPrefix + "-hmac"},
		"data":       map[string]any{HMACSecretKey: base64.StdEncoding.EncodeToString([]byte("k"))},
	}}
	return map[schema.GroupVersionResource][]*unstructured.Unstructured{
		cmGVR:     {shard, pointer},
		secretGVR: {hmac},
	}
}

// changedData returns a copy of u with different data, so its hash differs.
func changedData(u *unstructured.Unstructured) *unstructured.Unstructured {
	c := u.DeepCopy()
	c.Object["data"] = map[string]any{"changed": "eWVz"}
	return c
}

func asItems(objs []*unstructured.Unstructured) []any {
	out := make([]any, 0, len(objs))
	for _, o := range objs {
		out = append(out, o)
	}
	return out
}

func TestOwnStoreObjectsSendNothingAndStoreNothing(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	for gvr, objs := range ownStoreObjects(testNS, "own") {
		s := w.newObjectStore(context.Background(), gvr)
		// First list, unchanged relist, and a relist with changed objects.
		require.NoError(t, s.Replace(asItems(objs), "1"))
		require.NoError(t, s.Replace(asItems(objs), "2"))
		var changedObjs []*unstructured.Unstructured
		for _, o := range objs {
			changedObjs = append(changedObjs, changedData(o))
		}
		require.NoError(t, s.Replace(asItems(changedObjs), "3"))
		for _, o := range objs {
			require.NoError(t, s.Add(o))
			require.NoError(t, s.Update(changedData(o)))
			require.NoError(t, s.Delete(o))
			key := o.GetNamespace() + "/" + o.GetName()
			require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: key, Obj: o}))
			require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: key}))
		}
		// A relist that no longer holds the objects sends no tombstone.
		require.NoError(t, s.Replace(nil, "4"))
		for _, o := range objs {
			_, ok := store.Get(string(o.GetUID()))
			assert.False(t, ok, "own object %s/%s stored", o.GetNamespace(), o.GetName())
		}
	}
	assert.Empty(t, sink.objectRecords())
}

func TestOwnStoreNamesInOtherNamespaceAreSent(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil)
	var want []string
	for gvr, objs := range ownStoreObjects("other", "other") {
		s := w.newObjectStore(context.Background(), gvr)
		for _, o := range objs {
			want = append(want, "initial:"+string(o.GetUID()))
		}
		require.NoError(t, s.Replace(asItems(objs), "1"))
		for _, o := range objs {
			_, ok := store.Get(string(o.GetUID()))
			assert.True(t, ok, "object %s/%s not stored", o.GetNamespace(), o.GetName())
		}
	}
	sort.Strings(want)
	assert.Equal(t, want, sortedOps(sink.objectRecords()))
}

// A hash store written by a carrier that still sent its own objects holds entries for
// them. The watcher drops those entries at start and sends no tombstone for them.
func TestStoredOwnStoreEntriesDroppedWithoutTombstone(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource(), secretResource()))
	own := ownStoreObjects(testNS, "own")
	// The pointer is still listed; the shard and the Secret are gone from the list.
	w, _, store, sink := newTestWatcher(t, testConfig(), disc, own[cmGVR][1], configMap("a", "uid-a", "v1"))
	for gvr, objs := range own {
		for _, o := range objs {
			store.Put(string(o.GetUID()), Entry{GVR: GVRString(gvr), Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Hash: "0000000000000000", LastSent: 1})
		}
	}
	require.NoError(t, store.Flush(context.Background()))

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.gvrs) == 2 && w.allResolvedLocked()
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())

	assert.Equal(t, []string{"initial:uid-a"}, ops(sink.objectRecords()))
	for _, objs := range own {
		for _, o := range objs {
			_, ok := store.Get(string(o.GetUID()))
			assert.False(t, ok, "own object %s/%s still stored", o.GetNamespace(), o.GetName())
		}
	}
}

// Run uses the store that main loaded. A store list that fails once Run is called does not
// stop the watcher.
func TestRunDoesNotReloadStore(t *testing.T) {
	client := newClient()
	store := NewStore(client, testNS)
	require.NoError(t, store.Load(context.Background()))
	client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	sink := &recordingSink{}
	w := NewWatcher(testConfig(), newDynamic(), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	require.Eventually(t, func() bool {
		for _, r := range sink.records() {
			if r.Attributes["type"] == HeartbeatType {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "Run sent no heartbeat")
	select {
	case err := <-done:
		t.Fatalf("Run returned before cancel: %v", err)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A Secret watch object without kind or apiVersion is still redacted: the watcher knows
// from the GVR that it is a Secret.
func TestKindlessSecretRedacted(t *testing.T) {
	const canary = "canary-kindless-5b1e-do-not-ship"
	encoded := base64.StdEncoding.EncodeToString([]byte(canary))
	for _, tracked := range []bool{false, true} {
		w, _, _, sink := newTestWatcher(t, testConfig(), nil)
		if tracked {
			w.track(secretGVR, "Secret")
		}
		obj := map[string]any{
			"metadata":   map[string]any{"name": "s", "namespace": "default", "uid": "uid-s"},
			"data":       map[string]any{"password": encoded},
			"stringData": map[string]any{"token": canary},
		}
		require.NoError(t, w.HandleObject(context.Background(), secretGVR, OpInitial, obj))

		recs := sink.objectRecords()
		require.Len(t, recs, 1, "tracked=%v", tracked)
		text := recs[0].Attributes["object.p000"].(string)
		assert.NotContains(t, text, canary, "tracked=%v", tracked)
		assert.NotContains(t, text, encoded, "tracked=%v", tracked)
		assert.Contains(t, text, "<redacted", "tracked=%v", tracked)
		assert.Contains(t, text, `"apiVersion":"v1"`, "tracked=%v", tracked)
		assert.Equal(t, "Secret", recs[0].Attributes["k8s.kind"], "tracked=%v", tracked)
		_, hasKind := obj["kind"]
		assert.False(t, hasKind, "the watch's own object is untouched")
	}
}

func unsyncedObjects(w *Watcher) any {
	return w.HeartbeatRecord(time.Now()).Attributes["carrier.unsyncedObjects"]
}

func TestFailedSendRetriedUntilSent(t *testing.T) {
	cm := configMap("a", "uid-a", "v1")
	w, _, store, sink := newTestWatcher(t, testConfig(), nil, cm)
	ctx := context.Background()
	assert.Equal(t, 0, unsyncedObjects(w))

	sink.setFail(true)
	require.Error(t, w.HandleObject(ctx, cmGVR, OpInitial, cm.Object))
	assert.Equal(t, 1, unsyncedObjects(w))

	// A retry that fails again keeps the object unsynced.
	w.retryUnsynced(ctx)
	assert.Equal(t, 1, unsyncedObjects(w))

	sink.setFail(false)
	sink.reset()
	w.retryUnsynced(ctx)
	assert.Equal(t, []string{"initial:uid-a"}, ops(sink.objectRecords()))
	assert.Equal(t, 0, unsyncedObjects(w))
	_, ok := store.Get("uid-a")
	assert.True(t, ok)

	// Nothing is left to retry.
	sink.reset()
	w.retryUnsynced(ctx)
	assert.Empty(t, sink.records())
}

func TestFailedSendOfGoneObjectRetriedAsTombstone(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		cm := configMap("a", "uid-a", "v1")
		w, dyn, store, sink := newTestWatcher(t, testConfig(), nil, cm)
		ctx := context.Background()
		sink.setFail(true)
		require.Error(t, w.HandleObject(ctx, cmGVR, OpUpdated, cm.Object))
		require.NoError(t, dyn.Resource(cmGVR).Namespace("default").Delete(ctx, "a", metav1.DeleteOptions{}))
		if recreated {
			_, err := dyn.Resource(cmGVR).Namespace("default").Create(ctx, configMap("a", "uid-b", "v1"), metav1.CreateOptions{})
			require.NoError(t, err)
		}

		sink.setFail(false)
		w.retryUnsynced(ctx)
		recs := sink.objectRecords()
		require.Len(t, recs, 1, "recreated=%v", recreated)
		assert.Equal(t, "deleted", recs[0].Attributes["k8s.op"])
		assert.Equal(t, "uid-a", recs[0].Attributes["k8s.uid"])
		assert.Equal(t, "a", recs[0].Attributes["k8s.name"])
		assert.Equal(t, 0, recs[0].Attributes["object.parts"])
		assert.Equal(t, 0, unsyncedObjects(w))
		_, ok := store.Get("uid-a")
		assert.False(t, ok)
	}
}

func TestFailedTombstoneRetriedAsTombstone(t *testing.T) {
	// The object is still served, so a retry that fetched it would send a record.
	cm := configMap("a", "uid-a", "v1")
	w, _, store, sink := newTestWatcher(t, testConfig(), nil, cm)
	ctx := context.Background()
	s := w.newObjectStore(ctx, cmGVR)
	require.NoError(t, s.Replace([]any{cm}, "1"))

	sink.setFail(true)
	require.NoError(t, s.Delete(cm))
	assert.Equal(t, 1, unsyncedObjects(w))
	_, ok := store.Get("uid-a")
	require.True(t, ok)

	sink.setFail(false)
	sink.reset()
	w.retryUnsynced(ctx)
	recs := sink.objectRecords()
	require.Len(t, recs, 1)
	assert.Equal(t, "deleted", recs[0].Attributes["k8s.op"])
	assert.Equal(t, 0, recs[0].Attributes["object.parts"])
	assert.Equal(t, 0, unsyncedObjects(w))
	_, ok = store.Get("uid-a")
	assert.False(t, ok)

	// A failed tombstone sent from the stored identity is retried the same way.
	require.NoError(t, s.Replace([]any{configMap("b", "uid-b", "v1")}, "2"))
	sink.setFail(true)
	require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: "default/b"}))
	assert.Equal(t, 1, unsyncedObjects(w))
	sink.setFail(false)
	sink.reset()
	w.retryUnsynced(ctx)
	assert.Equal(t, []string{"deleted:uid-b"}, ops(sink.objectRecords()))
	assert.Equal(t, 0, unsyncedObjects(w))
}

// An event that brings back the version NRDB already holds makes the object synced again.
func TestUnsyncedClearedWhenStoredVersionReturns(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	ctx := context.Background()
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	sink.setFail(true)
	require.Error(t, w.HandleObject(ctx, cmGVR, OpUpdated, configMap("a", "uid-a", "v2").Object))
	assert.Equal(t, 1, unsyncedObjects(w))
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpUpdated, configMap("a", "uid-a", "v1").Object))
	assert.Equal(t, 0, unsyncedObjects(w))
}

func TestRunRetriesFailedSendsOnFlushTick(t *testing.T) {
	cfg := testConfig()
	cfg.FlushInterval = 20 * time.Millisecond
	w, _, store, sink := newTestWatcher(t, cfg, nil, configMap("a", "uid-a", "v1"))
	sink.setFail(true)
	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return unsyncedObjects(w) == 1 }, 5*time.Second, 10*time.Millisecond)

	sink.setFail(false)
	require.Eventually(t, func() bool { return unsyncedObjects(w) == 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())
	assert.Equal(t, []string{"initial:uid-a"}, ops(sink.objectRecords()))
	_, ok := store.Get("uid-a")
	assert.True(t, ok)
}

// watchedConfigMaps registers s as the reflector store of configmaps, as startLocked does,
// so a retry orders itself with s's watch events.
func watchedConfigMaps(w *Watcher, s *objectStore) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gvrs[cmGVR] = &gvrState{kind: "ConfigMap", cancel: func() {}, store: s}
}

// raceRetryWithWatch fails a send of version v1 of uid-a, then runs a retry pass whose
// hook, called after the retry's GET, delivers watchEvent from another goroutine. The
// hook waits up to 200 ms for the event to finish: without serialization it finishes
// before the retry sends, with it the event waits for the retry.
func raceRetryWithWatch(t *testing.T, watchEvent func(s *objectStore) error) (*Watcher, *Store, *recordingSink) {
	t.Helper()
	v1 := configMap("a", "uid-a", "v1")
	w, _, store, sink := newTestWatcher(t, testConfig(), nil, v1)
	ctx := context.Background()
	s := w.newObjectStore(ctx, cmGVR)
	watchedConfigMaps(w, s)
	require.NoError(t, s.Replace([]any{configMap("a", "uid-a", "v0")}, "1"))
	sink.setFail(true)
	require.NoError(t, s.Update(v1))
	require.Equal(t, 1, unsyncedObjects(w))
	sink.setFail(false)

	done := make(chan error, 1)
	w.afterRetryGet = func(string) {
		go func() { done <- watchEvent(s) }()
		select {
		case err := <-done:
			done <- err
		case <-time.After(200 * time.Millisecond):
		}
	}
	w.retryUnsynced(ctx)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("watch event never finished")
	}
	return w, store, sink
}

// A watch event for version N+1 that arrives while a retry holds version N must stay the
// newest record and the stored hash.
func TestRetryDoesNotOverwriteNewerWatchEvent(t *testing.T) {
	w, store, sink := raceRetryWithWatch(t, func(s *objectStore) error {
		return s.Update(configMap("a", "uid-a", "v2"))
	})
	recs := sink.objectRecords()
	require.NotEmpty(t, recs)
	last := recs[len(recs)-1]
	assert.Contains(t, last.Attributes["object.p000"], `"k":"v2"`, "newest record is N+1")
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, last.Attributes["object.hash"], e.Hash, "stored hash is hash(N+1)")
	assert.Equal(t, 0, unsyncedObjects(w))
}

// A watch delete that arrives while a retry holds a live version must not be followed by
// that version: no ghost object in NRDB or in the store.
func TestRetryDoesNotResurrectDeletedObject(t *testing.T) {
	w, store, sink := raceRetryWithWatch(t, func(s *objectStore) error {
		return s.Delete(configMap("a", "uid-a", "v1"))
	})
	recs := sink.objectRecords()
	require.NotEmpty(t, recs)
	assert.Equal(t, "deleted", recs[len(recs)-1].Attributes["k8s.op"], "no record after the tombstone")
	_, ok := store.Get("uid-a")
	assert.False(t, ok)
	assert.Equal(t, 0, unsyncedObjects(w))
}

// One retry pass sends at most maxRetryPerPass objects, oldest failure first.
func TestRetryPassCapped(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	base := time.Unix(1700000000, 0)
	w.mu.Lock()
	for i := 0; i < 501; i++ {
		uid := fmt.Sprintf("uid-%03d", i)
		w.unsynced[uid] = unsyncedObject{
			entry: Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: uid},
			op:    OpDeleted,
			since: base.Add(time.Duration(i) * time.Second),
		}
	}
	w.mu.Unlock()

	w.retryUnsynced(context.Background())
	assert.Len(t, sink.objectRecords(), 500)
	assert.Equal(t, 1, unsyncedObjects(w))
	w.mu.Lock()
	_, newestLeft := w.unsynced["uid-500"]
	w.mu.Unlock()
	assert.True(t, newestLeft, "the newest failure waits for the next pass")
}

// blockingHeartbeatSink holds every heartbeat until release closes or the send's context
// ends. Object records pass straight through.
type blockingHeartbeatSink struct {
	release chan struct{}
	recordingSink
	heartbeats atomic.Int32
	deadlineOK atomic.Bool
}

func (s *blockingHeartbeatSink) Send(ctx context.Context, recs []Record) error {
	if len(recs) == 1 && recs[0].Attributes["type"] == HeartbeatType {
		s.heartbeats.Add(1)
		if d, ok := ctx.Deadline(); ok && time.Until(d) <= heartbeatSendTimeout {
			s.deadlineOK.Store(true)
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.recordingSink.Send(ctx, recs)
}

// A heartbeat stuck on the Log API holds up neither the flush ticks nor a second heartbeat.
func TestHeartbeatDoesNotBlockRunLoop(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 10 * time.Millisecond
	cfg.FlushInterval = 10 * time.Millisecond
	client := newClient()
	store := NewStore(client, testNS)
	sink := &blockingHeartbeatSink{release: make(chan struct{})}
	w := NewWatcher(cfg, newDynamic(configMap("a", "uid-a", "v1")), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	stop := runWatcher(t, w)

	// The flush tick wrote uid-a while the first heartbeat is still sending.
	require.Eventually(t, func() bool {
		reloaded := NewStore(client, testNS)
		if err := reloaded.Load(context.Background()); err != nil {
			return false
		}
		_, ok := reloaded.Get("uid-a")
		return ok
	}, 5*time.Second, 10*time.Millisecond, "flush never ran while the heartbeat was blocked")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), sink.heartbeats.Load(), "at most one heartbeat in flight")
	assert.True(t, sink.deadlineOK.Load(), "heartbeat send has a deadline within heartbeatSendTimeout")

	close(sink.release)
	require.Eventually(t, func() bool { return sink.heartbeats.Load() >= 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())
}

// markFailing tracks every gvr and fails its list, as a list the apiserver refused does.
// The nil error keeps listFailed from logging each one.
func markFailing(w *Watcher, gvrs ...schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		w.track(gvr, "")
		w.listFailed(gvr, nil)
	}
}

func TestHeartbeatNewAttributes(t *testing.T) {
	cfg := testConfig()
	cfg.AccountFlag = func() string { return "on" }
	w, _, store, _ := newTestWatcher(t, cfg, nil)
	require.NoError(t, store.Load(context.Background()))
	require.False(t, store.HistoryStart().IsZero())
	markFailing(w, widgetGVR, configMapsGVR)

	h := w.HeartbeatRecord(time.Unix(1700000000, 0))
	assert.Equal(t, "K8sObjectCarrier", h.Attributes["type"])
	assert.Equal(t, "kubernetes.objectState", h.Category)
	assert.Equal(t, int64(1700000000), h.Attributes["timestamp"])
	assert.Equal(t, "on", h.Attributes["carrier.accountFlag"])
	assert.Equal(t, "/v1/configmaps,example.com/v1/widgets", h.Attributes["carrier.failedKinds"])
	assert.Equal(t, 2, h.Attributes["carrier.failedKindCount"])
	assert.Equal(t, "0000000000000000", h.Attributes["carrier.resyncCursor"])
	assert.Equal(t, store.HistoryStart().UnixMilli(), h.Attributes["carrier.historyStart"])
	for _, k := range []string{"carrier.startTime", "lastFullListTime", "failedSends", "carrier.kindCount", "carrier.unsyncedObjects", "k8s.cluster.name"} {
		assert.Contains(t, h.Attributes, k)
	}

	w.mu.Lock()
	w.resyncCursor = 0xabc
	w.mu.Unlock()
	assert.Equal(t, "0000000000000abc", w.HeartbeatRecord(time.Now()).Attributes["carrier.resyncCursor"])
}

func TestHeartbeatAccountFlagDefaultsToNotRequired(t *testing.T) {
	w := watcherOnly(t, testConfig(), nil)
	h := w.HeartbeatRecord(time.Now())
	assert.Equal(t, "not_required", h.Attributes["carrier.accountFlag"])
	// No kind failed its list.
	assert.Equal(t, "", h.Attributes["carrier.failedKinds"])
}

func TestHeartbeatFailedKindsCapped(t *testing.T) {
	w := watcherOnly(t, testConfig(), nil)
	var gvrs []schema.GroupVersionResource
	var want []string
	for i := 0; i < 400; i++ {
		gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: fmt.Sprintf("r%024d", i)}
		require.Len(t, GVRString(gvr), 30)
		gvrs = append(gvrs, gvr)
		want = append(want, GVRString(gvr))
	}
	markFailing(w, gvrs...)
	sort.Strings(want)

	h := w.HeartbeatRecord(time.Now())
	got, ok := h.Attributes["carrier.failedKinds"].(string)
	require.True(t, ok)
	assert.LessOrEqual(t, len([]rune(got)), PartChars)
	assert.LessOrEqual(t, len(got), MaxAttrBytes)
	assert.False(t, strings.HasPrefix(got, ","))
	assert.False(t, strings.HasSuffix(got, ","))
	// The list holds whole GVRs, in order, and dropped the rest.
	items := strings.Split(got, ",")
	require.Less(t, len(items), len(want))
	assert.Equal(t, want[:len(items)], items)
	assert.Equal(t, 400, h.Attributes["carrier.failedKindCount"])
}

var (
	applicationsGVR  = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	scaledObjectsGVR = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
)

// withUID returns obj with metadata.uid set.
func withUID(obj map[string]any, uid string) map[string]any {
	obj["metadata"].(map[string]any)["uid"] = uid
	return obj
}

func TestNoiseOnlyChangeSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		obj    func() map[string]any
		change func(map[string]any)
		name   string
		gvr    schema.GroupVersionResource
	}{
		{name: "argo application refresh", gvr: applicationsGVR, obj: argoApp, change: func(o map[string]any) {
			o["status"].(map[string]any)["reconciledAt"] = "2026-11-17T10:03:00Z"
			o["metadata"].(map[string]any)["generation"] = int64(8)
		}},
		{name: "keda recommendation annotation", gvr: scaledObjectsGVR, obj: scaledObject, change: func(o map[string]any) {
			o["metadata"].(map[string]any)["annotations"].(map[string]any)[kedaRecommendationAnnotation] = `[{"t":"2026-11-17T10:05:00Z","v":55}]`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _, sink := newTestWatcher(t, testConfig(), nil)
			ctx := context.Background()
			require.NoError(t, w.HandleObject(ctx, tc.gvr, OpAdded, withUID(tc.obj(), "uid-1")))
			changed := withUID(tc.obj(), "uid-1")
			tc.change(changed)
			require.NoError(t, w.HandleObject(ctx, tc.gvr, OpUpdated, changed))
			assert.Equal(t, []string{"added:uid-1"}, ops(sink.objectRecords()))
		})
	}
}

func TestRealChangeOfNoisyKindSends(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	ctx := context.Background()
	require.NoError(t, w.HandleObject(ctx, applicationsGVR, OpAdded, withUID(argoApp(), "uid-app")))
	changed := withUID(argoApp(), "uid-app")
	changed["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "v2"
	require.NoError(t, w.HandleObject(ctx, applicationsGVR, OpUpdated, changed))
	assert.Equal(t, []string{"added:uid-app", "updated:uid-app"}, ops(sink.objectRecords()))
}

func TestWatchDeleteTombstoneTimestamp(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	listed := time.Unix(1700000000, 0)
	deleted := listed.Add(time.Hour)
	w.now = func() time.Time { return listed }
	s := w.newObjectStore(context.Background(), cmGVR)
	require.NoError(t, s.Replace([]any{configMap("a", "uid-a", "v1"), configMap("b", "uid-b", "v1")}, "1"))
	sink.reset()

	w.now = func() time.Time { return deleted }
	require.NoError(t, s.Delete(configMap("a", "uid-a", "v1")))
	require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: "default/b"}))
	recs := sink.objectRecords()
	require.Equal(t, []string{"deleted:uid-a", "deleted:uid-b"}, ops(recs))
	for _, r := range recs {
		assert.Equal(t, deleted.Unix(), r.Attributes["timestamp"], r.Attributes["k8s.uid"])
	}
}

func TestRetriedTombstoneKeepsObservedTime(t *testing.T) {
	for _, lostState := range []bool{false, true} {
		t.Run(fmt.Sprintf("lostState=%v", lostState), func(t *testing.T) {
			cm := configMap("a", "uid-a", "v1")
			// The object is still served, so a retry that fetched it would send a record.
			w, _, _, sink := newTestWatcher(t, testConfig(), nil, cm)
			ctx := context.Background()
			s := w.newObjectStore(ctx, cmGVR)
			require.NoError(t, s.Replace([]any{cm}, "1"))

			observed := time.Unix(1700000000, 0)
			w.now = func() time.Time { return observed }
			sink.setFail(true)
			if lostState {
				require.NoError(t, s.Delete(cache.DeletedFinalStateUnknown{Key: "default/a"}))
			} else {
				require.NoError(t, s.Delete(cm))
			}
			require.Equal(t, 1, unsyncedObjects(w))

			// A retry pass that fails again keeps the observed time too.
			w.now = func() time.Time { return observed.Add(5 * time.Minute) }
			w.retryUnsynced(ctx)
			require.Equal(t, 1, unsyncedObjects(w))

			sink.setFail(false)
			sink.reset()
			w.now = func() time.Time { return observed.Add(10 * time.Minute) }
			w.retryUnsynced(ctx)
			recs := sink.objectRecords()
			require.Equal(t, []string{"deleted:uid-a"}, ops(recs))
			assert.Equal(t, observed.Unix(), recs[0].Attributes["timestamp"])
			assert.Equal(t, 0, unsyncedObjects(w))
		})
	}
}

// A retry that finds the object gone tombstones it at the time of the retry, when the
// carrier observed the deletion.
func TestRetryOfGoneObjectTombstonesAtRetryTime(t *testing.T) {
	cm := configMap("a", "uid-a", "v1")
	w, dyn, _, sink := newTestWatcher(t, testConfig(), nil, cm)
	ctx := context.Background()
	w.now = func() time.Time { return time.Unix(1700000000, 0) }
	sink.setFail(true)
	require.Error(t, w.HandleObject(ctx, cmGVR, OpUpdated, cm.Object))
	require.NoError(t, dyn.Resource(cmGVR).Namespace("default").Delete(ctx, "a", metav1.DeleteOptions{}))

	retried := time.Unix(1700000600, 0)
	w.now = func() time.Time { return retried }
	sink.setFail(false)
	w.retryUnsynced(ctx)
	recs := sink.objectRecords()
	require.Equal(t, []string{"deleted:uid-a"}, ops(recs))
	assert.Equal(t, retried.Unix(), recs[0].Attributes["timestamp"])
}

func TestStartupTombstoneUsesReconciliationTime(t *testing.T) {
	w, _, store, sink := newTestWatcher(t, testConfig(), nil, configMap("a", "uid-a", "v1"))
	// The previous carrier sent uid-gone long ago.
	store.Put("uid-gone", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "gone", Hash: "0000000000000000", LastSent: 1600000000})
	require.NoError(t, store.Flush(context.Background()))
	store.Delete("uid-gone")

	reconciled := time.Unix(1700000000, 0)
	w.now = func() time.Time { return reconciled }
	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())

	var found bool
	for _, r := range sink.objectRecords() {
		if r.Attributes["k8s.uid"] == "uid-gone" {
			found = true
			assert.Equal(t, "deleted", r.Attributes["k8s.op"])
			assert.Equal(t, reconciled.Unix(), r.Attributes["timestamp"])
		}
	}
	assert.True(t, found, "no tombstone for uid-gone")
}

func TestPrepareObjectMatchesHandleObject(t *testing.T) {
	kindlessSecret := map[string]any{
		"metadata":   map[string]any{"name": "s", "namespace": "default", "uid": "uid-s"},
		"data":       map[string]any{"password": base64.StdEncoding.EncodeToString([]byte("hunter2"))},
		"stringData": map[string]any{"token": "hunter2"},
	}
	for _, tc := range []struct {
		name string
		gvr  schema.GroupVersionResource
		obj  map[string]any
		uid  string
	}{
		{"configmap", cmGVR, configMap("a", "uid-a", "v1").Object, "uid-a"},
		{"kindless secret", secretGVR, kindlessSecret, "uid-s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, store, _ := newTestWatcher(t, testConfig(), nil)
			red, hash := PrepareObject(tc.gvr, w.kindOf(tc.gvr), tc.obj, watcherKey)
			require.NoError(t, w.HandleObject(context.Background(), tc.gvr, OpInitial, tc.obj))
			e, ok := store.Get(tc.uid)
			require.True(t, ok)
			assert.Equal(t, e.Hash, hash)
			assert.Equal(t, e.Kind, red["kind"])
			b, err := json.Marshal(red)
			require.NoError(t, err)
			assert.NotContains(t, string(b), "hunter2")
			assert.NotContains(t, string(b), base64.StdEncoding.EncodeToString([]byte("hunter2")))
		})
	}
	_, hasKind := kindlessSecret["kind"]
	assert.False(t, hasKind, "PrepareObject leaves obj untouched")
}

func TestIsOwnStoreObjectNeedsCarrierNamespace(t *testing.T) {
	own := ownStoreObjects(testNS, "own")
	pointerObj := own[cmGVR][1].Object
	assert.True(t, IsOwnStoreObject(testNS, cmGVR, pointerObj))
	assert.False(t, IsOwnStoreObject("other", cmGVR, pointerObj))
	assert.False(t, IsOwnStoreObject("", cmGVR, pointerObj))
	assert.False(t, IsOwnStoreObject(testNS, widgetGVR, pointerObj))
}

func TestDiscoverMatchesWatcherDiscover(t *testing.T) {
	disc := newTestDiscovery(coreList(cmResource(), secretResource()), widgetList())
	skip := map[schema.GroupVersionResource]bool{secretsGVR: true}
	found, failed, err := Discover(disc, skip)
	require.NoError(t, err)
	assert.Empty(t, failed)
	assert.Equal(t, map[schema.GroupVersionResource]string{cmGVR: "ConfigMap", widgetGVR: "Widget"}, found)
}

func TestHandleForceSendsUnchangedObject(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	ctx := context.Background()
	cm := configMap("a", "uid-a", "v1")
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpInitial, cm.Object))
	require.NoError(t, w.handle(ctx, cmGVR, OpResync, cm.Object, false))
	require.NoError(t, w.handle(ctx, cmGVR, OpResync, cm.Object, true))
	assert.Equal(t, []string{"initial:uid-a", "resync:uid-a"}, ops(sink.objectRecords()))
}

func TestIsListed(t *testing.T) {
	w := watcherOnly(t, testConfig(), nil)
	// A kind tracked without a reflector is never listed.
	w.track(widgetGVR, "Widget")
	require.NoError(t, w.newObjectStore(context.Background(), widgetGVR).Replace(nil, "1"))
	assert.False(t, w.isListed(widgetGVR))

	s := w.newObjectStore(context.Background(), cmGVR)
	watchedConfigMaps(w, s)
	assert.False(t, w.isListed(cmGVR), "before the first list")
	require.NoError(t, s.Replace(nil, "1"))
	assert.True(t, w.isListed(cmGVR))
	// A later failed list leaves the kind listed: its first list succeeded.
	w.listFailed(cmGVR, errors.New("forbidden"))
	assert.True(t, w.isListed(cmGVR))
	assert.False(t, w.isListed(secretGVR), "unwatched")
}

// Every failed send that was not cut short by cancellation counts in send_failures_total.
func TestFailedSendsCountInMetric(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	sink.setFail(true)
	before := counterValue(t, metricSendFailures)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, w.HandleObject(canceledCtx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	assert.Equal(t, before, counterValue(t, metricSendFailures))

	ctx := context.Background()
	require.Error(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))
	w.sendTombstone(ctx, "uid-x", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Name: "x"}, time.Now())
	w.sendHeartbeat(ctx)
	assert.Equal(t, before+3, counterValue(t, metricSendFailures))
}

func TestConfigResendDefaults(t *testing.T) {
	assert.Equal(t, time.Minute, NewWatcher(Config{ClusterName: "c"}, newDynamic(), newTestDiscovery(), NewStore(newClient(), testNS), &recordingSink{}, watcherKey).cfg.ResendInterval)
	w := NewWatcher(Config{ClusterName: "c", ResendInterval: time.Second}, newDynamic(), newTestDiscovery(), NewStore(newClient(), testNS), &recordingSink{}, watcherKey)
	assert.Equal(t, time.Second, w.cfg.ResendInterval)
	assert.Zero(t, w.cfg.ResendAfter, "ResendAfter takes no default")
}
