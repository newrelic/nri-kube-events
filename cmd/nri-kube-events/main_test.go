// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/newrelic/nri-kube-events/pkg/objects"
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

// noExit makes a logrus Fatal record the exit instead of exiting, for the test's duration.
func noExit(t *testing.T) *atomic.Bool {
	t.Helper()
	logger := logrus.StandardLogger()
	oldExit := logger.ExitFunc
	var exited atomic.Bool
	logger.ExitFunc = func(int) { exited.Store(true) }
	t.Cleanup(func() { logger.ExitFunc = oldExit })
	return &exited
}

// freshObjectStateRegistry points the objects metrics at a new registry for the test's
// duration, so no test registers them globally.
func freshObjectStateRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	r := prometheus.NewRegistry()
	old := objectStateRegisterer
	objectStateRegisterer = r
	t.Cleanup(func() { objectStateRegisterer = old })
	return r
}

// carrierFailures returns nri_kube_events_objects_carrier_failures_total per stage in r.
func carrierFailures(t *testing.T, r prometheus.Gatherer) map[string]float64 {
	t.Helper()
	mfs, err := r.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "nri_kube_events_objects_carrier_failures_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[m.GetLabel()[0].GetValue()] = m.GetCounter().GetValue()
		}
	}
	return out
}

func objectsFamilies(t *testing.T, g prometheus.Gatherer) []string {
	t.Helper()
	mfs, err := g.Gather()
	require.NoError(t, err)
	var names []string
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "nri_kube_events_objects_") {
			names = append(names, mf.GetName())
		}
	}
	return names
}

// failureDelta returns the growth of carrier_failures_total per stage since before.
func failureDelta(t *testing.T, r prometheus.Gatherer, before map[string]float64) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for stage, v := range carrierFailures(t, r) {
		if d := v - before[stage]; d != 0 {
			out[stage] = d
		}
	}
	return out
}

// A failed attempt is retried after the backoff, which doubles up to its max; failures are
// counted per stage, and nothing exits the process.
func TestRetryObjectStateRetriesWithBackoff(t *testing.T) {
	exited := noExit(t)
	r := freshObjectStateRegistry(t)
	require.NoError(t, objects.RegisterMetrics(r))
	before := carrierFailures(t, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var calls []time.Time
	fourth := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		retryObjectState(ctx, retryPolicy{initial: 20 * time.Millisecond, max: 40 * time.Millisecond}, func(ctx context.Context) (string, error) {
			mu.Lock()
			calls = append(calls, time.Now())
			n := len(calls)
			mu.Unlock()
			switch n {
			case 1, 2:
				return objects.StageStartup, errors.New("hmac secret: forbidden")
			case 3:
				return objects.StageRun, nil // a run that ends before the stop signal
			default:
				close(fourth)
				<-ctx.Done()
				return objects.StageRun, nil
			}
		})
	}()
	select {
	case <-fourth:
	case <-time.After(5 * time.Second):
		t.Fatal("no fourth attempt")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retryObjectState did not return after the stop")
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 4)
	assert.Equal(t, map[string]float64{objects.StageStartup: 2, objects.StageRun: 1}, failureDelta(t, r, before))
	assert.GreaterOrEqual(t, calls[1].Sub(calls[0]), 20*time.Millisecond)
	assert.GreaterOrEqual(t, calls[2].Sub(calls[1]), 40*time.Millisecond)
	assert.GreaterOrEqual(t, calls[3].Sub(calls[2]), 40*time.Millisecond, "the backoff stops at max")
	assert.False(t, exited.Load())
}

