package objects

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	storeNS = "newrelic"
	// apiserverLimit is the ConfigMap size the apiserver accepts.
	apiserverLimit = 1 << 20
)

// newClient returns a fake clientset that, like the apiserver, rejects a ConfigMap whose
// data passes 1 MiB.
func newClient(objs ...runtime.Object) *fake.Clientset {
	client := fake.NewClientset(objs...)
	reject := func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := actionConfigMap(a)
		if cm != nil && shardBytes(cm.Data) > apiserverLimit {
			return true, nil, apierrors.NewRequestEntityTooLargeError(
				fmt.Sprintf("configmap %s is %d bytes", cm.Name, shardBytes(cm.Data)))
		}
		return false, nil, nil
	}
	client.PrependReactor("create", "configmaps", reject)
	client.PrependReactor("update", "configmaps", reject)
	return client
}

func testEntry(i int) Entry {
	return Entry{
		GVR:       "apps/v1/deployments",
		Kind:      "Deployment",
		Namespace: "ns",
		Name:      fmt.Sprintf("d-%d", i),
		Hash:      fmt.Sprintf("%016x", i),
		LastSent:  1700000000000 + int64(i),
	}
}

// actionConfigMap returns the ConfigMap a create or update action carries, or nil.
func actionConfigMap(a k8stesting.Action) *corev1.ConfigMap {
	o, ok := a.(interface{ GetObject() runtime.Object })
	if !ok {
		return nil
	}
	cm, _ := o.GetObject().(*corev1.ConfigMap)
	return cm
}

func testUID(i int) string { return fmt.Sprintf("uid-%06d", i) }

// realUID and realEntry give a 226-byte stored entry: a 36-character uid, a CRD GVR and a
// 40-character name. realUID is a random version-4 UUID, as the apiserver assigns, seeded
// by i so runs repeat.
func realUID(i int) string {
	r := rand.New(rand.NewPCG(uint64(i), 0x9e3779b97f4a7c15)) //nolint:gosec // G404: seeded so test uids repeat
	var b [16]byte
	for k := range b {
		b[k] = byte(r.Uint32()) //nolint:gosec // G115: keeps the low 8 bits on purpose
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func realEntry(i int) Entry {
	ns := "observability-platform-production-"
	return Entry{
		GVR:       "kafka.strimzi.io/v1beta2/kafkamirrormaker2s",
		Kind:      "KafkaMirrorMaker2",
		Namespace: ns + strings.Repeat("x", 56-len(ns)),
		Name:      fmt.Sprintf("mirror-payments-api-gateway-%012d", i),
		Hash:      fmt.Sprintf("%016x", i),
		LastSent:  1700000000000 + int64(i),
	}
}

// shardConfigMaps returns the shard ConfigMaps of every generation, not the pointer.
func shardConfigMaps(t *testing.T, client *fake.Clientset) []corev1.ConfigMap {
	t.Helper()
	l, err := client.CoreV1().ConfigMaps(storeNS).List(context.Background(),
		metav1.ListOptions{LabelSelector: storeLabelKey + "=" + storeLabelValue})
	require.NoError(t, err)
	var out []corev1.ConfigMap
	for _, cm := range l.Items {
		if _, _, ok := parseShardName(cm.Name); ok {
			out = append(out, cm)
		}
	}
	return out
}

// pointer returns the generation and shard count the pointer ConfigMap names.
func pointer(t *testing.T, client *fake.Clientset) (gen, shards int) {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(storeNS).Get(context.Background(), pointerName, metav1.GetOptions{})
	require.NoError(t, err)
	gen, shards, ok := parsePointer(cm)
	require.True(t, ok)
	return gen, shards
}

// generations returns the generations that have shard ConfigMaps.
func generations(t *testing.T, client *fake.Clientset) map[int]int {
	t.Helper()
	out := map[int]int{}
	for _, cm := range shardConfigMaps(t, client) {
		g, _, _ := parseShardName(cm.Name)
		out[g]++
	}
	return out
}

func countActions(client *fake.Clientset, verb string) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "configmaps" {
			n++
		}
	}
	return n
}

// failWrites makes creates and updates of ConfigMap name fail while *on is true.
func failWrites(client *fake.Clientset, name string, on *bool) {
	f := func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := actionConfigMap(a)
		if *on && cm != nil && cm.Name == name {
			return true, nil, fmt.Errorf("injected write failure")
		}
		return false, nil, nil
	}
	client.PrependReactor("create", "configmaps", f)
	client.PrependReactor("update", "configmaps", f)
}

