package objects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkEvent "github.com/newrelic/infra-integrations-sdk/data/event"
	sdkIntegration "github.com/newrelic/infra-integrations-sdk/integration"
	"golang.org/x/time/rate"
)

const (
	// MaxEventsPerPost is the most events one POST to the forwarder carries.
	MaxEventsPerPost = 500
	// MaxPostBytes bounds the body of one POST to the forwarder; every body is smaller.
	MaxPostBytes = 1_000_000
	// EntityType is the type of the entity every record and heartbeat attaches to.
	EntityType = "k8s:objectstate"

	// sdkName is the integration name, the same as the events sink's.
	sdkName = "kube_events"
	// envelopeReserve is the room batches keeps for the payload around the events.
	envelopeReserve = 4096
	// errBodyChars is the most response-body characters an error message quotes.
	errBodyChars = 200
	// maxDrainBytes is the most response-body bytes read after the quoted part, so the
	// connection can be reused.
	maxDrainBytes = 64 << 10
	// decoratedAttrs is the number of attributes decorate adds to every event.
	decoratedAttrs = 4
	// backoffFactor multiplies the retry wait after every failed attempt.
	backoffFactor = 2

	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 5 * time.Minute
	defaultHTTPTimeout    = 60 * time.Second
)

// EntityName is the name of the entity that the records of cluster attach to.
func EntityName(cluster string) string {
	return "k8s:" + cluster + ":objectstate"
}

// RecordSink receives the records of one object version, or one tombstone or heartbeat.
type RecordSink interface {
	Send(ctx context.Context, recs []Record) error
}

var _ RecordSink = (*InfraSink)(nil)

// InfraSinkConfig configures an InfraSink.
type InfraSinkConfig struct {
	// HTTPClient is copied; nil means a client with a 60 s timeout.
	HTTPClient *http.Client
	// Endpoint is the forwarder's /v1/data URL.
	Endpoint string
	// ClusterName is added to every event as clusterName and names the entity.
	ClusterName string
	// IntegrationVersion is the SDK integration version and the integrationVersion and
	// eventRouterVersion attributes.
	IntegrationVersion string
	// MaxRecordsPerSecond caps the records the sink posts per second.
	MaxRecordsPerSecond float64
	// InitialBackoff is the first retry wait; zero means 1 s.
	InitialBackoff time.Duration
	// MaxBackoff caps each retry wait and the summed waits of one batch; zero means 5 min.
	MaxBackoff time.Duration
}

// InfraSink posts records as SDK v3 integration payloads to the infrastructure agent
// forwarder. It is safe for concurrent use.
type InfraSink struct {
	client  *http.Client
	limiter *rate.Limiter
	// sdk holds the entities of one payload between Entity and Clear; mu guards it.
	sdk      *sdkIntegration.Integration
	cfg      InfraSinkConfig
	mu       sync.Mutex
	maxBatch int
}

