package objects

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newResendWatcher returns a watcher with resendAfter whose ConfigMaps are watched and
// listed, and whose dynamic client serves objs.
func newResendWatcher(t *testing.T, resendAfter time.Duration, objs ...runtime.Object) (*Watcher, *Store, *recordingSink) {
	t.Helper()
	cfg := testConfig()
	cfg.ResendAfter = resendAfter
	w, _, store, sink := newTestWatcher(t, cfg, nil, objs...)
	s := w.newObjectStore(context.Background(), cmGVR)
	watchedConfigMaps(w, s)
	w.markListed(cmGVR)
	return w, store, sink
}

// putOverdue stores n ConfigMap entries uid-00000 … that were last sent at lastSent.
func putOverdue(store *Store, n int, lastSent time.Time) {
	for i := 0; i < n; i++ {
		store.Put(fmt.Sprintf("uid-%05d", i), Entry{
			GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default",
			Name: fmt.Sprintf("cm-%05d", i), Hash: "0000000000000000", LastSent: lastSent.Unix(),
		})
	}
}

// byOrder returns uids sorted as the resend scan orders them.
func byOrder(uids []string) []string {
	out := append([]string(nil), uids...)
	sort.Slice(out, func(i, j int) bool {
		if oi, oj := uidOrder(out[i]), uidOrder(out[j]); oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

// storeConfigMapA stores ConfigMap default/a, uid-a, value v1 as sent at lastSent, with the
// hash the watcher computes for it.
func storeConfigMapA(store *Store, lastSent time.Time) {
	_, hash := PrepareObject(cmGVR, "ConfigMap", configMap("a", "uid-a", "v1").Object, watcherKey)
	store.Put("uid-a", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "a", Hash: hash, LastSent: lastSent.Unix()})
}

// A scan over a backlog sends the steady part, ceil(3000/1440) = 3, plus the catch-up part,
// ceil(3000/60) = 50.
func TestResendScanAddsCatchUpForBacklog(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	putOverdue(store, 3000, time.Unix(1_000_000, 0))
	due := w.dueForResend(time.Unix(1_000_000, 0).Add(48 * time.Hour))
	assert.Len(t, due, 53)
	assert.True(t, sort.SliceIsSorted(due, func(i, j int) bool { return uidOrder(due[i]) < uidOrder(due[j]) }))
}

func TestResendScanCapsBacklog(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	w.cfg.MaxResendsPerScan = 20
	putOverdue(store, 3000, time.Unix(1_000_000, 0))
	assert.Len(t, w.dueForResend(time.Unix(1_000_000, 0).Add(48*time.Hour)), 20)
}

func TestScanBudget(t *testing.T) {
	w, _, _ := newResendWatcher(t, 24*time.Hour)
	w.mu.Lock()
	defer w.mu.Unlock()
	// No backlog: the scan takes what is due, up to the steady part.
	assert.Equal(t, 0, w.scanBudget(14400, 0))
	assert.Equal(t, 10, w.scanBudget(14400, 10))
	// A backlog of 3,600 adds ceil(3600/60) = 60 to the steady 10.
	assert.Equal(t, 70, w.scanBudget(14400, 3600))
	// The catch-up part holds while the backlog shrinks, and grows if it grows.
	assert.Equal(t, 70, w.scanBudget(14400, 1200))
	assert.Equal(t, 110, w.scanBudget(14400, 6000))
	// The catch-up part never takes more than is due.
	assert.Equal(t, 30, w.scanBudget(14400, 30))
	// It resets once a scan finds no more due than the steady part.
	assert.Equal(t, 10, w.scanBudget(14400, 10))
	assert.Equal(t, 0, w.catchUp)
	assert.Equal(t, 11, w.scanBudget(14400, 11)) // 10 + ceil(11/60)
}

// simulateResend runs one resend scan a minute from start for scans scans over store, and
// records each sent uid's send as the real carrier would: LastSent becomes the scan time. It
// returns, per uid, the first scan that sent it, and the largest age of a last send at the
// moment it was resent.
func simulateResend(w *Watcher, store *Store, start time.Time, scans int) (map[string]int, time.Duration) {
	firstScan := map[string]int{}
	var maxAge time.Duration
	for k := 0; k < scans; k++ {
		now := start.Add(time.Duration(k) * time.Minute)
		for _, uid := range w.dueForResend(now) {
			e, _ := store.Get(uid)
			maxAge = max(maxAge, now.Sub(time.Unix(e.LastSent, 0)))
			e.LastSent = now.Unix()
			store.Put(uid, e)
			if _, ok := firstScan[uid]; !ok {
				firstScan[uid] = k
			}
		}
	}
	return firstScan, maxAge
}

// putSpread stores n ConfigMap entries whose last sends were spread over the resendAfter
// before at, as the first sends of one startup spread them.
func putSpread(store *Store, n int, at time.Time, resendAfter time.Duration) {
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("uid-%05d", i)
		store.Put(uid, Entry{
			GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default",
			Name: fmt.Sprintf("cm-%05d", i), Hash: "0000000000000000", LastSent: firstSentAt(uid, at, resendAfter),
		})
	}
}

// After a six-hour outage a quarter of the store is overdue. The resend sends every one of
// those objects within about an hour, and no object goes more than resendAfter plus the
// outage plus that hour without a record.
func TestResendDrainsBacklogAfterOutage(t *testing.T) {
	const n = 14400
	resendAfter := 24 * time.Hour
	outageStart := time.Unix(1_700_000_000, 0)
	outage := 6 * time.Hour
	w, store, _ := newResendWatcher(t, resendAfter)
	putSpread(store, n, outageStart, resendAfter)

	restart := outageStart.Add(outage)
	var overdue []string
	for uid, e := range store.entriesWhere(func(Entry) bool { return true }) {
		if w.isDue(e, restart) {
			overdue = append(overdue, uid)
		}
	}
	require.InDelta(t, n/4, len(overdue), n/40)

	firstScan, maxAge := simulateResend(w, store, restart, 180)
	for _, uid := range overdue {
		k, ok := firstScan[uid]
		require.True(t, ok, "%s was never resent", uid)
		assert.LessOrEqual(t, k, 62, "%s was first resent in scan %d", uid, k)
	}
	assert.LessOrEqual(t, maxAge, resendAfter+outage+62*time.Minute)

	// With the backlog drained, the scans fall back to the steady part. A minute slot can
	// hold one uid more than the average, which adds a catch-up part of one.
	w.mu.Lock()
	assert.LessOrEqual(t, w.catchUp, 1)
	w.mu.Unlock()
	assert.LessOrEqual(t, len(w.dueForResend(restart.Add(180*time.Minute))), resendBudget(n)+1)
}

// With no outage, the scans never add a catch-up part.
func TestResendSteadyStateNeedsNoCatchUp(t *testing.T) {
	const n = 14400
	resendAfter := 24 * time.Hour
	at := time.Unix(1_700_000_000, 0)
	w, store, _ := newResendWatcher(t, resendAfter)
	putSpread(store, n, at, resendAfter)

	_, maxAge := simulateResend(w, store, at, 120)
	assert.LessOrEqual(t, maxAge, resendAfter+2*time.Minute)
}

func TestResendBudget(t *testing.T) {
	for entries, want := range map[int]int{0: 0, 1: 1, 1440: 1, 1441: 2, 2881: 3, 3000: 3} {
		assert.Equal(t, want, resendBudget(entries), "entries=%d", entries)
	}
}

func TestDueSkipsFreshAndUnwatchedEntries(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-48 * time.Hour).Unix()
	// widgets have a reflector whose first list has not finished; secrets are not watched.
	w.mu.Lock()
	w.gvrs[widgetGVR] = &gvrState{kind: "Widget", cancel: func() {}, store: w.newObjectStore(context.Background(), widgetGVR)}
	w.mu.Unlock()

	store.Put("uid-fresh", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "fresh", Hash: "h", LastSent: now.Add(-time.Hour).Unix()})
	store.Put("uid-widget", Entry{GVR: "example.com/v1/widgets", Kind: "Widget", Namespace: "default", Name: "w", Hash: "h", LastSent: old})
	store.Put("uid-secret", Entry{GVR: "/v1/secrets", Kind: "Secret", Namespace: "default", Name: "s", Hash: "h", LastSent: old})
	store.Put("uid-pointer", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: testNS, Name: pointerName, Hash: "h", LastSent: old})
	store.Put("uid-shard", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: testNS, Name: shardNamePrefix + "1-0", Hash: "h", LastSent: old})
	store.Put("uid-hmac", Entry{GVR: "/v1/secrets", Kind: "Secret", Namespace: testNS, Name: HMACSecretName, Hash: "h", LastSent: old})
	assert.Empty(t, w.dueForResend(now))

	// The same scan does pick an overdue entry of a watched and listed kind.
	store.Put("uid-due", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "due", Hash: "h", LastSent: old})
	assert.Equal(t, []string{"uid-due"}, w.dueForResend(now))
}

