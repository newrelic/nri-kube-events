package objects

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
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
}

// maxStartDelay bounds the random wait before a run starts after the flag turns on. New
// Relic turns the flag on for an account, and every cluster of the account polls it on its
// own 60 s timer; the wait spreads their first lists over this long, and does the same
// after every off-to-on flip.
const maxStartDelay = 5 * time.Minute

// startDelay returns the wait before a run starts. Tests replace it.
var startDelay = randomStartDelay

// randomStartDelay returns a uniformly random wait in [0, maxStartDelay).
func randomStartDelay() time.Duration {
	return time.Duration(rand.Int64N(int64(maxStartDelay))) //nolint:gosec // jitter, not security
}

// errRunExited is returned, wrapped, when run returns while the flag is on and ctx is live.
var errRunExited = errors.New("objects: watcher returned while the account flag was on")

// RunGated runs run with a child context while flag is On. When the state leaves On it
// cancels that context and waits for run to return before it looks at the state again, so
// two runs never overlap and never write the hash store together. When the state returns
// to On it calls run again; run builds a fresh Watcher over the same Store, so its first
// list sends the objects that changed while off and tombstones the ones that went.
//
// A start after this RunGated has seen the flag Off, which is New Relic turning the flag on
// at run time, waits a random startDelay of up to maxStartDelay first; a flag that leaves
// On during the wait cancels the start. A start with the flag already On when RunGated
// begins, after a pod restart or a carrier retry, does not wait, so the new watcher's
// running heartbeat follows the old pod's stopped one at once.
//
// RunGated returns after ctx is canceled and the running run, if any, has returned, with
// that run's error. A run that returns while ctx is live and the flag is On makes RunGated
// return a non-nil error at once. Every stop resets the objects gauge.
func RunGated(ctx context.Context, flag FlagSource, run func(context.Context) error) error {
	g := &gate{ctx: ctx, flag: flag, run: run}
	defer g.cancelStart()
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
		case <-g.startAt():
			g.startTimer = nil
			if flag.State() == accountflag.On && ctx.Err() == nil {
				log.Info("objects: starting the watcher")
				g.active = startRun(ctx, run)
			}
		case <-flag.Changed():
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
	ctx  context.Context
	flag FlagSource
	run  func(context.Context) error
	// active is the running run, nil while none runs.
	active *gatedRun
	// startTimer fires when a start that waits out its startDelay may run; it is nil while
	// no start waits.
	startTimer *time.Timer
	// seenOff is set once the flag was Off; only a start after that waits.
	seenOff bool
}

// reconcile schedules a start when the flag is on and nothing runs or waits, cancels a
// waiting start when the flag is not on, and stops a running run when the flag is not on.
func (g *gate) reconcile() {
	state := g.flag.State()
	if state == accountflag.Off {
		g.seenOff = true
	}
	switch {
	case state == accountflag.On && g.active == nil && g.startTimer == nil && !g.seenOff:
		log.WithField("state", state.String()).Info("objects: account flag is on, starting the watcher")
		g.active = startRun(g.ctx, g.run)
	case state == accountflag.On && g.active == nil && g.startTimer == nil:
		d := startDelay()
		log.WithFields(log.Fields{"state": state.String(), "delay": d.String()}).Info("objects: account flag is on, starting the watcher after a random delay")
		g.startTimer = time.NewTimer(d)
	case state != accountflag.On && g.startTimer != nil:
		log.WithField("state", state.String()).Info("objects: account flag is not on, canceling the pending start")
		g.cancelStart()
	case state != accountflag.On && g.active != nil:
		log.WithField("state", state.String()).Info("objects: account flag is not on, stopping the watcher")
		if err := g.stop(); err != nil {
			log.WithError(err).Warn("objects: watcher stopped with an error")
		}
	}
}

// startAt receives when a waiting start may run. It is nil, and never receives, while no
// start waits.
func (g *gate) startAt() <-chan time.Time {
	if g.startTimer == nil {
		return nil
	}
	return g.startTimer.C
}

func (g *gate) cancelStart() {
	if g.startTimer != nil {
		g.startTimer.Stop()
		g.startTimer = nil
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
