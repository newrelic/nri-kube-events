package objects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

// counterValue reads c without the testutil package, which would add a module dependency.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

type fakeRequest struct {
	Method string
	Body   []byte
}

// fakeForwarder answers each request with the next status of its list and repeats the last
// one once the list runs out.
type fakeForwarder struct {
	*httptest.Server
	statuses []int
	requests []fakeRequest
	mu       sync.Mutex
}

func newFakeForwarder(t *testing.T, statuses ...int) *fakeForwarder {
	t.Helper()
	f := &fakeForwarder{statuses: statuses}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, fakeRequest{Method: r.Method, Body: body})
		status := f.statuses[min(n, len(f.statuses)-1)]
		f.mu.Unlock()
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			_, _ = w.Write([]byte(strings.Repeat("x", 500)))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeForwarder) all() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeForwarder) only(t *testing.T) fakeRequest {
	t.Helper()
	reqs := f.all()
	require.Len(t, reqs, 1)
	return reqs[0]
}

// newTestSink is a sink for cluster with fast test values.
func newTestSink(t *testing.T, endpoint, cluster string) *InfraSink {
	t.Helper()
	return newTestSinkWith(t, InfraSinkConfig{Endpoint: endpoint, ClusterName: cluster})
}

// newTestSinkWith fills the fields cfg leaves zero with fast test values.
func newTestSinkWith(t *testing.T, cfg InfraSinkConfig) *InfraSink {
	t.Helper()
	if cfg.ClusterName == "" {
		cfg.ClusterName = "c1"
	}
	if cfg.IntegrationVersion == "" {
		cfg.IntegrationVersion = "1.2.3"
	}
	if cfg.MaxRecordsPerSecond == 0 {
		cfg.MaxRecordsPerSecond = 1000
	}
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = time.Millisecond
	}
	s, err := NewInfraSink(cfg)
	require.NoError(t, err)
	return s
}

func rec(summary string) Record {
	return Record{
		Summary:  summary,
		Category: Category,
		Attributes: map[string]any{
			"type":             ObjectType,
			"k8s.cluster.name": "c1",
			"k8s.name":         summary,
		},
	}
}

// sizedRec is a record whose json.Marshal size is n bytes.
func sizedRec(t *testing.T, summary string, n int) Record {
	t.Helper()
	r := rec(summary)
	r.Attributes["object.p000"] = ""
	b, err := json.Marshal(r)
	require.NoError(t, err)
	r.Attributes["object.p000"] = strings.Repeat("a", n-len(b))
	b, err = json.Marshal(r)
	require.NoError(t, err)
	require.Len(t, b, n)
	return r
}

type sentPayload struct {
	Name            string `json:"name"`
	ProtocolVersion string `json:"protocol_version"`
	Data            []struct {
		Entity struct{ Name, Type string } `json:"entity"`
		Events []struct {
			Attributes        map[string]any
			Summary, Category string
		} `json:"events"`
	} `json:"data"`
}

func decode(t *testing.T, body []byte) sentPayload {
	t.Helper()
	var p sentPayload
	require.NoError(t, json.Unmarshal(body, &p))
	return p
}

func TestSendRequestShape(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	s := newTestSink(t, fwd.URL, "c1")
	require.NoError(t, s.Send(context.Background(), []Record{rec("a"), rec("b")}))
	req := fwd.only(t)
	assert.Equal(t, http.MethodPost, req.Method)
	p := decode(t, req.Body)
	assert.Equal(t, "kube_events", p.Name)
	assert.Equal(t, "3", p.ProtocolVersion)
	require.Len(t, p.Data, 1)
	assert.Equal(t, "k8s:c1:objectstate", p.Data[0].Entity.Name)
	assert.Equal(t, "k8s:objectstate", p.Data[0].Entity.Type)
	require.Len(t, p.Data[0].Events, 2)
	ev := p.Data[0].Events[0]
	assert.Equal(t, "a", ev.Summary)
	assert.Equal(t, "kubernetes.objectState", ev.Category)
	assert.Equal(t, "c1", ev.Attributes["clusterName"])
	assert.Equal(t, "c1", ev.Attributes["k8s.cluster.name"])
	assert.Equal(t, "K8sObject", ev.Attributes["type"])
	assert.Equal(t, "kube_events", ev.Attributes["integrationName"])
	assert.Equal(t, "1.2.3", ev.Attributes["integrationVersion"])
	assert.Equal(t, "1.2.3", ev.Attributes["eventRouterVersion"])
}