// An attempt that returns after the stop is the shutdown; it is not counted or retried.
func TestRetryObjectStateStopsOnCancel(t *testing.T) {
	r := freshObjectStateRegistry(t)
	require.NoError(t, objects.RegisterMetrics(r))
	before := carrierFailures(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	retryObjectState(ctx, retryPolicy{initial: time.Millisecond, max: time.Millisecond}, func(context.Context) (string, error) {
		calls++
		return objects.StageRun, errors.New("flushing hash store on shutdown")
	})
	assert.Equal(t, 1, calls)
	assert.Empty(t, failureDelta(t, r, before))
}

// startObjectState returns at once and never exits the process, whether its config is
// invalid or its startup keeps failing, here because the kubeconfig does not exist. The
// stop signal ends the retries.
func TestStartObjectStateFailuresDoNotStopTheProcess(t *testing.T) {
	exited := noExit(t)
	// g reads the counters; each startObjectState call registers in a fresh registry of its
	// own, as a process registers once.
	g := prometheus.NewRegistry()
	require.NoError(t, objects.RegisterMetrics(g))
	oldRetry, oldKubeConfig := objectStateRetry, *kubeConfig
	objectStateRetry = retryPolicy{initial: 10 * time.Millisecond, max: 10 * time.Millisecond}
	*kubeConfig = t.TempDir() + "/missing-kubeconfig"
	t.Cleanup(func() { objectStateRetry, *kubeConfig = oldRetry, oldKubeConfig })

	// With the license key set, startup gets as far as the missing kubeconfig.
	t.Setenv(licenseKeyEnv, "test-license-key")
	oc := ObjectStateConfig{
		Enabled: true, ClusterName: "c", Namespace: "ns",
		AgentEndpoint: "http://localhost:8001/v1/data",
	}
	invalid := oc
	invalid.ClusterName = ""

	stop := make(chan struct{})
	wg := &sync.WaitGroup{}
	start := func(oc ObjectStateConfig) {
		t.Helper()
		freshObjectStateRegistry(t)
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			startObjectState(oc, nil, stop, wg)
		}()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("startObjectState blocked")
		}
	}

	before := carrierFailures(t, g)
	start(invalid)
	assert.Equal(t, map[string]float64{objects.StageConfig: 1}, failureDelta(t, g, before))

	before = carrierFailures(t, g)
	start(oc)
	require.Eventually(t, func() bool { return failureDelta(t, g, before)[objects.StageStartup] >= 3 }, 5*time.Second, 5*time.Millisecond)

	close(stop)
	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the carrier did not stop after the stop signal")
	}
	assert.False(t, exited.Load())
}

// No nri_kube_events_objects_ family is registered until startObjectState runs, in the
// default registry or in the one the carrier uses. startObjectState registers them even
// when its config is invalid and it starts nothing.
func TestObjectStateMetricsRegisterOnlyWhenStarted(t *testing.T) {
	r := freshObjectStateRegistry(t)
	stop := make(chan struct{})
	defer close(stop)
	assert.Empty(t, objectsFamilies(t, r))
	assert.Empty(t, objectsFamilies(t, prometheus.DefaultGatherer))

	startObjectState(ObjectStateConfig{Enabled: true}, nil, stop, &sync.WaitGroup{})
	assert.Contains(t, objectsFamilies(t, r), "nri_kube_events_objects_carrier_failures_total")
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

// The describe path skips covered resources only with describe capture on and the carrier
// enabled.
func TestDescribeCoverage(t *testing.T) {
	on := ObjectStateConfig{Enabled: true}
	assert.NotNil(t, describeCoverage(true, on))
	assert.Nil(t, describeCoverage(false, on), "describe capture off")
	assert.Nil(t, describeCoverage(true, ObjectStateConfig{}), "carrier off")
}

// A carrier that never starts covers nothing, so the describe path keeps describing.
func TestFailedCarrierCoversNothing(t *testing.T) {
	oldRetry, oldKubeConfig := objectStateRetry, *kubeConfig
	objectStateRetry = retryPolicy{initial: 10 * time.Millisecond, max: 10 * time.Millisecond}
	*kubeConfig = t.TempDir() + "/missing-kubeconfig"
	t.Cleanup(func() { objectStateRetry, *kubeConfig = oldRetry, oldKubeConfig })
	g := freshObjectStateRegistry(t)

	// With the license key set, startup gets as far as the missing kubeconfig.
	t.Setenv(licenseKeyEnv, "test-license-key")
	oc := ObjectStateConfig{
		Enabled: true, ClusterName: "c", Namespace: "ns",
		AgentEndpoint: "http://localhost:8001/v1/data",
	}
	coverage := describeCoverage(true, oc)
	require.NotNil(t, coverage)
	stop := make(chan struct{})
	wg := &sync.WaitGroup{}
	before := carrierFailures(t, g)
	startObjectState(oc, coverage, stop, wg)
	require.Eventually(t, func() bool { return failureDelta(t, g, before)[objects.StageStartup] >= 2 }, 5*time.Second, 5*time.Millisecond)
	assert.False(t, coverage.Covers(schema.GroupResource{Resource: "pods"}))
	close(stop)
	wg.Wait()
}
