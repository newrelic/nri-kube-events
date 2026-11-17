package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

const testCarrierNS = "newrelic-objects"

var (
	cmGVR          = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	widgetGVR      = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	coreEventsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	eventsGroupGVR = schema.GroupVersionResource{Group: "events.k8s.io", Version: "v1", Resource: "events"}
	listVerbs      = metav1.Verbs{"get", "list", "watch"}
)

// fakeCluster serves configmaps, secrets and events in the core group, events.k8s.io events
// and example.com widgets, holding objs.
func fakeCluster(t *testing.T, objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *discoveryfake.FakeDiscovery) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cmGVR:          "ConfigMapList",
		secretsGVR:     "SecretList",
		widgetGVR:      "WidgetList",
		coreEventsGVR:  "EventList",
		eventsGroupGVR: "EventList",
	}, objs...)
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{Resources: []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: listVerbs},
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: listVerbs},
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: listVerbs},
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: listVerbs},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: listVerbs},
		}},
	}}}
	return dyn, disc
}

func configMap(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       namespace,
			"uid":             "uid-cm-" + namespace + "-" + name,
			"resourceVersion": "7",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
		"data": map[string]any{"k": "v"},
	}}
}

func widget(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "uid": "uid-widget-" + namespace + "-" + name},
		"spec":       map[string]any{"size": int64(3)},
	}}
}

func event(gvr schema.GroupVersionResource, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "uid": "uid-event-" + gvr.Group + "-" + name},
	}}
}

func secret(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "uid": "uid-secret-" + name},
		"data":       map[string]any{"key": "dmFsdWU="},
	}}
}

// ownShard is a hash-store shard ConfigMap of the carrier installed in namespace.
func ownShard(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "nri-kube-events-hashes-g1-0",
			"namespace": namespace,
			"uid":       "uid-own-shard",
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "nri-kube-events-objects"},
		},
		"data": map[string]any{"x": "y"},
	}}
}

// recordingSink accepts every record.
type recordingSink struct {
	recs []objects.Record
	mu   sync.Mutex
}

func (s *recordingSink) Send(_ context.Context, recs []objects.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, recs...)
	return nil
}

// carrierOver runs the carrier's watcher over dyn and disc until its store holds want
// entries, and returns its store.
func carrierOver(t *testing.T, dyn *dynamicfake.FakeDynamicClient, disc *discoveryfake.FakeDiscovery, want int) *objects.Store {
	t.Helper()
	store := objects.NewStore(fake.NewClientset(), testCarrierNS)
	require.NoError(t, store.Load(t.Context()))
	w := objects.NewWatcher(objects.Config{
		ClusterName:       "c1",
		Namespace:         testCarrierNS,
		DiscoveryInterval: time.Hour,
		HeartbeatInterval: time.Hour,
		FlushInterval:     time.Hour,
	}, dyn, disc, store, &recordingSink{}, []byte("0123456789abcdef0123456789abcdef"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return store.Len() == want }, 5*time.Second, 10*time.Millisecond)
	return store
}