func requireAll(t *testing.T, client *fake.Clientset, n int, uid func(int) string, entry func(int) Entry) {
	t.Helper()
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(context.Background()))
	for i := 0; i < n; i++ {
		got, ok := r.Get(uid(i))
		require.True(t, ok, uid(i))
		require.Equal(t, entry(i), got)
	}
}

// requireSettled checks that a fresh Load of the stored state needs no writes.
func requireSettled(t *testing.T, client *fake.Clientset) {
	t.Helper()
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(context.Background()))
	client.ClearActions()
	require.NoError(t, r.Flush(context.Background()))
	assert.Empty(t, client.Actions())
}

func TestRealEntryIs226Bytes(t *testing.T) {
	assert.Equal(t, 226, len(realUID(1))+len(encodeEntry(realEntry(1))))
}

func TestPutFlushLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	require.NoError(t, s.Flush(ctx))
	assert.Empty(t, client.Actions()[1:], "an empty fresh store writes nothing")

	cluster := Entry{GVR: "/v1/namespaces", Kind: "Namespace", Name: "kube-system", Hash: "abcdef0123456789", LastSent: 42}
	s.Put("uid-a", testEntry(1))
	s.Put("uid-b", cluster)
	s.Put("uid-c", testEntry(3))
	s.Delete("uid-c")
	require.NoError(t, s.Flush(ctx))

	cms := shardConfigMaps(t, client)
	require.Len(t, cms, 1)
	assert.Equal(t, "nri-kube-events-hashes-g1-0", cms[0].Name)
	assert.Equal(t, "1", cms[0].Labels[generationLabelKey])
	assert.Equal(t, "apps/v1/deployments|Deployment|ns|d-1|0000000000000001|1700000000001", cms[0].Data["uid-a"])
	gen, shards := pointer(t, client)
	assert.Equal(t, 1, gen)
	assert.Equal(t, 1, shards)

	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	got, ok := r.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, testEntry(1), got)
	got, ok = r.Get("uid-b")
	require.True(t, ok)
	assert.Equal(t, cluster, got)
	_, ok = r.Get("uid-c")
	assert.False(t, ok)
	requireSettled(t, client)
}

func TestFlushWritesOnlyChangedShards(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	require.Greater(t, len(shardConfigMaps(t, client)), 2)

	// A fresh process loads the same layout and changes one entry.
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	client.ClearActions()

	require.NoError(t, r.Flush(ctx))
	assert.Empty(t, client.Actions(), "flush with nothing changed makes no API calls")

	e := testEntry(7)
	r.Put(testUID(7), e) // same value: not a change
	require.NoError(t, r.Flush(ctx))
	assert.Empty(t, client.Actions())

	e.Hash = "ffffffffffffffff"
	r.Put(testUID(7), e)
	require.NoError(t, r.Flush(ctx))
	assert.Equal(t, 1, countActions(client, "update"))
	assert.Equal(t, 0, countActions(client, "create"))

	client.ClearActions()
	r.Delete(testUID(8))
	r.Delete("not-stored")
	require.NoError(t, r.Flush(ctx))
	assert.Equal(t, 1, countActions(client, "update"))
	gen, _ := pointer(t, client)
	assert.Equal(t, 1, gen, "in-place writes keep the generation")
}

// Each shard write adds the shard's whole data size to the bytes-written counter; a flush
// with nothing changed adds nothing.
func TestFlushCountsBytesWritten(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	before := counterValue(t, metricHashStoreBytesWritten)
	require.NoError(t, s.Flush(ctx))
	total := 0
	for _, cm := range shardConfigMaps(t, client) {
		total += shardBytes(cm.Data)
	}
	assert.InDelta(t, float64(total), counterValue(t, metricHashStoreBytesWritten)-before, 0)

	before = counterValue(t, metricHashStoreBytesWritten)
	require.NoError(t, s.Flush(ctx))
	assert.InDelta(t, 0, counterValue(t, metricHashStoreBytesWritten)-before, 0)

	e := testEntry(7)
	e.Hash = "ffffffffffffffff"
	s.Put(testUID(7), e)
	require.NoError(t, s.Flush(ctx))
	s.mu.Lock()
	data := s.group(s.shards)[shardOf(testUID(7), s.shards)]
	s.mu.Unlock()
	assert.InDelta(t, float64(shardBytes(data)), counterValue(t, metricHashStoreBytesWritten)-before, 0)
}