func TestSendDoesNotMutateRecords(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	s := newTestSink(t, fwd.URL, "c1")
	recs := []Record{rec("a"), rec("b")}
	require.NoError(t, s.Send(context.Background(), recs))
	for _, r := range recs {
		assert.NotContains(t, r.Attributes, "clusterName")
		assert.NotContains(t, r.Attributes, "integrationName")
		assert.Len(t, r.Attributes, 3)
	}
}

func TestSendBatchesBy500AndBytes(t *testing.T) {
	t.Run("event cap", func(t *testing.T) {
		fwd := newFakeForwarder(t, http.StatusNoContent)
		s := newTestSink(t, fwd.URL, "c1")
		recs := make([]Record, 1201)
		for i := range recs {
			recs[i] = rec(fmt.Sprintf("r%d", i))
		}
		require.NoError(t, s.Send(context.Background(), recs))
		reqs := fwd.all()
		require.Len(t, reqs, 3)
		counts := make([]int, 0, len(reqs))
		for _, r := range reqs {
			counts = append(counts, len(decode(t, r.Body).Data[0].Events))
			assert.Less(t, len(r.Body), MaxPostBytes)
		}
		assert.Equal(t, []int{500, 500, 201}, counts)
	})
	for _, size := range []int{400_000, MaxRecordBytes} {
		t.Run(fmt.Sprintf("bytes %d", size), func(t *testing.T) {
			fwd := newFakeForwarder(t, http.StatusNoContent)
			s := newTestSink(t, fwd.URL, "c1")
			recs := []Record{sizedRec(t, "a", size), sizedRec(t, "b", size), sizedRec(t, "c", size)}
			require.NoError(t, s.Send(context.Background(), recs))
			reqs := fwd.all()
			require.Len(t, reqs, 2)
			counts := make([]int, 0, len(reqs))
			for _, r := range reqs {
				counts = append(counts, len(decode(t, r.Body).Data[0].Events))
				assert.Less(t, len(r.Body), MaxPostBytes)
			}
			assert.Equal(t, []int{2, 1}, counts)
		})
	}
}

func TestBatchesMeasuresDecoratedEvents(t *testing.T) {
	// Two records that fit with the reserve before decoration, but not after it.
	n := (MaxPostBytes-envelopeReserve)/2 - 1
	a, b := Record{Summary: "a", Attributes: map[string]any{}}, Record{Summary: "b", Attributes: map[string]any{}}
	pad := func(r Record, size int) Record {
		raw, _ := json.Marshal(r)
		r.Attributes["p"] = strings.Repeat("x", size-len(raw)-len(`"p":""`))
		return r
	}
	a, b = pad(a, n), pad(b, n)
	assert.Len(t, batches([]Record{a, b}, MaxEventsPerPost), 1)

	s := newTestSink(t, "http://127.0.0.1:1", "c1")
	assert.Len(t, batches(s.decorate([]Record{a, b}), MaxEventsPerPost), 2)
}

func TestSendRetries5xxThen204(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusNoContent)
	s := newTestSink(t, fwd.URL, "c1")
	before500 := counterValue(t, metricForwarderRejections.WithLabelValues("500"))
	before503 := counterValue(t, metricForwarderRejections.WithLabelValues("503"))
	require.NoError(t, s.Send(context.Background(), []Record{rec("a")}))
	assert.Len(t, fwd.all(), 3)
	assert.Equal(t, 1.0, counterValue(t, metricForwarderRejections.WithLabelValues("500"))-before500)
	assert.Equal(t, 1.0, counterValue(t, metricForwarderRejections.WithLabelValues("503"))-before503)
}

// refuseFirst returns a connection-refused error on its first round trip and forwards
// every later one.
type refuseFirst struct {
	mu    sync.Mutex
	calls int
}

func (r *refuseFirst) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestSendRetriesTransportErrorThenSucceeds(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	s := newTestSinkWith(t, InfraSinkConfig{
		Endpoint:   fwd.URL,
		HTTPClient: &http.Client{Transport: &refuseFirst{}},
	})
	before := counterValue(t, metricForwarderRejections.WithLabelValues("transport"))
	require.NoError(t, s.Send(context.Background(), []Record{rec("a")}))
	assert.Len(t, fwd.all(), 1)
	assert.Equal(t, 1.0, counterValue(t, metricForwarderRejections.WithLabelValues("transport"))-before)
}

func TestSendGivesUpAfterBackoffCap(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusInternalServerError)
	s := newTestSinkWith(t, InfraSinkConfig{Endpoint: fwd.URL, MaxBackoff: 20 * time.Millisecond})
	err := s.Send(context.Background(), []Record{rec("a")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
	assert.LessOrEqual(t, len(err.Error()), 400, "the error quotes at most 200 body characters")
	n := len(fwd.all())
	assert.Greater(t, n, 1)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, fwd.all(), n, "no requests after Send returned")
}

