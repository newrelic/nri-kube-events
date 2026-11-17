package objects

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
)

// fakeFlag is a FlagSource whose state a test sets. Like the poller, it notifies through a
// channel buffered 1, so quick changes collapse into one receive.
type fakeFlag struct {
	changed chan struct{}
	mu      sync.Mutex
	state   accountflag.State
}

func newFakeFlag(s accountflag.State) *fakeFlag {
	return &fakeFlag{changed: make(chan struct{}, 1), state: s}
}

func (f *fakeFlag) State() accountflag.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeFlag) Changed() <-chan struct{} { return f.changed }

func (f *fakeFlag) set(s accountflag.State) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

type gated struct {
	cancel context.CancelFunc
	errc   chan error
}

func startGated(t *testing.T, flag FlagSource, run func(context.Context) error) *gated {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g := &gated{cancel: cancel, errc: make(chan error, 1)}
	go func() { g.errc <- RunGated(ctx, flag, run) }()
	t.Cleanup(cancel)
	return g
}

// stopGated cancels RunGated's context and returns its error.
func stopGated(t *testing.T, g *gated) error {
	t.Helper()
	g.cancel()
	select {
	case err := <-g.errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("RunGated did not return after cancel")
		return nil
	}
}

// seriesCount returns the number of series c exports, without the testutil package, which
// would add a module dependency.
func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}

// returned reports whether RunGated has returned, and its error if so.
func returned(g *gated) (bool, error) {
	select {
	case err := <-g.errc:
		return true, err
	default:
		return false, nil
	}
}

func TestRunGatedNeverOverlapsRuns(t *testing.T) {
	flag := newFakeFlag(accountflag.Unknown)
	var running, maxRunning, starts atomic.Int32
	run := func(ctx context.Context) error {
		starts.Add(1)
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a slow shutdown flush
		running.Add(-1)
		return nil
	}
	done := startGated(t, flag, run)
	for i := 0; i < 20; i++ {
		flag.set(accountflag.On)
		flag.set(accountflag.Off)
	}
	flag.set(accountflag.On)
	require.Eventually(t, func() bool { return running.Load() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, stopGated(t, done))
	assert.EqualValues(t, 1, maxRunning.Load())
	assert.GreaterOrEqual(t, starts.Load(), int32(1))
	assert.Zero(t, running.Load())
}

// TestRunGatedRestartsOnlyAfterSlowStop turns the flag back on while each run is still in
// its slow shutdown, so every restart has to wait for the previous run to return.
func TestRunGatedRestartsOnlyAfterSlowStop(t *testing.T) {
	flag := newFakeFlag(accountflag.On)
	var running, maxRunning, starts, stopping atomic.Int32
	run := func(ctx context.Context) error {
		starts.Add(1)
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-ctx.Done()
		stopping.Add(1)
		time.Sleep(20 * time.Millisecond) // a slow shutdown flush
		running.Add(-1)
		return nil
	}
	done := startGated(t, flag, run)
	for i := int32(1); i <= 5; i++ {
		require.Eventually(t, func() bool { return starts.Load() == i }, time.Second, time.Millisecond)
		flag.set(accountflag.Off)
		require.Eventually(t, func() bool { return stopping.Load() == i }, time.Second, time.Millisecond)
		flag.set(accountflag.On)
	}
	require.Eventually(t, func() bool { return starts.Load() == 6 && running.Load() == 1 },
		time.Second, time.Millisecond)
	require.NoError(t, stopGated(t, done))
	assert.EqualValues(t, 1, maxRunning.Load())
	assert.Zero(t, running.Load())
}

func TestRunGatedStaysOffWhileUnknown(t *testing.T) {
	flag := newFakeFlag(accountflag.Unknown)
	var calls atomic.Int32
	run := func(ctx context.Context) error {
		calls.Add(1)
		<-ctx.Done()
		return nil
	}
	done := startGated(t, flag, run)
	time.Sleep(25 * time.Millisecond)
	flag.set(accountflag.Off)
	flag.set(accountflag.Unknown)
	time.Sleep(25 * time.Millisecond)
	assert.Zero(t, calls.Load())
	require.NoError(t, stopGated(t, done))
	assert.Zero(t, calls.Load())
}

func TestRunGatedStopsOnOff(t *testing.T) {
	flag := newFakeFlag(accountflag.On)
	started := make(chan struct{})
	stopped := make(chan struct{})
	run := func(ctx context.Context) error {
		setObjectsMetric(map[string]int{"ConfigMap": 3})
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil
	}
	done := startGated(t, flag, run)
	<-started
	assert.Equal(t, 1, seriesCount(metricObjects))

	flag.set(accountflag.Off)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("run's context was not canceled on Off")
	}
	require.Eventually(t, func() bool { return seriesCount(metricObjects) == 0 },
		time.Second, time.Millisecond, "the objects gauge is reset on stop")

	time.Sleep(25 * time.Millisecond)
	ok, _ := returned(done)
	assert.False(t, ok, "RunGated keeps running while the flag is off")
	require.NoError(t, stopGated(t, done))
}