func TestDueNeedsResendOn(t *testing.T) {
	w, store, _ := newResendWatcher(t, 0)
	putOverdue(store, 10, time.Unix(1_000_000, 0))
	assert.Empty(t, w.dueForResend(time.Unix(1_000_000, 0).Add(48*time.Hour)))
}

// Three scans over 2,881 overdue entries (budget 3 + ceil(2881/60) = 52) take the first 156
// in scan order, with no uid twice.
func TestCursorAdvancesInUIDOrder(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	putOverdue(store, 2881, time.Unix(1_000_000, 0))
	now := time.Unix(1_000_000, 0).Add(48 * time.Hour)
	all := byOrder(entryUIDs(store.entriesWhere(func(Entry) bool { return true })))

	var got []string
	for i := 0; i < 3; i++ {
		due := w.dueForResend(now)
		require.Len(t, due, 52)
		got = append(got, due...)
	}
	assert.Equal(t, all[:156], got)
}

// A scan that starts below the highest order takes it, then wraps to the lowest orders.
func TestCursorWraps(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	putOverdue(store, 2881, time.Unix(1_000_000, 0))
	now := time.Unix(1_000_000, 0).Add(48 * time.Hour)
	all := byOrder(entryUIDs(store.entriesWhere(func(Entry) bool { return true })))
	n := len(all)

	w.mu.Lock()
	w.resyncCursor = uidOrder(all[n-2])
	w.mu.Unlock()
	w.cfg.MaxResendsPerScan = 3
	assert.Equal(t, []string{all[n-1], all[0], all[1]}, w.dueForResend(now))
	w.mu.Lock()
	assert.Equal(t, uidOrder(all[1]), w.resyncCursor)
	w.mu.Unlock()
}

