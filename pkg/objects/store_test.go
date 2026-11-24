package objects

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
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
		if _, _, ok := parseShard(&cm); ok {
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
		g, _, _ := parseShard(&cm)
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
	assert.Equal(t, "nri-kube-events-hashes-a-0", cms[0].Name)
	assert.Equal(t, "1", cms[0].Labels[generationLabelKey])
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

	// The next Flush writes the shards of generation 2 it has not written yet, moves the
	// pointer, then deletes generation 1.
	fail = false
	require.NoError(t, s.Flush(ctx))
	gen, shards = pointer(t, client)
	assert.Equal(t, 2, gen)
	assert.Equal(t, map[int]int{2: shards}, generations(t, client))
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

	// Its first Flush rewrites into a generation above both, in the slot the pointer does
	// not name (generation 3 shares generation 1's slot, so it is 4), then removes them.
	require.NoError(t, r.Flush(ctx))
	gen, shards := pointer(t, client)
	assert.Equal(t, 4, gen)
	assert.Equal(t, map[int]int{4: shards}, generations(t, client))
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
	assert.ElementsMatch(t, []string{"d1", "d3"}, s.Missing("apps/v1/deployments", listed))
	assert.Empty(t, s.Missing("/v1/namespaces", map[string]struct{}{"n1": {}}))
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

// With a budget a flush writes the shards that changed first, stays within the budget but
// writes at least one shard, and leaves the rest dirty for later flushes, until every
// shard is written.
func TestFlushBudgetWritesOldestShardsFirst(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	s.mu.Lock()
	shards := s.shards
	s.mu.Unlock()
	require.Greater(t, shards, 2)

	// Change every shard, one after another.
	for i := 0; i < 12000; i++ {
		e := testEntry(i)
		e.Hash = "ffffffffffffffff"
		s.Put(testUID(i), e)
	}
	s.mu.Lock()
	first := s.takeDirtyPeek()
	one := shardBytes(s.group(s.shards)[first])
	s.mu.Unlock()

	client.ClearActions()
	require.NoError(t, s.FlushBudget(ctx, 1))
	assert.Equal(t, 1, countActions(client, "update"), "a budget below one shard still writes one")
	s.mu.Lock()
	_, stillDirty := s.dirty[first]
	left := len(s.dirty)
	s.mu.Unlock()
	assert.False(t, stillDirty, "the shard that changed first is written first")
	assert.Equal(t, shards-1, left)

	client.ClearActions()
	require.NoError(t, s.FlushBudget(ctx, 2*one+one/2))
	assert.Equal(t, 2, countActions(client, "update"))

	for flushes := 0; ; flushes++ {
		s.mu.Lock()
		n := len(s.dirty)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		require.Less(t, flushes, shards)
		require.NoError(t, s.FlushBudget(ctx, one))
	}
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	got, ok := r.Get(testUID(11999))
	require.True(t, ok)
	assert.Equal(t, "ffffffffffffffff", got.Hash)
}

// takeDirtyPeek returns the dirty shard that changed first. The caller holds s.mu.
func (s *Store) takeDirtyPeek() int {
	first, at := -1, time.Time{}
	for i, since := range s.dirty {
		if first < 0 || since.Before(at) || (since.Equal(at) && i < first) {
			first, at = i, since
		}
	}
	return first
}

// Shard names come from a finite set, so the chart's Role can list them: two slots by the
// parity of the generation, and at most MaxShards indexes.
func TestShardNamesAreFinite(t *testing.T) {
	names := ShardNames()
	assert.Len(t, names, 2*MaxShards)
	assert.Contains(t, names, shardName(1, 0))
	assert.Contains(t, names, shardName(2, MaxShards-1))
	assert.Equal(t, shardName(1, 3), shardName(7, 3), "odd generations share a slot")
	assert.NotEqual(t, shardName(1, 3), shardName(2, 3))
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: shardName(3, 2), Labels: map[string]string{generationLabelKey: "3"}}}
	gen, i, ok := parseShard(cm)
	assert.True(t, ok)
	assert.Equal(t, 3, gen)
	assert.Equal(t, 2, i)
	cm.Labels[generationLabelKey] = "4"
	_, _, ok = parseShard(cm)
	assert.False(t, ok, "a generation label whose slot is not the name's")
}

