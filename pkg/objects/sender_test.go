package objects

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateSink records every Send call. While held, Send blocks until release. It rejects, with
// ErrRejected, every call that carries a record of a uid in reject.
type gateSink struct {
	reject  map[string]bool
	held    chan struct{}
	entered chan struct{}
	calls   [][]Record
	mu      sync.Mutex
}

func newGateSink() *gateSink {
	return &gateSink{reject: map[string]bool{}, entered: make(chan struct{}, 100)}
}

func (s *gateSink) hold() {
	s.mu.Lock()
	s.held = make(chan struct{})
	s.mu.Unlock()
}

func (s *gateSink) release() {
	s.mu.Lock()
	close(s.held)
	s.held = nil
	s.mu.Unlock()
}

func (s *gateSink) Send(ctx context.Context, recs []Record) error {
	s.mu.Lock()
	held := s.held
	s.mu.Unlock()
	if held != nil {
		s.entered <- struct{}{}
		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		if uid, _ := r.Attributes["k8s.uid"].(string); s.reject[uid] {
			return fmt.Errorf("status 400: bad event: %w", ErrRejected)
		}
	}
	s.calls = append(s.calls, append([]Record(nil), recs...))
	return nil
}

// objectCalls returns, per accepted Send call, the uids of its object records.
func (s *gateSink) objectCalls() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]string
	for _, c := range s.calls {
		var uids []string
		for _, r := range c {
			if r.Attributes["type"] == ObjectType {
				uids = append(uids, r.Attributes["k8s.uid"].(string))
			}
		}
		if len(uids) > 0 {
			out = append(out, uids)
		}
	}
	return out
}

