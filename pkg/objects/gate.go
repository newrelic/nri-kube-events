package objects

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
)

// FlagSource reports the account collection flag. *accountflag.Poller satisfies it.
type FlagSource interface {
	// State returns the cached state without blocking.
	State() accountflag.State
	// Changed receives after a state change. Changes may collapse into one receive.
	Changed() <-chan struct{}
	// Polls returns the number of successful polls so far.
	Polls() uint64
	// OnSincePoll returns the number of the poll that last turned the state On.
	OnSincePoll() uint64
	// Polled receives after a successful poll. Polls may collapse into one receive.
	Polled() <-chan struct{}
}

// restartWindow is the least time between two starts of the watcher once the flag has
// been off: a flag that flaps off and on within it starts the watcher again only when the
// window ends, if the flag is still on. Tests shorten it.
var restartWindow = 5 * time.Minute

// errRunExited is returned, wrapped, when run returns while the flag is on and ctx is live.
var errRunExited = errors.New("objects: watcher returned while the account flag was on")

// RunGated runs run with a child context while flag is On. When the state leaves On it
// cancels that context at once and waits for run to return before it looks at the state
// again, so two runs never overlap and never write the hash store together. When the state
// returns to On it calls run again; run builds a fresh Watcher over the same Store, so its
// first list sends the objects that changed while off and tombstones the ones that went.
//
// Starting is debounced once this RunGated has seen the flag Off, which is New Relic
// turning the flag on at run time: the watcher starts only after a second poll in a row
// reads On, about one poll interval (60 s) after the first, and at least restartWindow
// after the previous start. A flag that leaves On while a start waits cancels it. A start
// with the flag On before any Off, after a pod restart or a carrier retry, happens at the
// first poll that reads On, so the new watcher's running heartbeat follows the old pod's
// stopped one at once.
//
// RunGated returns after ctx is canceled and the running run, if any, has returned, with
// that run's error. A run that returns while ctx is live and the flag is On makes RunGated
// return a non-nil error at once. Every stop resets the objects gauge.
func RunGated(ctx context.Context, flag FlagSource, run func(context.Context) error) error {
	g := &gate{ctx: ctx, flag: flag, run: run, now: time.Now}
	defer g.cancelWindow()
	log.WithField("state", flag.State().String()).Info("objects: the watcher runs only while the account flag is on")
	for {
		if ctx.Err() == nil {
			g.reconcile()
		}
		select {
		case <-ctx.Done():
			if g.active == nil {
				return nil
			}
			return g.stop()
		case <-g.windowC():
			g.windowTimer = nil
			if flag.State() == accountflag.On && g.active == nil && ctx.Err() == nil {
				g.start("objects: restart window ended and the account flag is still on, starting the watcher")
			}
		case <-flag.Changed():
		case <-flag.Polled():
		case err := <-g.active.doneC():
			g.active.cancel()
			g.active = nil
			setObjectsMetric(nil)
			if ctx.Err() != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("%w: %w", errRunExited, err)
			}
			return errRunExited
		}
	}
}

// gate is the state of one RunGated call.
type gate struct {
	lastStart time.Time
	ctx       context.Context
	flag      FlagSource
	run       func(context.Context) error
	now       func() time.Time
	// active is the running run, nil while none runs.
	active *gatedRun
	// windowTimer fires when a confirmed start may run after restartWindow; it is nil while
	// no start waits for the window.
	windowTimer *time.Timer
	// confirming is set while a start waits for a second poll in a row that reads On.
	confirming bool
	// seenOff is set once the flag was Off; only a start after that is debounced.
	seenOff bool
}

// reconcile stops a running run, and cancels a waiting start, when the flag is not on.
// When the flag is on and nothing runs or waits, it starts the run: at once before the
// flag was ever off, and after that once a second poll in a row reads On and the restart
// window since the last start has passed.
func (g *gate) reconcile() {
	state := g.flag.State()
	if state != accountflag.On {
		g.flagNotOn(state)
		return
	}
	if g.active != nil || g.windowTimer != nil {
		return
	}
	if !g.seenOff {
		g.start("objects: account flag is on, starting the watcher")
		return
	}
	if g.flag.Polls() <= g.flag.OnSincePoll() {
		if !g.confirming {
			log.Info("objects: account flag turned on; starting the watcher once the next poll also reads on")
		}
		g.confirming = true
		return
	}
	g.confirming = false
	if wait := g.lastStart.Add(restartWindow).Sub(g.now()); !g.lastStart.IsZero() && wait > 0 {
		log.WithField("wait", wait.Round(time.Second).String()).Info("objects: account flag is on again; starting the watcher when the restart window ends")
		g.windowTimer = time.NewTimer(wait)
		return
	}
	g.start("objects: account flag read on in two polls in a row, starting the watcher")
}

// flagNotOn cancels a waiting start and stops a running run at once.
func (g *gate) flagNotOn(state accountflag.State) {
	if state == accountflag.Off {
		g.seenOff = true
	}
	if g.confirming || g.windowTimer != nil {
		log.WithField("state", state.String()).Info("objects: account flag is not on, canceling the pending start")
	}
	g.confirming = false
	g.cancelWindow()
	if g.active == nil {
		return
	}
	log.WithField("state", state.String()).Info("objects: account flag is not on, stopping the watcher")
	if err := g.stop(); err != nil {
		log.WithError(err).Warn("objects: watcher stopped with an error")
	}
}

func (g *gate) start(msg string) {
	log.Info(msg)
	g.active = startRun(g.ctx, g.run)
	g.lastStart = g.now()
}

// windowC receives when the restart window of a waiting start ends. It is nil, and never
// receives, while no start waits.
func (g *gate) windowC() <-chan time.Time {
	if g.windowTimer == nil {
		return nil
	}
	return g.windowTimer.C
}

func (g *gate) cancelWindow() {
	if g.windowTimer != nil {
		g.windowTimer.Stop()
		g.windowTimer = nil
	}
}

// stop stops the running run, resets the objects gauge and returns the run's error.
func (g *gate) stop() error {
	err := g.active.stop()
	g.active = nil
	setObjectsMetric(nil)
	return err
}

// gatedRun is one call of run in its own goroutine.
type gatedRun struct {
	cancel context.CancelFunc
	done   chan error
}

func startRun(ctx context.Context, run func(context.Context) error) *gatedRun {
	rctx, cancel := context.WithCancel(ctx)
	r := &gatedRun{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- run(rctx) }()
	return r
}

// stop cancels the run and returns its error once it has returned.
func (r *gatedRun) stop() error {
	r.cancel()
	return <-r.done
}

// doneC receives run's error. It is nil, and never receives, on a nil *gatedRun.
func (r *gatedRun) doneC() <-chan error {
	if r == nil {
		return nil
	}
	return r.done
}