// A reshard whose pointer write landed but reported an error is finished by the next flush,
// which reads the pointer first.
func TestReshardAfterLandedPointerWriteKeepsThePointedSlot(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	// The pointer update to generation 2 lands, then reports an error.
	var once sync.Once
	client.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := a.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if cm.Name != pointerName {
			return false, nil, nil
		}
		var err error
		once.Do(func() {
			_ = client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, storeNS)
			err = errors.New("lost response")
		})
		if err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	for i := 4000; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.Error(t, s.Flush(ctx))
	gen, _ := pointer(t, client)
	require.Equal(t, 2, gen, "the pointer write landed")

	require.NoError(t, s.Flush(ctx))
	gen, shards := pointer(t, client)
	assert.Equal(t, 2, gen, "the next flush reads the pointer, finds the reshard done, and writes no generation into the slot it names")
	assert.Equal(t, map[int]int{2: shards}, generations(t, client))
	requireAll(t, client, 12000, testUID, testEntry)
}

// The chart's Role lists the shard names by template; it must agree with ShardNames.
func TestChartRoleNamesEveryShard(t *testing.T) {
	b, err := os.ReadFile("../../charts/nri-kube-events/templates/objects-role.yaml")
	require.NoError(t, err)
	role := string(b)
	assert.Contains(t, role, fmt.Sprintf("range $i := until %d", MaxShards))
	assert.Contains(t, role, `range $slot := list "`+slotOf(1)+`" "`+slotOf(2)+`"`)
	assert.Contains(t, role, "- "+shardNamePrefix+"{{ $slot }}-{{ $i }}")
	assert.Contains(t, role, "- "+pointerName)
	assert.Contains(t, role, "- "+HMACSecretName)
}

// The chart creates the HMAC Secret without data; the first start fills it by an update and
// later starts read the same key.
func TestHMACKeyFillsTheChartsEmptySecret(t *testing.T) {
	ctx := context.Background()
	client := newClient(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: HMACSecretName, Namespace: storeNS}})
	k1, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Len(t, k1, hmacKeyBytes)
	secretActions := map[string]int{}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "secrets" {
			secretActions[a.GetVerb()]++
		}
	}
	assert.Zero(t, secretActions["create"], "no Secret create")
	assert.Equal(t, 1, secretActions["update"])
	k2, err := LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Equal(t, k1, k2)
}

// A reshard with a budget writes the new generation over several flushes, keeps the
// pointer on the old one until every new shard is written, and loses nothing if the
// process restarts in between.
func TestReshardRespectsTheFlushBudget(t *testing.T) {
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	for i := 0; i < 4000; i++ {
		s.Put(testUID(i), testEntry(i))
	}
	require.NoError(t, s.Flush(ctx))
	for i := 4000; i < 12000; i++ {
		s.Put(testUID(i), testEntry(i))
	}

	client.ClearActions()
	require.NoError(t, s.FlushBudget(ctx, 1))
	assert.Equal(t, 1, countActions(client, "create")+countActions(client, "update"), "one shard per flush at a tiny budget")
	gen, _ := pointer(t, client)
	assert.Equal(t, 1, gen, "the pointer waits for every new shard")

	// A restart now merges the old generation and the new shard written so far.
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	got, ok := r.Get(testUID(0))
	require.True(t, ok)
	assert.Equal(t, testEntry(0), got)

	for flushes := 0; ; flushes++ {
		require.Less(t, flushes, 10)
		require.NoError(t, s.FlushBudget(ctx, 1))
		if g, _ := pointer(t, client); g != 1 {
			break
		}
	}
	gen, shards := pointer(t, client)
	assert.Equal(t, 2, gen)
	assert.Equal(t, map[int]int{2: shards}, generations(t, client))
	requireAll(t, client, 12000, testUID, testEntry)
}

