package objects

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
)

func heartbeats(recs []Record) []Record {
	var out []Record
	for _, r := range recs {
		if r.Attributes["type"] == HeartbeatType {
			out = append(out, r)
		}
	}
	return out
}

func statesOf(hbs []Record) []string {
	out := make([]string, 0, len(hbs))
	for _, h := range hbs {
		out = append(out, h.Attributes["carrier.state"].(string))
	}
	return out
}

// Flag on, off, on, then shutdown: each run's heartbeats say running, and each run ends
// with exactly one stopped heartbeat. The flag-off one reports the flag off, the shutdown
// one reports it on, and both carry the stop time.
func TestFinalHeartbeatAcrossFlagFlipsAndShutdown(t *testing.T) {
	cfg := testConfig()
	flag := newFakeFlag(accountflag.On)
	cfg.AccountFlag = func() string { return flag.State().String() }
	dyn := newDynamic(configMap("a", "uid-a", "v1"))
	disc := newTestDiscovery(coreList(cmResource()))
	store := NewStore(newClient(), testNS)
	require.NoError(t, store.Load(context.Background()))
	sink := &recordingSink{}
	stopAt := time.Unix(1700000000, 250_000_000)

	returned := make(chan struct{}, 4)
	run := func(ctx context.Context) error {
		w := NewWatcher(cfg, dyn, disc, store, sink, watcherKey)
		w.now = func() time.Time { return stopAt }
		defer func() { returned <- struct{}{} }()
		return w.Run(ctx)
	}
	waitReturned := func() {
		t.Helper()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("the watcher did not stop")
		}
	}
	running := func() bool {
		hbs := heartbeats(sink.records())
		return len(hbs) > 0 && hbs[len(hbs)-1].Attributes["carrier.state"] == CarrierStateRunning
	}

	g := startGated(t, flag, run)
	require.Eventually(t, running, 5*time.Second, 5*time.Millisecond, "a running heartbeat at start")

	flag.set(accountflag.Off)
	waitReturned()
	hbs := heartbeats(sink.records())
	assert.Equal(t, []string{CarrierStateRunning, CarrierStateStopped}, statesOf(hbs))
	stopped := hbs[len(hbs)-1].Attributes
	assert.Equal(t, "off", stopped["carrier.accountFlag"])
	assert.Equal(t, stopAt.UnixMilli(), stopped["carrier.stoppedAt"])
	assert.Equal(t, int64(0), hbs[0].Attributes["carrier.stoppedAt"])

	sink.reset()
	flag.set(accountflag.On)
	require.Eventually(t, running, 5*time.Second, 5*time.Millisecond, "running heartbeats resume")
	assert.Equal(t, int64(0), heartbeats(sink.records())[0].Attributes["carrier.stoppedAt"])

	require.NoError(t, stopGated(t, g))
	waitReturned()
	hbs = heartbeats(sink.records())
	assert.Equal(t, []string{CarrierStateRunning, CarrierStateStopped}, statesOf(hbs))
	assert.Equal(t, "on", hbs[len(hbs)-1].Attributes["carrier.accountFlag"], "a shutdown while the flag is on")
}

// stuckFinalSink accepts every record except the final heartbeat, whose send blocks until
// its context ends.
type stuckFinalSink struct {
	recordingSink
	mu      sync.Mutex
	blocked int
}

func (s *stuckFinalSink) Send(ctx context.Context, recs []Record) error {
	for _, r := range recs {
		if r.Attributes["type"] == HeartbeatType && r.Attributes["carrier.state"] != CarrierStateRunning {
			s.mu.Lock()
			s.blocked++
			s.mu.Unlock()
			<-ctx.Done()
			return errors.New("forwarder not answering")
		}
	}
	return s.recordingSink.Send(ctx, recs)
}

// A final heartbeat the forwarder never accepts is counted as a failed send, and the stop
// waits only for the final heartbeat's timeout.
func TestFailedFinalHeartbeatIsCountedAndDoesNotBlockTheStop(t *testing.T) {
	sink := &stuckFinalSink{}
	store := NewStore(newClient(), testNS)
	w := NewWatcher(testConfig(), newDynamic(), newTestDiscovery(coreList(cmResource())), store, sink, watcherKey)
	w.finalTimeout = 50 * time.Millisecond
	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return len(heartbeats(sink.records())) == 1 }, 5*time.Second, 5*time.Millisecond)
	failedBefore := w.HeartbeatRecord(time.Now()).Attributes["failedSends"].(int64)

	began := time.Now()
	require.NoError(t, stop())
	assert.Less(t, time.Since(began), 5*time.Second, "the stop is bounded by the final heartbeat's timeout")
	sink.mu.Lock()
	assert.Equal(t, 1, sink.blocked)
	sink.mu.Unlock()
	assert.Equal(t, failedBefore+1, w.HeartbeatRecord(time.Now()).Attributes["failedSends"])
}

func TestStoppedHeartbeatShape(t *testing.T) {
	w, _, _, sink := newTestWatcher(t, testConfig(), nil)
	at := time.Unix(1700000000, 0)
	w.now = func() time.Time { return at }
	w.sendStoppedHeartbeat()
	hbs := heartbeats(sink.records())
	require.Len(t, hbs, 1)
	assert.Equal(t, CarrierStateStopped, hbs[0].Attributes["carrier.state"])
	assert.Equal(t, at.UnixMilli(), hbs[0].Attributes["carrier.stoppedAt"])
	assert.Equal(t, at.Unix(), hbs[0].Attributes["timestamp"])
}