// A scan with fewer due entries than its budget takes each once.
func TestScanTakesEachDueEntryOnce(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	now := time.Unix(1_700_000_000, 0)
	putOverdue(store, 1441, now) // budget 2, none due
	store.Put("uid-due", Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: "due", Hash: "h", LastSent: now.Add(-48 * time.Hour).Unix()})
	assert.Equal(t, []string{"uid-due"}, w.dueForResend(now))
}

func entryUIDs(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestResendSendsUnchangedObjectAsResync(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-a", "v1"))
	storeConfigMapA(store, sent)
	w.now = func() time.Time { return now }
	before := counterValue(t, metricResends)

	w.resendOne(context.Background(), "uid-a")
	recs := sink.objectRecords()
	require.Equal(t, []string{"resync:uid-a"}, ops(recs))
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, now.Unix(), e.LastSent)
	assert.Equal(t, recs[0].Attributes["object.hash"], e.Hash)
	assert.Equal(t, before+1, counterValue(t, metricResends))
}

func TestResendOf404SendsTombstone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w, store, sink := newResendWatcher(t, 24*time.Hour)
	storeConfigMapA(store, now.Add(-25*time.Hour))
	w.now = func() time.Time { return now }
	before := counterValue(t, metricResends)

	w.resendOne(context.Background(), "uid-a")
	recs := sink.objectRecords()
	require.Equal(t, []string{"deleted:uid-a"}, ops(recs))
	assert.Equal(t, now.Unix(), recs[0].Attributes["timestamp"])
	assert.Equal(t, "a", recs[0].Attributes["k8s.name"])
	_, ok := store.Get("uid-a")
	assert.False(t, ok)
	assert.Equal(t, before, counterValue(t, metricResends))
}

func TestResendOfRecreatedNameSendsTombstone(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-new", "v1"))
	storeConfigMapA(store, now.Add(-25*time.Hour))
	w.now = func() time.Time { return now }

	w.resendOne(context.Background(), "uid-a")
	recs := sink.objectRecords()
	require.Equal(t, []string{"deleted:uid-a"}, ops(recs))
	assert.Equal(t, now.Unix(), recs[0].Attributes["timestamp"])
	_, ok := store.Get("uid-a")
	assert.False(t, ok)
	// The new object is its own watch's to send.
	_, ok = store.Get("uid-new")
	assert.False(t, ok)
}