func TestRunGatedReturnsRunErrorOnShutdown(t *testing.T) {
	flag := newFakeFlag(accountflag.On)
	var started atomic.Bool
	flushFailed := errors.New("flush failed")
	run := func(ctx context.Context) error {
		started.Store(true)
		<-ctx.Done()
		return flushFailed
	}
	done := startGated(t, flag, run)
	require.Eventually(t, started.Load, time.Second, time.Millisecond)
	assert.ErrorIs(t, stopGated(t, done), flushFailed)
}

func TestRunGatedReturnsNilOnShutdownWhileOff(t *testing.T) {
	done := startGated(t, newFakeFlag(accountflag.Off), func(context.Context) error {
		t.Error("run called while off")
		return nil
	})
	assert.NoError(t, stopGated(t, done))
}

func TestRunGatedFailsWhenRunExitsEarly(t *testing.T) {
	watchFailed := errors.New("watch failed")
	for name, runErr := range map[string]error{"nil": nil, "error": watchFailed} {
		t.Run(name, func(t *testing.T) {
			flag := newFakeFlag(accountflag.On)
			done := startGated(t, flag, func(context.Context) error { return runErr })
			var err error
			require.Eventually(t, func() bool {
				var ok bool
				ok, err = returned(done)
				return ok
			}, time.Second, time.Millisecond, "RunGated returns without ctx being canceled")
			require.Error(t, err)
			assert.ErrorIs(t, err, errRunExited)
			if runErr != nil {
				assert.ErrorIs(t, err, runErr)
			}
		})
	}
}

func TestFlagOffThenOnReconciles(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	dyn := newDynamic(configMap("a", "uid-a", "v1"), configMap("b", "uid-b", "v1"))
	disc := newTestDiscovery(coreList(cmResource()))
	store := NewStore(newClient(), testNS)
	require.NoError(t, store.Load(context.Background()))
	sink := &recordingSink{}

	var mu sync.Mutex
	var watchers []*Watcher
	var running atomic.Int32
	run := func(ctx context.Context) error {
		w := NewWatcher(cfg, dyn, disc, store, sink, watcherKey)
		mu.Lock()
		watchers = append(watchers, w)
		mu.Unlock()
		running.Add(1)
		defer running.Add(-1)
		return w.Run(ctx)
	}

	flag := newFakeFlag(accountflag.On)
	done := startGated(t, flag, run)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"initial:uid-a", "initial:uid-b"}, sortedOps(sink.objectRecords()))
	var firstHashA string
	for _, r := range sink.objectRecords() {
		if r.Attributes["k8s.uid"] == "uid-a" {
			firstHashA = r.Attributes["object.hash"].(string)
		}
	}
	require.NotEmpty(t, firstHashA)

	flag.set(accountflag.Off)
	require.Eventually(t, func() bool { return running.Load() == 0 }, 5*time.Second, time.Millisecond)
	sink.reset()

	cms := dyn.Resource(cmGVR).Namespace("default")
	require.NoError(t, cms.Delete(context.Background(), "b", metav1.DeleteOptions{}))
	_, err := cms.Update(context.Background(), configMap("a", "uid-a", "v2"), metav1.UpdateOptions{})
	require.NoError(t, err)

	time.Sleep(3*cfg.HeartbeatInterval + 20*time.Millisecond)
	sink.mu.Lock()
	callsWhileOff := sink.calls
	sink.mu.Unlock()
	assert.Zero(t, callsWhileOff, "nothing is sent while the flag is off, the heartbeat included")

	flag.set(accountflag.On)
	require.Eventually(t, func() bool { return len(sink.objectRecords()) >= 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stopGated(t, done))

	recs := sink.objectRecords()
	assert.Equal(t, []string{"deleted:uid-b", "initial:uid-a"}, sortedOps(recs))
	for _, r := range recs {
		if r.Attributes["k8s.uid"] == "uid-a" {
			assert.NotEqual(t, firstHashA, r.Attributes["object.hash"], "cm-a is sent with its new content")
			e, ok := store.Get("uid-a")
			require.True(t, ok)
			assert.Equal(t, r.Attributes["object.hash"], e.Hash)
		}
	}
	_, ok := store.Get("uid-b")
	assert.False(t, ok, "the tombstoned object leaves the store")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, watchers, 2)
	for _, w := range watchers {
		assert.Same(t, store, w.store)
	}
}