func names(rows []row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

func TestSnapshotHashesMatchCarrier(t *testing.T) {
	dyn, disc := fakeCluster(t, configMap("ns2", "a"), widget("ns1", "w"), ownShard(testCarrierNS))
	rows, failed, err := snapshot(t.Context(), dyn, disc, testCarrierNS)
	require.NoError(t, err)
	assert.Empty(t, failed)
	require.Len(t, rows, 2)

	store := carrierOver(t, dyn, disc, len(rows))
	for _, r := range rows {
		e, ok := store.Get(r.uid)
		require.True(t, ok, r.name)
		assert.Equal(t, e.Hash, r.hash, r.name)
		assert.Equal(t, e.Kind, r.kind, r.name)
		assert.Equal(t, e.Namespace, r.namespace, r.name)
	}
	// Own store objects are left out.
	assert.NotContains(t, names(rows), "nri-kube-events-hashes-g1-0")
}

func TestSnapshotRowsCarryKindAndGroup(t *testing.T) {
	dyn, disc := fakeCluster(t, configMap("ns1", "a"), widget("ns2", "w"))
	rows, _, err := snapshot(t.Context(), dyn, disc, testCarrierNS)
	require.NoError(t, err)
	byName := map[string]row{}
	for _, r := range rows {
		byName[r.name] = r
	}
	assert.Equal(t, row{uid: "uid-cm-ns1-a", kind: "ConfigMap", group: "", namespace: "ns1", name: "a", hash: byName["a"].hash}, byName["a"])
	assert.Equal(t, row{uid: "uid-widget-ns2-w", kind: "Widget", group: "example.com", namespace: "ns2", name: "w", hash: byName["w"].hash}, byName["w"])
	assert.NotEmpty(t, byName["a"].hash)
}

func TestSnapshotSkipsSecretsAndEvents(t *testing.T) {
	dyn, disc := fakeCluster(t,
		configMap("ns1", "a"), secret("ns1", "s1"),
		event(coreEventsGVR, "ns1", "e1"), event(eventsGroupGVR, "ns1", "e2"))
	rows, failed, err := snapshot(t.Context(), dyn, disc, testCarrierNS)
	require.NoError(t, err)
	assert.Empty(t, failed)
	assert.Equal(t, []string{"a"}, names(rows))
	actions := dyn.Actions()
	listed := make([]schema.GroupVersionResource, 0, len(actions))
	for _, a := range actions {
		listed = append(listed, a.GetResource())
	}
	assert.NotContains(t, listed, secretsGVR)
	assert.NotContains(t, listed, coreEventsGVR)
	assert.NotContains(t, listed, eventsGroupGVR)
	assert.Contains(t, listed, cmGVR)
}

func TestSnapshotReportsFailedLists(t *testing.T) {
	dyn, disc := fakeCluster(t, configMap("ns1", "a"), widget("ns1", "w"))
	dyn.PrependReactor("list", "widgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	rows, failed, err := snapshot(t.Context(), dyn, disc, testCarrierNS)
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com/v1/widgets"}, failed)
	assert.Equal(t, []string{"a"}, names(rows))
}

// pagingDynamic serves configmaps in two pages and records each list's options. The
// dynamic fake drops Limit and Continue from the options its reactors see.
type pagingDynamic struct {
	dynamic.Interface
	opts []metav1.ListOptions
}

func (p *pagingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	res := p.Interface.Resource(gvr)
	if gvr != cmGVR {
		return res
	}
	return &pagingResource{NamespaceableResourceInterface: res, p: p}
}

type pagingResource struct {
	dynamic.NamespaceableResourceInterface
	p *pagingDynamic
}

func (r *pagingResource) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	r.p.opts = append(r.p.opts, opts)
	list := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMapList"}}
	if opts.Continue == "" {
		list.SetContinue("page-2")
		list.Items = []unstructured.Unstructured{*configMap("ns1", "a")}
	} else {
		list.Items = []unstructured.Unstructured{*configMap("ns1", "b")}
	}
	return list, nil
}

func TestSnapshotPagesLists(t *testing.T) {
	fakeDyn, disc := fakeCluster(t)
	dyn := &pagingDynamic{Interface: fakeDyn}
	rows, failed, err := snapshot(t.Context(), dyn, disc, testCarrierNS)
	require.NoError(t, err)
	assert.Empty(t, failed)
	assert.ElementsMatch(t, []string{"a", "b"}, names(rows))
	require.Len(t, dyn.opts, 2)
	assert.Equal(t, int64(listLimit), dyn.opts[0].Limit)
	assert.Empty(t, dyn.opts[0].Continue)
	assert.Equal(t, int64(listLimit), dyn.opts[1].Limit)
	assert.Equal(t, "page-2", dyn.opts[1].Continue)
}

func TestWriteRowsSortedWithHeader(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRows(&buf, []row{
		{uid: "u3", kind: "Widget", group: "example.com", namespace: "ns1", name: "w", hash: "h3"},
		{uid: "u2", kind: "ConfigMap", group: "", namespace: "ns2", name: "a", hash: "h2"},
		{uid: "u1", kind: "ConfigMap", group: "", namespace: "ns1", name: "b", hash: "h1"},
		{uid: "u4", kind: "Widget", group: "", namespace: "ns1", name: "w", hash: "h4"},
	}))
	assert.Equal(t, "uid\tkind\tgroup\tnamespace\tname\tobject.hash\n"+
		"u1\tConfigMap\t\tns1\tb\th1\n"+
		"u2\tConfigMap\t\tns2\ta\th2\n"+
		"u4\tWidget\t\tns1\tw\th4\n"+
		"u3\tWidget\texample.com\tns1\tw\th3\n", buf.String())
}