func TestResendFailureKeepsObjectDue(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-a", "v1"))
	storeConfigMapA(store, sent)
	w.now = func() time.Time { return now }
	failures, resends := counterValue(t, metricSendFailures), counterValue(t, metricResends)

	require.Equal(t, []string{"uid-a"}, w.dueForResend(now))
	sink.setFail(true)
	w.resendOne(context.Background(), "uid-a")
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, sent.Unix(), e.LastSent)
	assert.Equal(t, failures+1, counterValue(t, metricSendFailures))
	assert.Equal(t, resends, counterValue(t, metricResends))
	// The failed send left uid-a unsynced, so scans leave it to the retry pass. The pass finds
	// the stored hash unchanged, sends nothing and clears it, and the next scan takes it.
	assert.Empty(t, w.dueForResend(now.Add(time.Minute)))
	sink.setFail(false)
	w.retryUnsynced(context.Background())
	assert.Empty(t, sink.objectRecords())
	assert.Equal(t, []string{"uid-a"}, w.dueForResend(now.Add(2*time.Minute)))
}

// A failed GET leaves the object stored as it was, and due.
func TestResendGetErrorKeepsObjectDue(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-a", "v1"))
	storeConfigMapA(store, sent)
	w.now = func() time.Time { return now }
	w.dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})

	w.resendOne(context.Background(), "uid-a")
	assert.Empty(t, sink.objectRecords())
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, sent.Unix(), e.LastSent)
	assert.Equal(t, []string{"uid-a"}, w.dueForResend(now))
}

// A watch update sent while resendOne waits for the kind's lock makes the object fresh, so
// resendOne sends nothing once it has the lock.
func TestResendRechecksUnderKindLock(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-a", "v1"))
	storeConfigMapA(store, sent)
	w.now = func() time.Time { return now }
	require.Equal(t, []string{"uid-a"}, w.dueForResend(now))

	s := w.reflectorStore(cmGVR)
	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.resendOne(context.Background(), "uid-a")
	}()
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, sink.objectRecords(), "resendOne sent without the kind's lock")
	// The watch update, as objectStore.Update handles it under s.mu.
	require.NoError(t, w.HandleObject(context.Background(), cmGVR, OpUpdated, configMap("a", "uid-a", "v2").Object))
	s.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resendOne never returned")
	}
	assert.Equal(t, []string{"updated:uid-a"}, ops(sink.objectRecords()))
}

func TestFirstSendSpreadsLastSent(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	at := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return at }
	ctx := context.Background()
	minutes := map[int64]struct{}{}
	for i := 0; i < 10_000; i++ {
		uid := fmt.Sprintf("uid-%05d", i)
		require.NoError(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap(fmt.Sprintf("cm-%05d", i), uid, "v1").Object))
		e, ok := store.Get(uid)
		require.True(t, ok)
		require.Greater(t, e.LastSent, at.Add(-24*time.Hour).Unix(), uid)
		require.LessOrEqual(t, e.LastSent, at.Unix(), uid)
		minutes[(e.LastSent+int64((24*time.Hour).Seconds()))/60] = struct{}{}
	}
	assert.GreaterOrEqual(t, len(minutes), 1000)
}

func TestFirstSentAt(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, uid := range []string{"a", "b", "uid-1"} {
		want := at.Unix() - int64((24*time.Hour).Seconds()*float64(uidOrder(uid)%1440)/1440)
		assert.Equal(t, want, firstSentAt(uid, at, 24*time.Hour))
	}
}

func TestLaterSendsRecordRealTime(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	ctx := context.Background()
	first := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return first }
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpInitial, configMap("a", "uid-a", "v1").Object))

	later := first.Add(time.Hour)
	w.now = func() time.Time { return later }
	require.NoError(t, w.HandleObject(ctx, cmGVR, OpUpdated, configMap("a", "uid-a", "v2").Object))
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, later.Unix(), e.LastSent)
}