func TestShardStaysUnder5000(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	// Grow across several flushes so resharding runs, not only the initial layout.
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	for i := 4000; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))

	cms := shardConfigMaps(t, client)
	total := 0
	for _, cm := range cms {
		assert.LessOrEqual(t, len(cm.Data), maxShardEntries, cm.Name)
		total += len(cm.Data)
	}
	assert.Equal(t, 12000, total)
	assert.Len(t, generations(t, client), 1, "the old generation is deleted")
	requireAll(t, client, 12000, testUID, testEntry)
	requireSettled(t, client)
}

func TestLoadShrinkReshardsIntoNewGeneration(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	for i := 10; i < 12000; i++ {
		s.Delete(testUID(i))
	}
	require.NoError(t, s.Flush(ctx))
	gen, _ := pointer(t, client)

	// On the next start the shard count is 1, so the entries move to a new generation.
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	require.NoError(t, r.Flush(ctx))
	cms := shardConfigMaps(t, client)
	require.Len(t, cms, 1)
	assert.Len(t, cms[0].Data, 10)
	newGen, shards := pointer(t, client)
	assert.Equal(t, gen+1, newGen)
	assert.Equal(t, 1, shards)
	requireAll(t, client, 10, testUID, testEntry)
	requireSettled(t, client)
}

func TestShardBoundedByBytes(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	long := strings.Repeat("n", 243)
	for i := 0; i < maxShardEntries; i++ {
		e := testEntry(i)
		e.Name = fmt.Sprintf("%s-%09d", long, i) // 253 characters
		s.Put(testUID(i), e)
	}
	require.NoError(t, s.Flush(ctx))

	cms := shardConfigMaps(t, client)
	require.Greater(t, len(cms), 1, "5,000 long-named entries need more than one shard")
	total := 0
	for _, cm := range cms {
		assert.Less(t, shardBytes(cm.Data), maxShardBytes, cm.Name)
		total += len(cm.Data)
	}
	assert.Equal(t, maxShardEntries, total)
	requireSettled(t, client)
}

// TestReshard3To4UnderApiserverLimit grows a store of 226-byte entries until the pointer
// moves from 3 shards to 4. Every write passes the 1 MiB check.
func TestReshard3To4UnderApiserverLimit(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	var counts []int
	n := 0
	for n < 20000 {
		for end := n + 250; n < end; n++ {
			s.Put(realUID(n), realEntry(n))
		}
		require.NoError(t, s.Flush(ctx), "flush at %d entries", n)
		_, shards := pointer(t, client)
		if len(counts) == 0 || counts[len(counts)-1] != shards {
			counts = append(counts, shards)
		}
		if shards >= 4 {
			break
		}
	}
	assert.Contains(t, fmt.Sprint(counts), "3 4", "shard counts seen: %v", counts)
	assert.Len(t, generations(t, client), 1)
	for _, cm := range shardConfigMaps(t, client) {
		assert.LessOrEqual(t, shardBytes(cm.Data), maxShardBytes, cm.Name)
	}
	requireAll(t, client, n, realUID, realEntry)
	requireSettled(t, client)
}