func startedSender(t *testing.T, sink RecordSink) *Watcher {
	t.Helper()
	store := NewStore(newClient(), testNS)
	require.NoError(t, store.Load(context.Background()))
	w := NewWatcher(testConfig(), newDynamic(), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	w.startSender()
	t.Cleanup(w.stopSender)
	return w
}

// While the forwarder holds a send, a watch event of the same kind is queued at once: the
// kind's store lock is never held across a network send. The objects queued meanwhile go
// out together in one call.
func TestSendsLeaveTheWatchAndGroupObjects(t *testing.T) {
	sink := newGateSink()
	w := startedSender(t, sink)
	ctx := context.Background()
	cms := w.newObjectStore(ctx, cmGVR)
	w.track(cmGVR, "ConfigMap")

	sink.hold()
	require.NoError(t, cms.Add(configMap("first", "uid-0", "v")))
	<-sink.entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 20; i++ {
			_ = cms.Add(configMap(fmt.Sprintf("cm-%d", i), fmt.Sprintf("uid-%d", i), "v"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch events waited for the held send")
	}
	sink.release()
	require.Eventually(t, func() bool { return len(storeUIDs(w.store)) == 21 }, 5*time.Second, 5*time.Millisecond)
	calls := sink.objectCalls()
	assert.Equal(t, []string{"uid-0"}, calls[0])
	assert.Len(t, calls, 2, "the 20 queued objects go out in one call")
	assert.Len(t, calls[1], 20)
}

// One object the forwarder always rejects is dropped and counted; the others in its group
// are delivered, and the dropped version is not sent again.
func TestRejectedObjectIsDroppedAndDoesNotStallItsGroup(t *testing.T) {
	sink := newGateSink()
	sink.reject["uid-bad"] = true
	w := startedSender(t, sink)
	ctx := context.Background()
	w.track(cmGVR, "ConfigMap")
	cms := w.newObjectStore(ctx, cmGVR)
	before := counterValue(t, metricRecordsRejected)

	sink.hold()
	require.NoError(t, cms.Add(configMap("first", "uid-first", "v")))
	<-sink.entered
	for _, uid := range []string{"uid-a", "uid-bad", "uid-b", "uid-c"} {
		require.NoError(t, cms.Add(configMap(uid, uid, "v")))
	}
	sink.release()
	require.Eventually(t, func() bool { return len(storeUIDs(w.store)) == 5 }, 5*time.Second, 5*time.Millisecond)
	var sent []string
	for _, c := range sink.objectCalls() {
		sent = append(sent, c...)
	}
	assert.ElementsMatch(t, []string{"uid-first", "uid-a", "uid-b", "uid-c"}, sent)
	assert.Equal(t, 1.0, counterValue(t, metricRecordsRejected)-before)
	assert.Equal(t, 0, unsyncedObjects(w), "a rejected object is not retried")

	calls := len(sink.objectCalls())
	require.NoError(t, cms.Update(configMap("uid-bad", "uid-bad", "v")))
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, sink.objectCalls(), calls, "the same version is not sent again")
}

// Versions of one object are delivered in the order they were queued, and the store ends
// with the last one.
func TestQueuedVersionsKeepTheirOrder(t *testing.T) {
	sink := newGateSink()
	w := startedSender(t, sink)
	ctx := context.Background()
	w.track(cmGVR, "ConfigMap")
	cms := w.newObjectStore(ctx, cmGVR)
	sink.hold()
	require.NoError(t, cms.Add(configMap("a", "uid-a", "v1")))
	<-sink.entered
	require.NoError(t, cms.Update(configMap("a", "uid-a", "v2")))
	require.NoError(t, cms.Update(configMap("a", "uid-a", "v3")))
	require.NoError(t, cms.Update(configMap("a", "uid-a", "v3")))
	sink.release()
	_, hashV3 := prepareObject(cmGVR, "ConfigMap", configMap("a", "uid-a", "v3").Object, watcherKey)
	require.Eventually(t, func() bool {
		e, ok := w.store.Get("uid-a")
		return ok && e.Hash == hashV3
	}, 5*time.Second, 5*time.Millisecond)
	var sent []string
	for _, c := range sink.objectCalls() {
		sent = append(sent, c...)
	}
	assert.Equal(t, []string{"uid-a", "uid-a", "uid-a"}, sent, "v1, v2 and v3, and the repeated v3 is not queued again")
}

// The queue left at shutdown is still delivered.
func TestStopSenderDeliversTheQueue(t *testing.T) {
	sink := newGateSink()
	store := NewStore(newClient(), testNS)
	w := NewWatcher(testConfig(), newDynamic(), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	w.startSender()
	w.track(cmGVR, "ConfigMap")
	cms := w.newObjectStore(context.Background(), cmGVR)
	for i := 0; i < 5; i++ {
		require.NoError(t, cms.Add(configMap(fmt.Sprintf("cm-%d", i), fmt.Sprintf("uid-%d", i), "v")))
	}
	w.stopSender()
	assert.Len(t, storeUIDs(store), 5)
}

func TestWarnerRateLimits(t *testing.T) {
	logger, hook := test.NewNullLogger()
	r := newWarner()
	for i := 0; i < 100; i++ {
		r.warn(log.NewEntry(logger), "objects: failed")
	}
	assert.LessOrEqual(t, len(hook.AllEntries()), warnBurst+1)
	assert.Positive(t, r.suppressed.Load())
	time.Sleep(time.Second / warnsPerSecond)
	r.warn(log.NewEntry(logger), "objects: failed")
	last := hook.LastEntry()
	require.NotNil(t, last)
	assert.True(t, strings.HasPrefix(last.Message, "objects:"))
	assert.Contains(t, last.Data, "suppressedWarnings")
}

// rejectAllSink rejects every Send, heartbeats included, and counts the calls.
type rejectAllSink struct {
	calls atomic.Int32
}

func (s *rejectAllSink) Send(context.Context, []Record) error {
	s.calls.Add(1)
	return fmt.Errorf("status 404: not found: %w", ErrRejected)
}

// A forwarder that rejects every request is a forwarder failure, not a batch of poison
// objects: nothing is stored as sent, every object stays unsynced for the retry pass, the
// metric counts it, and the group is not bisected.
func TestForwarderThatRejectsEverythingKeepsObjectsUnsynced(t *testing.T) {
	sink := &rejectAllSink{}
	store := NewStore(newClient(), testNS)
	w := NewWatcher(testConfig(), newDynamic(), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	w.track(cmGVR, "ConfigMap")
	before := counterValue(t, metricForwarderRejectsAll)
	rejectedBefore := counterValue(t, metricRecordsRejected)
	items := make([]pendingSend, 0, 8)
	for i := 0; i < 8; i++ {
		_, hash := prepareObject(cmGVR, "ConfigMap", configMap(fmt.Sprintf("cm-%d", i), fmt.Sprintf("uid-%d", i), "v").Object, watcherKey)
		recs := []Record{{Attributes: map[string]any{"type": ObjectType, "k8s.uid": fmt.Sprintf("uid-%d", i)}}}
		items = append(items, pendingSend{uid: fmt.Sprintf("uid-%d", i), op: OpAdded, recs: recs, size: queuedSize(recs),
			entry: Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "default", Name: fmt.Sprintf("cm-%d", i), Hash: hash}})
	}
	err := w.deliver(context.Background(), items)
	require.ErrorIs(t, err, errForwarderRejectsAll)
	assert.Equal(t, int32(2), sink.calls.Load(), "the group and one heartbeat probe, no bisection")
	assert.Empty(t, storeUIDs(store), "nothing is stored as sent")
	assert.Equal(t, 8, unsyncedObjects(w))
	assert.Equal(t, 1.0, counterValue(t, metricForwarderRejectsAll)-before)
	assert.Equal(t, 0.0, counterValue(t, metricRecordsRejected)-rejectedBefore, "no object is counted as poison")
}

// While the forwarder rejects everything, the sender waits between groups.
func TestSenderBacksOffWhileTheForwarderRejectsEverything(t *testing.T) {
	sink := &rejectAllSink{}
	w := startedSender(t, sink)
	w.track(cmGVR, "ConfigMap")
	cms := w.newObjectStore(context.Background(), cmGVR)
	for i := 0; i < 50; i++ {
		require.NoError(t, cms.Add(configMap(fmt.Sprintf("cm-%d", i), fmt.Sprintf("uid-%d", i), "v")))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	assert.LessOrEqual(t, int(sink.calls.Load()), 4, "at most two groups, each with its probe, before the first wait ends")
}