func TestSendStopsOnContextCancel(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusInternalServerError)
	s := newTestSinkWith(t, InfraSinkConfig{Endpoint: fwd.URL, InitialBackoff: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, []Record{rec("a")}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), err.Error())
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Send did not return within 100ms of cancel")
	}
}

func TestSendRateLimited(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	// The limiter's bucket starts filling when the sink is built, so start the clock first.
	start := time.Now()
	s := newTestSinkWith(t, InfraSinkConfig{Endpoint: fwd.URL, MaxRecordsPerSecond: 20})
	for i := 0; i < 60; i++ {
		require.NoError(t, s.Send(context.Background(), []Record{rec("a")}))
	}
	took := time.Since(start)
	assert.GreaterOrEqual(t, took, 2*time.Second)
	assert.Less(t, took, 4*time.Second)
}

func TestSendCountsAcceptedRecordsAndBytes(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	s := newTestSink(t, fwd.URL, "c1")
	records, bytesSent := counterValue(t, metricRecordsSent), counterValue(t, metricBytesSent)
	require.NoError(t, s.Send(context.Background(), []Record{rec("a"), rec("b")}))
	req := fwd.only(t)
	assert.Equal(t, 2.0, counterValue(t, metricRecordsSent)-records)
	assert.Equal(t, float64(len(req.Body)), counterValue(t, metricBytesSent)-bytesSent)
}

func TestSendDoesNotCountRejectedRecords(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusInternalServerError)
	s := newTestSinkWith(t, InfraSinkConfig{Endpoint: fwd.URL, MaxBackoff: 5 * time.Millisecond})
	before := counterValue(t, metricRecordsSent)
	require.Error(t, s.Send(context.Background(), []Record{rec("a")}))
	assert.Equal(t, before, counterValue(t, metricRecordsSent))
}

// eventsSinkOnce guards sinks.Create, whose SDK flags and promauto metrics can be defined
// once per process.
var eventsSinkOnce sync.Once

func TestTwoSinksAndEventsSinkCoexist(t *testing.T) {
	var err error
	eventsSinkOnce.Do(func() {
		_, err = sinks.Create([]sinks.SinkConfig{{
			Name:   "newRelicInfra",
			Config: map[string]string{"clusterName": "c1", "agentEndpoint": "http://127.0.0.1:1/v1/data"},
		}}, "1.2.3")
	})
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		for i := 0; i < 2; i++ {
			_, err := NewInfraSink(InfraSinkConfig{
				Endpoint:            "http://127.0.0.1:1/v1/data",
				ClusterName:         "c1",
				IntegrationVersion:  "1.2.3",
				MaxRecordsPerSecond: 10,
			})
			require.NoError(t, err)
		}
	})
}

func TestConcurrentSendsKeepPayloadsSeparate(t *testing.T) {
	fwd := newFakeForwarder(t, http.StatusNoContent)
	s := newTestSink(t, fwd.URL, "c2")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assert.NoError(t, s.Send(context.Background(), []Record{rec(fmt.Sprintf("s%d", i))}))
		}(i)
	}
	wg.Wait()
	reqs := fwd.all()
	require.Len(t, reqs, 20)
	seen := map[string]bool{}
	for _, r := range reqs {
		p := decode(t, r.Body)
		require.Len(t, p.Data, 1)
		require.Len(t, p.Data[0].Events, 1)
		ev := p.Data[0].Events[0]
		assert.Equal(t, ev.Summary, ev.Attributes["k8s.name"])
		assert.Equal(t, "c2", ev.Attributes["clusterName"])
		assert.Equal(t, "k8s:c2:objectstate", p.Data[0].Entity.Name)
		seen[ev.Summary] = true
	}
	assert.Len(t, seen, 20)
}

func TestNewInfraSinkValidatesConfig(t *testing.T) {
	ok := InfraSinkConfig{Endpoint: "http://x/v1/data", ClusterName: "c1", IntegrationVersion: "1", MaxRecordsPerSecond: 1}
	_, err := NewInfraSink(ok)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*InfraSinkConfig){
		"endpoint": func(c *InfraSinkConfig) { c.Endpoint = "" },
		"cluster":  func(c *InfraSinkConfig) { c.ClusterName = "" },
		"version":  func(c *InfraSinkConfig) { c.IntegrationVersion = "" },
		"rate":     func(c *InfraSinkConfig) { c.MaxRecordsPerSecond = 0 },
	} {
		cfg := ok
		mutate(&cfg)
		_, err := NewInfraSink(cfg)
		assert.Error(t, err, name)
	}
}
