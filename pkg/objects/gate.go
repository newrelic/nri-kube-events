package objects

import (
	"context"
	"errors"
	"fmt"

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

// errRunExited is returned, wrapped, when run returns while the flag is on and ctx is live.
var errRunExited = errors.New("objects: watcher returned while the account flag was on")

// RunGated runs run with a child context while flag is On. When the state leaves On it
// cancels that context and waits for run to return before it looks at the state again, so
// two runs never overlap and never write the hash store together. When the state returns
// to On it calls run again; run builds a fresh Watcher over the same Store, so its first
// list sends the objects that changed while off and tombstones the ones that went.
//
// RunGated returns after ctx is canceled and the running run, if any, has returned, with
// that run's error. A run that returns while ctx is live and the flag is On makes RunGated
// return a non-nil error at once. Every stop resets the objects gauge.
func RunGated(ctx context.Context, flag FlagSource, run func(context.Context) error) error {
	// active is the running run, nil while none runs.
	var active *gatedRun
	stop := func() error {
		err := active.stop()
		active = nil
		setObjectsMetric(nil)
		return err
	}

	log.WithField("state", flag.State().String()).Info("objects: the watcher runs only while the account flag is on")
	for {
		if ctx.Err() == nil {
			state := flag.State()
			switch {
			case state == accountflag.On && active == nil:
				log.WithField("state", state.String()).Info("objects: account flag is on, starting the watcher")
				active = startRun(ctx, run)
			case state != accountflag.On && active != nil:
				log.WithField("state", state.String()).Info("objects: account flag is not on, stopping the watcher")
				if err := stop(); err != nil {
					log.WithError(err).Warn("objects: watcher stopped with an error")
				}
			}
		}

		select {
		case <-ctx.Done():
			if active == nil {
				return nil
			}
			return stop()
		case <-flag.Changed():
		case err := <-active.doneC():
			active.cancel()
			active = nil
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
