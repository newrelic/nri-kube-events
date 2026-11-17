// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServePrometheusStopDoesNotExit checks that closing stopChan shuts the metrics server
// down without exiting the process, so main's wg.Wait lets the objects watcher run its
// final hash-store flush.
func TestServePrometheusStopDoesNotExit(t *testing.T) {
	logger := logrus.StandardLogger()
	oldExit := logger.ExitFunc
	var exited atomic.Bool
	logger.ExitFunc = func(int) { exited.Store(true) }
	t.Cleanup(func() { logger.ExitFunc = oldExit })

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		servePrometheus("127.0.0.1:0", stop)
	}()
	// Give ListenAndServe time to start before the stop arrives.
	time.Sleep(100 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("servePrometheus did not return after stop")
	}
	assert.Never(t, exited.Load, 500*time.Millisecond, 10*time.Millisecond, "servePrometheus exited the process on shutdown")
}

// TestWatcherExitBeforeStopIsFatal checks that the process exits, so the pod restarts, when
// the objects watcher returns while the stop signal has not fired, and only logs when it
// returns after the stop.
func TestWatcherExitBeforeStopIsFatal(t *testing.T) {
	logger := logrus.StandardLogger()
	oldExit := logger.ExitFunc
	var exited atomic.Bool
	logger.ExitFunc = func(int) { exited.Store(true) }
	t.Cleanup(func() { logger.ExitFunc = oldExit })

	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		ctx   context.Context
		err   error
		name  string
		fatal bool
	}{
		{live, errors.New("boom"), "error before stop", true},
		{live, nil, "nil before stop", true},
		{stopped, errors.New("flush failed"), "error after stop", false},
		{stopped, nil, "nil after stop", false},
	} {
		exited.Store(false)
		watcherExited(tc.ctx, tc.err)
		assert.Equal(t, tc.fatal, exited.Load(), tc.name)
	}
}

func TestServeErrorTreatsServerClosedAsShutdown(t *testing.T) {
	assert.NoError(t, serveError("127.0.0.1:8080", http.ErrServerClosed))

	err := serveError("127.0.0.1:8080", errors.New("address already in use"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "address already in use")
}

func TestMaxResendsPerScan(t *testing.T) {
	// The default 100 records/s is above half the client QPS, 25/s: 25 × 60 s.
	assert.Equal(t, 1500, maxResendsPerScan(100))
	assert.Equal(t, 600, maxResendsPerScan(10))
}