func TestReshard1To3UnderApiserverLimit(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 100; i++ {
		s.Put(realUID(i), realEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	_, shards := pointer(t, client)
	require.Equal(t, 1, shards)

	for i := 100; i < 8500; i++ {
		s.Put(realUID(i), realEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	gen, shards := pointer(t, client)
	assert.Equal(t, 2, gen)
	assert.Equal(t, 3, shards)
	assert.Equal(t, map[int]int{2: 3}, generations(t, client))
	requireAll(t, client, 8500, realUID, realEntry)
	requireSettled(t, client)
}

func TestReshardCreateFailureKeepsOldGeneration(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	old, err := client.CoreV1().ConfigMaps(storeNS).Get(ctx, shardName(1, 0), metav1.GetOptions{})
	require.NoError(t, err)

	// Growing to 12,000 reshards into generation 2; creating its shard 1 fails.
	fail := true
	failWrites(client, shardName(2, 1), &fail)
	for i := 4000; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	client.ClearActions()
	require.Error(t, s.Flush(ctx))
	assert.Equal(t, 0, countActions(client, "delete"))
	gen, shards := pointer(t, client)
	assert.Equal(t, 1, gen, "the pointer stays on the old generation")
	assert.Equal(t, 1, shards)
	now, err := client.CoreV1().ConfigMaps(storeNS).Get(ctx, shardName(1, 0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, old.Data, now.Data, "the old generation is untouched")

	// A crash here loses nothing that was persisted before the Flush.
	requireAll(t, client, 4000, testUID, testEntry)

	// The next Flush retries the reshard into a fresh generation 3, then deletes the
	// partial generation 2 and generation 1.
	fail = false
	require.NoError(t, s.Flush(ctx))
	gen, shards = pointer(t, client)
	assert.Equal(t, 3, gen)
	assert.Equal(t, map[int]int{3: shards}, generations(t, client))
	requireAll(t, client, 12000, testUID, testEntry)
	requireSettled(t, client)
}

func TestCrashBeforePointerMoveRecoversViaMerge(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))

	// Every generation-2 shard is written, then the pointer update fails: the carrier
	// "crashes" with two generations on the cluster and the pointer on the older one.
	fail := true
	failWrites(client, pointerName, &fail)
	for i := 4000; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	changed := testEntry(5)
	changed.Hash = "ffffffffffffffff"
	s.Put(testUID(5), changed)
	require.Error(t, s.Flush(ctx))
	gen, _ := pointer(t, client)
	require.Equal(t, 1, gen)
	require.Len(t, generations(t, client), 2)
	fail = false

	// A fresh Load merges both generations, newest copy winning.
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	got, ok := r.Get(testUID(5))
	require.True(t, ok)
	assert.Equal(t, changed, got, "the newer generation wins")
	for i := 0; i < 12000; i++ {
		if i == 5 {
			continue
		}
		got, ok := r.Get(testUID(i))
		require.True(t, ok, testUID(i))
		require.Equal(t, testEntry(i), got)
	}

	// Its first Flush rewrites into a generation above both, then removes them.
	require.NoError(t, r.Flush(ctx))
	gen, shards := pointer(t, client)
	assert.Equal(t, 3, gen)
	assert.Equal(t, map[int]int{3: shards}, generations(t, client))
	requireSettled(t, client)
}

// TestReshardRetryAfterAppliedPointerUsesFreshGeneration covers a pointer write that the
// apiserver applied but whose response was lost. The retry must not rewrite the generation
// the pointer now names, or a crash before the retry's own pointer write would leave the
// pointer on a generation whose shard count no longer matches its contents.
func TestReshardRetryAfterAppliedPointerUsesFreshGeneration(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))

	// mode 1: apply the pointer write, then report an error. mode 2: fail it unapplied.
	mode := 1
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	f := func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := actionConfigMap(a)
		if cm == nil || cm.Name != pointerName {
			return false, nil, nil
		}
		switch mode {
		case 1:
			require.NoError(t, client.Tracker().Update(gvr, cm.DeepCopy(), storeNS))
			return true, nil, fmt.Errorf("injected timeout after apply")
		case 2:
			return true, nil, fmt.Errorf("injected pointer failure")
		}
		return false, nil, nil
	}
	client.PrependReactor("create", "configmaps", f)
	client.PrependReactor("update", "configmaps", f)

	// Reshard 1 -> 2: the pointer moves to generation 2 but the carrier sees an error.
	for i := 4000; i < 8000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.Error(t, s.Flush(ctx))
	gen, shards := pointer(t, client)
	require.Equal(t, 2, gen)
	require.Equal(t, 2, shards)

	// The count grows to 3 before the retry, and the retry's pointer write fails: the
	// carrier "crashes" here.
	mode = 2
	for i := 8000; i < 12001; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.Error(t, s.Flush(ctx))
	mode = 0

	requireAll(t, client, 12001, testUID, testEntry)

	// The recovered store settles into one generation.
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	require.NoError(t, r.Flush(ctx))
	gen, shards = pointer(t, client)
	assert.Equal(t, map[int]int{gen: shards}, generations(t, client))
	requireAll(t, client, 12001, testUID, testEntry)
	requireSettled(t, client)
}

func TestMissingPointerRecoversViaMerge(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 100; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	require.NoError(t, client.CoreV1().ConfigMaps(storeNS).Delete(ctx, pointerName, metav1.DeleteOptions{}))

	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	require.NoError(t, r.Flush(ctx))
	gen, _ := pointer(t, client)
	assert.Equal(t, 2, gen)
	assert.Equal(t, map[int]int{2: 1}, generations(t, client))
	requireAll(t, client, 100, testUID, testEntry)
	requireSettled(t, client)
}