// A store that needs more than MaxShards shards is not written; the gauge and an error log
// say so once, and the flush returns an error every time.
func TestStoreOverCapacityIsRefusedLoudly(t *testing.T) {
	s := NewStore(newClient(), storeNS)
	require.NoError(t, s.Load(context.Background()))
	s.mu.Lock()
	s.shards = MaxShards
	for i := 0; i < MaxShards*maxShardEntries+1; i++ {
		s.entries[testUID(i)] = Entry{Hash: "h"}
	}
	s.markDirty(0)
	s.mu.Unlock()
	err := s.Flush(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than 128 shards")
	var m dto.Metric
	require.NoError(t, metricHashStoreOverCapacity.Write(&m))
	assert.Equal(t, 1.0, m.GetGauge().GetValue())
}

// A reshard of a 200,000-object store that keeps changing, at the daily resend's rate of
// about 139 objects a minute, ends within about one flush per shard, as the review's store
// simulation measured it; then the changes made after a shard was written are written in
// place, and every entry is stored.
func TestReshardOfALargeChangingStoreFinishes(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 200,000 entries")
	}
	ctx := context.Background()
	client := newClient()
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	const n, churn = 200_000, 139
	entry := func(i int, lastSent int64) Entry {
		return Entry{GVR: "apps/v1/replicasets", Kind: "ReplicaSet", Namespace: "team-namespace-x",
			Name: fmt.Sprintf("service-name-%d-7d9f8c6b5", i), Hash: "0123456789abcdef", LastSent: lastSent}
	}
	uid := func(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }
	for i := 0; i < n; i++ {
		s.Put(uid(i), entry(i, 1_700_000_000))
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a repeatable test sequence
	last := map[int]int64{}
	moved := 0
	for flush := 1; flush <= 70 && moved == 0; flush++ {
		for k := 0; k < churn; k++ {
			i := rng.IntN(n)
			last[i] = int64(1_700_000_000 + flush)
			s.Put(uid(i), entry(i, last[i]))
		}
		require.NoError(t, s.FlushBudget(ctx, flushBudgetBytes))
		if _, err := client.CoreV1().ConfigMaps(storeNS).Get(ctx, pointerName, metav1.GetOptions{}); err == nil {
			moved = flush
		}
	}
	s.mu.Lock()
	shards := s.shards
	s.mu.Unlock()
	require.Positive(t, moved, "the pointer moved")
	t.Logf("%d shards; the pointer moved at flush %d", shards, moved)
	assert.LessOrEqual(t, moved, shards+2, "about one flush per shard")
	var m dto.Metric
	require.NoError(t, metricHashStoreReshardLeft.Write(&m))
	assert.Zero(t, m.GetGauge().GetValue())

	// The changes made after their shard was written follow in place within the budget.
	for flushes := 0; ; flushes++ {
		s.mu.Lock()
		dirty := len(s.dirty)
		s.mu.Unlock()
		if dirty == 0 {
			break
		}
		require.Less(t, flushes, shards)
		require.NoError(t, s.FlushBudget(ctx, flushBudgetBytes))
	}
	r := NewStore(client, storeNS)
	require.NoError(t, r.Load(ctx))
	assert.Equal(t, n, r.Len())
	for i, ls := range last {
		got, ok := r.Get(uid(i))
		require.True(t, ok)
		require.Equal(t, ls, got.LastSent, uid(i))
	}
}

// A key generated while the hash store exists replaced a lost key; it is counted. A key
// generated on a first install is not.
func TestReplacedRedactionKeyIsCounted(t *testing.T) {
	ctx := context.Background()
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: HMACSecretName, Namespace: storeNS}}
	before := counterValue(t, metricRedactionKeyReplaced)
	_, err := LoadOrCreateHMACKey(ctx, newClient(empty.DeepCopy()), storeNS)
	require.NoError(t, err)
	assert.Equal(t, before, counterValue(t, metricRedactionKeyReplaced), "a first install")

	client := newClient(empty.DeepCopy())
	s := NewStore(client, storeNS)
	require.NoError(t, s.Load(ctx))
	s.Put(testUID(1), testEntry(1))
	require.NoError(t, s.Flush(ctx))
	_, err = LoadOrCreateHMACKey(ctx, client, storeNS)
	require.NoError(t, err)
	assert.Equal(t, before+1, counterValue(t, metricRedactionKeyReplaced), "the Secret was emptied while the store existed")
}