// NewInfraSink returns a sink for cfg. Its SDK integration has no Args option, so it defines
// no flags, and keeps its store in memory.
func NewInfraSink(cfg InfraSinkConfig) (*InfraSink, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, errors.New("infra sink: endpoint is required")
	case cfg.ClusterName == "":
		return nil, errors.New("infra sink: cluster name is required")
	case cfg.MaxRecordsPerSecond <= 0:
		return nil, fmt.Errorf("infra sink: max records per second must be positive, got %v", cfg.MaxRecordsPerSecond)
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = defaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	sdk, err := sdkIntegration.New(sdkName, cfg.IntegrationVersion, sdkIntegration.InMemoryStore())
	if err != nil {
		return nil, fmt.Errorf("infra sink: initializing SDK integration: %w", err)
	}

	c := http.Client{Timeout: defaultHTTPTimeout}
	if cfg.HTTPClient != nil {
		c = *cfg.HTTPClient
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	burst := max(1, int(cfg.MaxRecordsPerSecond))
	return &InfraSink{
		cfg:      cfg,
		client:   &c,
		limiter:  rate.NewLimiter(rate.Limit(cfg.MaxRecordsPerSecond), burst),
		maxBatch: min(MaxEventsPerPost, burst),
		sdk:      sdk,
	}, nil
}

// Send posts recs in batches, in order. It returns nil only after every batch got HTTP 204,
// and stops at the first batch that fails. It does not modify recs.
func (s *InfraSink) Send(ctx context.Context, recs []Record) error {
	bs := batches(s.decorate(recs), s.maxBatch)
	for i, b := range bs {
		if err := s.sendBatch(ctx, b); err != nil {
			return fmt.Errorf("forwarder batch %d of %d (%d records): %w", i+1, len(bs), len(b), err)
		}
	}
	return nil
}

// payload is the SDK v3 JSON for one batch of recs.
func (s *InfraSink) payload(recs []Record) ([]byte, error) {
	return s.encode(s.decorate(recs))
}

// decorate returns the records as this sink posts them; see Decorate.
func (s *InfraSink) decorate(recs []Record) []Record {
	return Decorate(recs, s.cfg.ClusterName, s.cfg.IntegrationVersion)
}

// Decorate returns copies of recs with the attributes the events sink adds to every event
// (decorateAttrs in pkg/sinks/new_relic_infra.go). InfraSink posts these copies, so the
// json.Marshal size of a decorated record is the size of the event it sends.
func Decorate(recs []Record, cluster, integrationVersion string) []Record {
	out := make([]Record, len(recs))
	for i, r := range recs {
		attrs := make(map[string]any, len(r.Attributes)+decoratedAttrs)
		for k, v := range r.Attributes {
			attrs[k] = v
		}
		attrs["clusterName"] = cluster
		attrs["integrationName"] = sdkName
		attrs["integrationVersion"] = integrationVersion
		attrs["eventRouterVersion"] = integrationVersion
		out[i] = Record{Summary: r.Summary, Category: r.Category, Attributes: attrs}
	}
	return out
}

// encode marshals already decorated records as one integration payload.
func (s *InfraSink) encode(recs []Record) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.sdk.Clear()

	e, err := s.sdk.Entity(EntityName(s.cfg.ClusterName), EntityType)
	if err != nil {
		return nil, fmt.Errorf("creating entity: %w", err)
	}
	for _, r := range recs {
		if err = e.AddEvent(sdkEvent.NewWithAttributes(r.Summary, r.Category, r.Attributes)); err != nil {
			return nil, fmt.Errorf("adding event %q: %w", r.Summary, err)
		}
	}
	body, err := json.Marshal(s.sdk)
	if err != nil {
		return nil, fmt.Errorf("marshaling payload: %w", err)
	}
	if len(body) >= MaxPostBytes {
		return nil, fmt.Errorf("payload of %d events is %d bytes, the limit is under %d", len(recs), len(body), MaxPostBytes)
	}
	return body, nil
}

// batches splits recs, in order, into groups of at most maxEvents whose summed json.Marshal
// sizes, commas between them and envelopeReserve stay under MaxPostBytes. A record is never
// split; MaxRecordBytes keeps any single record under the limit.
func batches(recs []Record, maxEvents int) [][]Record {
	var out [][]Record
	cur := make([]Record, 0, min(len(recs), maxEvents))
	size := 0
	for _, r := range recs {
		n := recordSize(r)
		if len(cur) > 0 {
			n++ // the comma before this event
		}
		if len(cur) > 0 && (len(cur) == maxEvents || size+n+envelopeReserve >= MaxPostBytes) {
			out = append(out, cur)
			cur, size = make([]Record, 0, min(len(recs), maxEvents)), 0
			n--
		}
		cur = append(cur, r)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// recordSize is the marshaled size of r. A record that fails to marshal counts as filling a
// batch, so it travels alone and encode reports the error.
func recordSize(r Record) int {
	b, err := json.Marshal(r)
	if err != nil {
		return MaxPostBytes
	}
	return len(b)
}

// sendBatch waits for the rate limiter, then posts one batch of decorated records, retrying
// every failure with exponential backoff until the summed waits would pass MaxBackoff.
func (s *InfraSink) sendBatch(ctx context.Context, recs []Record) error {
	body, err := s.encode(recs)
	if err != nil {
		return err
	}
	if err := s.limiter.WaitN(ctx, len(recs)); err != nil {
		return fmt.Errorf("rate limit: %w", err)
	}
	wait := s.cfg.InitialBackoff
	var waited time.Duration
	for attempt := 1; ; attempt++ {
		err := s.post(ctx, body)
		if err == nil {
			metricRecordsSent.Add(float64(len(recs)))
			metricBytesSent.Add(float64(len(body)))
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("attempt %d: %w (last error: %w)", attempt, ctx.Err(), err)
		}
		delay := min(wait, s.cfg.MaxBackoff)
		if waited+delay > s.cfg.MaxBackoff {
			return fmt.Errorf("giving up after %d attempts over %s: %w", attempt, waited, err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("attempt %d: %w (last error: %w)", attempt, ctx.Err(), err)
		case <-t.C:
		}
		waited += delay
		wait = min(wait*backoffFactor, s.cfg.MaxBackoff)
	}
}

// post makes one attempt. It returns nil on HTTP 204. Any other status or a transport error
// increments forwarder_rejections_total, unless ctx ended the attempt.
func (s *InfraSink) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			metricForwarderRejections.WithLabelValues("transport").Inc()
		}
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyChars))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	metricForwarderRejections.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()
	return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
}