func TestFailedWriteKeepsShardDirty(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	s.Put("uid-a", testEntry(1))
	require.NoError(t, s.Flush(ctx))

	fail := true
	failWrites(client, shardName(1, 0), &fail)
	changed := testEntry(1)
	changed.Hash = "ffffffffffffffff"
	s.Put("uid-a", changed)
	require.Error(t, s.Flush(ctx))

	fail = false
	client.ClearActions()
	require.NoError(t, s.Flush(ctx))
	assert.Equal(t, 1, countActions(client, "update"))
	cms := shardConfigMaps(t, client)
	require.Len(t, cms, 1)
	assert.Equal(t, encodeEntry(changed), cms[0].Data["uid-a"])
}

func TestMissingReturnsStoredUidsNotListed(t *testing.T) {
	s := NewStore(newClient(), storeNS)
	s.Put("d1", testEntry(1))
	s.Put("d2", testEntry(2))
	s.Put("d3", testEntry(3))
	s.Put("n1", Entry{GVR: "/v1/namespaces", Kind: "Namespace", Name: "x"})

	listed := map[string]struct{}{"d2": {}}
	assert.ElementsMatch(t, []string{"d1", "d3"}, s.Missing("apps/v1/deployments", listed, true))
	assert.Empty(t, s.Missing("/v1/namespaces", map[string]struct{}{"n1": {}}, true))
}

func TestMissingSkipsKindWhoseListFailed(t *testing.T) {
	s := NewStore(newClient(), storeNS)
	s.Put("d1", testEntry(1))
	assert.Empty(t, s.Missing("apps/v1/deployments", nil, false))
}

func TestHMACKeyCreatedOnceAndReused(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	k1, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Len(t, k1, 32)
	k2, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Equal(t, k1, k2)

	creates := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "secrets" {
			creates++
		}
	}
	assert.Equal(t, 1, creates)
	sec, err := client.CoreV1().Secrets(storeNS).Get(ctx, HMACSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, k1, sec.Data[HMACSecretKey])
}

func TestHMACKeyCreateRaceUsesExisting(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	winner := []byte("0123456789abcdef0123456789abcdef")
	// Another writer creates the Secret between our Get and our Create.
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		obj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: HMACSecretName, Namespace: storeNS},
			Data:       map[string][]byte{HMACSecretKey: winner},
		}
		require.NoError(t, client.Tracker().Add(obj))
		return false, nil, nil
	})
	k, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Equal(t, winner, k)
}

func TestHMACKeyRejectsShortKey(t *testing.T) {
	ctx := context.Background()
	short := []byte("too-short-secret-value")
	client := newClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: HMACSecretName, Namespace: storeNS},
		Data:       map[string][]byte{HMACSecretKey: short},
	})
	k, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.Error(t, err)
	assert.Nil(t, k)
	assert.Contains(t, err.Error(), HMACSecretName)
	assert.Contains(t, err.Error(), HMACSecretKey)
	assert.NotContains(t, err.Error(), string(short))
}

func TestStoreConcurrentUse(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := w * 2000; i < (w+1)*2000; i++ {
				s.Put(testUID(i), testEntry(i))
				_, _ = s.Get(testUID(i))
				if i%3 == 0 {
					s.Delete(testUID(i))
				}
			}
		}(w)
	}
	for f := 0; f < 3; f++ {
		assert.NoError(t, s.Flush(ctx))
	}
	for w := 0; w < 4; w++ {
		<-done
	}
	require.NoError(t, s.Flush(ctx))

	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	for i := 0; i < 8000; i++ {
		_, ok := r.Get(testUID(i))
		assert.Equal(t, i%3 != 0, ok, testUID(i))
	}
}

func TestStoreLen(t *testing.T) {
	s := NewStore(newClient(), storeNS)
	s.Put(testUID(1), testEntry(1))
	s.Put(testUID(2), testEntry(2))
	s.Put(testUID(3), testEntry(3))
	s.Delete(testUID(2))
	assert.Equal(t, 2, s.Len())
}