func TestResendOffSendsNothing(t *testing.T) {
	cfg := testConfig()
	cfg.ResendInterval = 10 * time.Millisecond
	w, _, store, sink := newTestWatcher(t, cfg, nil, configMap("a", "uid-a", "v1"), configMap("b", "uid-b", "v1"))
	at := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return at }
	// uid-a was sent two days ago and is unchanged.
	storeConfigMapA(store, at.Add(-48*time.Hour))
	require.NoError(t, store.Flush(context.Background()))

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, stop())
	assert.Equal(t, []string{"initial:uid-b"}, ops(sink.objectRecords()))
	e, ok := store.Get("uid-b")
	require.True(t, ok)
	assert.Equal(t, at.Unix(), e.LastSent)
}

func TestRunResendsOnTick(t *testing.T) {
	cfg := testConfig()
	cfg.ResendAfter = 24 * time.Hour
	cfg.ResendInterval = 10 * time.Millisecond
	w, _, store, sink := newTestWatcher(t, cfg, nil, configMap("a", "uid-a", "v1"))
	at := time.Unix(1_700_000_000, 0)
	w.now = func() time.Time { return at }
	storeConfigMapA(store, at.Add(-48*time.Hour))
	require.NoError(t, store.Flush(context.Background()))

	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stop())
	assert.Equal(t, []string{"resync:uid-a"}, ops(sink.objectRecords()))
}

func TestHeartbeatReportsCursor(t *testing.T) {
	w, store, _ := newResendWatcher(t, 24*time.Hour)
	putOverdue(store, 2000, time.Unix(1_000_000, 0))
	due := w.dueForResend(time.Unix(1_000_000, 0).Add(48 * time.Hour))
	require.Len(t, due, 36) // 2 + ceil(2000/60)
	assert.Equal(t, fmt.Sprintf("%016x", uidOrder(due[len(due)-1])), w.HeartbeatRecord(time.Now()).Attributes["carrier.resyncCursor"])
}

// hangingGets is a dynamic client whose GETs return only when their context ends.
type hangingGets struct{ dynamic.Interface }

func (h hangingGets) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return hangingResource{h.Interface.Resource(gvr)}
}

type hangingResource struct {
	dynamic.NamespaceableResourceInterface
}

func (r hangingResource) Namespace(string) dynamic.ResourceInterface { return r }

func (r hangingResource) Get(ctx context.Context, _ string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A GET that never returns times out: the scan sends nothing, releases the kind's lock and
// ends, and the object stays due.
func TestResendGetTimesOut(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour, configMap("a", "uid-a", "v1"))
	storeConfigMapA(store, sent)
	w.now = func() time.Time { return now }
	w.dyn = hangingGets{w.dyn}
	w.resendGetTimeout = 50 * time.Millisecond
	assert.Equal(t, 30*time.Second, resendGetTimeout)

	w.startResend(context.Background())
	require.Eventually(t, func() bool { return !w.resendBusy.Load() }, 5*time.Second, 10*time.Millisecond, "the scan never ended")
	w.background.Wait()
	s := w.reflectorStore(cmGVR)
	require.True(t, s.mu.TryLock(), "the kind's lock is still held")
	s.mu.Unlock()
	assert.Empty(t, sink.objectRecords())
	e, ok := store.Get("uid-a")
	require.True(t, ok)
	assert.Equal(t, sent.Unix(), e.LastSent)
	assert.Equal(t, []string{"uid-a"}, w.dueForResend(now))
}

// A uid whose tombstone failed is left to the retry pass, which keeps the observed deletion
// time; the resend neither picks it nor sends it.
func TestResendSkipsUnsyncedUIDs(t *testing.T) {
	sent := time.Unix(1_700_000_000, 0)
	deleted := sent.Add(24 * time.Hour)
	now := sent.Add(25 * time.Hour)
	w, store, sink := newResendWatcher(t, 24*time.Hour)
	storeConfigMapA(store, sent)
	e, _ := store.Get("uid-a")
	sink.setFail(true)
	w.sendTombstone(context.Background(), "uid-a", e, deleted)
	sink.setFail(false)
	require.Equal(t, 1, unsyncedObjects(w))

	w.now = func() time.Time { return now }
	assert.Empty(t, w.dueForResend(now))
	w.resendOne(context.Background(), "uid-a")
	assert.Empty(t, sink.objectRecords())

	w.retryUnsynced(context.Background())
	recs := sink.objectRecords()
	require.Equal(t, []string{"deleted:uid-a"}, ops(recs))
	assert.Equal(t, deleted.Unix(), recs[0].Attributes["timestamp"])
}
