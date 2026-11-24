package objects

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
)

var cmResourceGR = cmGVR.GroupResource()

func changedSignal(c *Coverage) bool {
	select {
	case <-c.Changed():
		return true
	default:
		return false
	}
}

func TestNilCoverageCoversNothing(t *testing.T) {
	var c *Coverage
	assert.False(t, c.Covers(cmResourceGR))
	assert.Nil(t, c.Changed())
	c.set(cmResourceGR, true)
	c.clear()
}

func TestCoverageNotifiesOnlyOnChange(t *testing.T) {
	c := NewCoverage()
	c.set(cmResourceGR, true)
	assert.True(t, c.Covers(cmResourceGR))
	assert.True(t, changedSignal(c))
	c.set(cmResourceGR, true)
	assert.False(t, changedSignal(c), "setting the same value again is no change")
	c.set(cmResourceGR, false)
	assert.True(t, changedSignal(c))
	c.clear()
	assert.False(t, changedSignal(c), "clearing an empty coverage is no change")
}

// A resource is covered only after its first list has been sent, stops being covered while
// its list fails, and stops for good when the watcher shuts down, even if a list completes
// after that.
func TestWatcherCoverageFollowsListsAndShutdown(t *testing.T) {
	cfg := testConfig()
	cfg.Coverage = NewCoverage()
	w, _, store, sink := newTestWatcher(t, cfg, nil)
	require.NoError(t, store.Load(context.Background()))
	ctx := context.Background()
	w.track(cmGVR, "ConfigMap")
	cms := w.newObjectStore(ctx, cmGVR)
	assert.False(t, cfg.Coverage.Covers(cmResourceGR), "not covered before the first list")

	require.NoError(t, cms.Replace([]any{configMap("a", "uid-a", "v")}, "1"))
	assert.True(t, cfg.Coverage.Covers(cmResourceGR))
	assert.Len(t, sink.objectRecords(), 1, "the list was sent before the resource was covered")

	w.listFailed(cmGVR, nil)
	assert.False(t, cfg.Coverage.Covers(cmResourceGR), "a failing list withdraws coverage")
	require.NoError(t, cms.Replace([]any{configMap("a", "uid-a", "v")}, "2"))
	assert.True(t, cfg.Coverage.Covers(cmResourceGR), "a list that succeeds again restores it")

	require.NoError(t, w.shutdown())
	assert.False(t, cfg.Coverage.Covers(cmResourceGR))
	w.markListed(cmGVR)
	assert.False(t, cfg.Coverage.Covers(cmResourceGR), "a list that completes during shutdown covers nothing")
}

func TestWatcherCoverageWithdrawnWhenKindLeavesDiscovery(t *testing.T) {
	cfg := testConfig()
	cfg.Coverage = NewCoverage()
	disc := newTestDiscovery(coreList(cmResource()), widgetList())
	w, _, store, _ := newTestWatcher(t, cfg, disc, widget("w", "uid-w"))
	stop := runWatcher(t, w)
	require.Eventually(t, func() bool { return cfg.Coverage.Covers(widgetGVR.GroupResource()) }, 5*time.Second, 10*time.Millisecond)

	disc.set(coreList(cmResource()))
	w.syncWatches(context.Background())
	assert.False(t, cfg.Coverage.Covers(widgetGVR.GroupResource()))
	assert.True(t, cfg.Coverage.Covers(cmResourceGR))
	require.NoError(t, stop())
	assert.False(t, cfg.Coverage.Covers(cmResourceGR))
	_, ok := store.Get("uid-w")
	assert.True(t, ok, "the widget was sent and stored")
}

// Across account flag flips, a resource is covered only while a watcher runs and has sent
// its list, and coverage is withdrawn before the stopped watcher's run returns.
func TestCoverageAcrossFlagFlips(t *testing.T) {
	cfg := testConfig()
	cfg.Coverage = NewCoverage()
	dyn := newDynamic(configMap("a", "uid-a", "v1"))
	disc := newTestDiscovery(coreList(cmResource()))
	store := NewStore(newClient(), testNS)
	require.NoError(t, store.Load(context.Background()))
	sink := &recordingSink{}

	returned := make(chan bool, 4)
	run := func(ctx context.Context) error {
		err := NewWatcher(cfg, dyn, disc, store, sink, watcherKey).Run(ctx)
		returned <- cfg.Coverage.Covers(cmResourceGR)
		return err
	}

	flag := newFakeFlag(accountflag.Off)
	g := startGated(t, flag, run)
	time.Sleep(50 * time.Millisecond)
	assert.False(t, cfg.Coverage.Covers(cmResourceGR), "nothing is covered while the flag is off")

	for i := 0; i < 2; i++ {
		flag.set(accountflag.On)
		require.Eventually(t, func() bool { return cfg.Coverage.Covers(cmResourceGR) }, 5*time.Second, 5*time.Millisecond)
		flag.set(accountflag.Off)
		select {
		case covered := <-returned:
			assert.False(t, covered, "coverage was withdrawn before the run returned")
		case <-time.After(5 * time.Second):
			t.Fatal("the watcher did not stop after the flag turned off")
		}
		assert.False(t, cfg.Coverage.Covers(cmResourceGR))
	}
	require.NoError(t, stopGated(t, g))
}