// pointerHistoryStart returns the history-start annotation of the pointer ConfigMap.
func pointerHistoryStart(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(storeNS).Get(context.Background(), pointerName, metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Annotations[historyStartAnnotation]
}

// storedPointer returns a pointer ConfigMap naming generation 1 with one shard, and that
// shard holding testEntry(1). An empty historyStart leaves the annotation out.
func storedPointer(historyStart string) []runtime.Object {
	ptr := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pointerName,
			Namespace: storeNS,
			Labels:    map[string]string{storeLabelKey: storeLabelValue},
		},
		Data: map[string]string{pointerGeneration: "1", pointerShards: "1"},
	}
	if historyStart != "" {
		ptr.Annotations = map[string]string{historyStartAnnotation: historyStart}
	}
	shard := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shardName(1, 0),
			Namespace: storeNS,
			Labels:    map[string]string{storeLabelKey: storeLabelValue, generationLabelKey: "1"},
		},
		Data: map[string]string{testUID(1): encodeEntry(testEntry(1))},
	}
	return []runtime.Object{ptr, shard}
}

func TestHistoryStartSurvivesReload(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh store", func(t *testing.T) {
		client := newClient()
		s := NewStore(client, storeNS)
		before := time.Now().Truncate(time.Second)
		require.NoError(t, s.Load(ctx))
		hs := s.HistoryStart()
		assert.False(t, hs.Before(before), "history start %v is before Load ran", hs)
		assert.False(t, hs.After(time.Now()))

		s.Put(testUID(1), testEntry(1))
		require.NoError(t, s.Flush(ctx))
		assert.Equal(t, hs.UTC().Format(time.RFC3339), pointerHistoryStart(t, client))

		r := NewStore(client, storeNS)
		require.NoError(t, r.Load(ctx))
		assert.True(t, hs.Equal(r.HistoryStart()), "got %v, want %v", r.HistoryStart(), hs)
		assert.Equal(t, hs.UnixMilli(), r.HistoryStart().UnixMilli())
		requireSettled(t, client)
	})

	t.Run("pointer already annotated", func(t *testing.T) {
		const stored = "2026-01-02T03:04:05Z"
		client := newClient(storedPointer(stored)...)
		s := NewStore(client, storeNS)
		client.ClearActions()
		require.NoError(t, s.Load(ctx))
		assert.Equal(t, 0, countActions(client, "update")+countActions(client, "create"),
			"an annotated pointer is not rewritten by Load")
		want, err := time.Parse(time.RFC3339, stored)
		require.NoError(t, err)
		assert.True(t, want.Equal(s.HistoryStart()), "got %v", s.HistoryStart())

		e := testEntry(1)
		e.Hash = "ffffffffffffffff"
		s.Put(testUID(1), e)
		s.Put(testUID(2), testEntry(2))
		require.NoError(t, s.Flush(ctx))
		assert.Equal(t, stored, pointerHistoryStart(t, client))
	})

	t.Run("pointer without annotation", func(t *testing.T) {
		client := newClient(storedPointer("")...)
		s := NewStore(client, storeNS)
		client.ClearActions()
		require.NoError(t, s.Load(ctx))
		assert.Equal(t, 1, countActions(client, "update"), "Load annotates the pointer once")
		hs := s.HistoryStart()
		require.False(t, hs.IsZero())
		assert.Equal(t, hs.UTC().Format(time.RFC3339), pointerHistoryStart(t, client))
		gen, shards := pointer(t, client)
		assert.Equal(t, 1, gen, "annotating keeps the pointer's data")
		assert.Equal(t, 1, shards)

		r := NewStore(client, storeNS)
		require.NoError(t, r.Load(ctx))
		assert.True(t, hs.Equal(r.HistoryStart()))
		got, ok := r.Get(testUID(1))
		require.True(t, ok)
		assert.Equal(t, testEntry(1), got)
		requireSettled(t, client)
	})
}

func TestHistoryStartSurvivesReshard(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 100; i++ {
		s.Put(realUID(i), realEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	stored := pointerHistoryStart(t, client)
	require.NotEmpty(t, stored)

	for i := 100; i < 8500; i++ {
		s.Put(realUID(i), realEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	gen, shards := pointer(t, client)
	require.Equal(t, 2, gen, "the pointer moved to a new generation")
	require.Equal(t, 3, shards)
	assert.Equal(t, stored, pointerHistoryStart(t, client))

	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	assert.True(t, s.HistoryStart().Equal(r.HistoryStart()))
}
