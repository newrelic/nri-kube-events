// Package accountflag polls the infrastructure command API for the per-account flag that
// turns object-state collection on.
//
// The request matches the infrastructure agent's (pkg/backend/commandapi/client.go at
// 3346a15): GET with X-License-Key and X-NRI-Agent-Entity-Id. The carrier has no agent
// entity, so it always sends 0, and it never acks a command.
package accountflag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// State is the collection flag as the poller last learned it.
type State int

const (
	// Unknown means no poll has succeeded yet. Callers treat it as off.
	Unknown State = iota
	Off
	On
)

func (s State) String() string {
	switch s {
	case Unknown:
		return "unknown"
	case Off:
		return "off"
	case On:
		return "on"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

const (
	// Category and Flag name the set_feature_flag command that carries the collection flag.
	Category = "Infra_Agent"
	Flag     = "k8s_object_state_enabled"

	DefaultInterval       = 60 * time.Second
	DefaultInitialBackoff = 10 * time.Second
	DefaultMaxBackoff     = 5 * time.Minute

	licenseHeader  = "X-License-Key"
	entityIDHeader = "X-NRI-Agent-Entity-Id"
	setFlagCommand = "set_feature_flag"

	backoffFactor  = 2
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 4 << 20
)

// Config configures a Poller. URL and LicenseKey are required. Zero durations take the
// defaults. Proxy, when set, is the proxy URL for a client the poller builds; it cannot be
// combined with HTTPClient, which the caller configures itself.
type Config struct {
	HTTPClient     *http.Client
	URL            string
	LicenseKey     string
	UserAgent      string
	Proxy          string
	Interval       time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Poller polls the command API and holds the last known flag state.
type Poller struct {
	client  *http.Client
	changed chan struct{}
	// after returns the channel Run waits on between polls. Tests replace it.
	after func(time.Duration) <-chan time.Time
	cfg   Config
	state atomic.Int32
}

// NewPoller validates cfg and returns a poller in state Unknown.
func NewPoller(cfg Config) (*Poller, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("accountflag: URL must be an absolute URL")
	}
	if cfg.LicenseKey == "" {
		return nil, errors.New("accountflag: license key is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = DefaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	cfg.MaxBackoff = max(cfg.MaxBackoff, cfg.InitialBackoff)

	client, err := buildClient(cfg)
	if err != nil {
		return nil, err
	}
	p := &Poller{
		cfg:     cfg,
		client:  client,
		changed: make(chan struct{}, 1),
		after:   time.After,
	}
	setStateMetric(Unknown)
	return p, nil
}

// buildClient returns the client the poller uses. Redirects are never followed, because
// Go forwards custom headers such as X-License-Key to the redirect target.
func buildClient(cfg Config) (*http.Client, error) {
	if cfg.HTTPClient != nil {
		if cfg.Proxy != "" {
			return nil, errors.New("accountflag: set Proxy or HTTPClient, not both")
		}
		c := *cfg.HTTPClient
		c.CheckRedirect = noRedirect
		return &c, nil
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("accountflag: default transport is not an *http.Transport")
	}
	tr = tr.Clone()
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil || pu.Scheme == "" || pu.Host == "" {
			// The proxy URL can hold credentials, so the error does not quote it.
			return nil, errors.New("accountflag: proxy must be an absolute URL")
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &http.Client{Transport: tr, Timeout: requestTimeout, CheckRedirect: noRedirect}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// State returns the last known state. It reads a cached value and never blocks.
func (p *Poller) State() State { return State(p.state.Load()) }

// Changed receives after every state change. It is buffered 1, so changes that arrive
// while nobody reads collapse into one notification.
func (p *Poller) Changed() <-chan struct{} { return p.changed }

// Run polls at once, then every Interval after a success. After a failure it waits
// InitialBackoff, doubling per consecutive failure up to MaxBackoff. It returns when ctx
// is done.
func (p *Poller) Run(ctx context.Context) {
	var wait, backoff time.Duration
	for {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.after(wait):
			}
		} else if ctx.Err() != nil {
			return
		}

		err := p.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 0
			wait = p.cfg.Interval
			continue
		}
		metricPollFailures.Inc()
		backoff = min(max(backoffFactor*backoff, p.cfg.InitialBackoff), p.cfg.MaxBackoff)
		wait = backoff
		log.WithError(err).WithFields(log.Fields{
			"state":   p.State().String(),
			"backoff": backoff.String(),
		}).Warn("accountflag: polling the command API failed, keeping the last known state")
	}
}

// poll makes one request and applies its answer. Errors never quote the license key, the
// request headers or the response body.
func (p *Poller) poll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, http.NoBody)
	if err != nil {
		return errors.New("building the command request failed")
	}
	req.Header.Set(licenseHeader, p.cfg.LicenseKey)
	req.Header.Set(entityIDHeader, "0")
	req.Header.Set("Accept", "application/json")
	if p.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", p.cfg.UserAgent)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New(p.redact(err.Error()))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("command API returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return errors.New(p.redact("reading the command API response: " + err.Error()))
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("command API response is larger than %d bytes", maxBodyBytes)
	}
	enabled, found, err := ParseCommands(body, Category, Flag)
	if err != nil {
		return err
	}
	p.apply(enabled, found)
	return nil
}

func (p *Poller) redact(s string) string {
	return strings.ReplaceAll(s, p.cfg.LicenseKey, "[redacted]")
}

// apply moves the state for one successful answer. An explicit value sets it. An answer
// without the flag keeps On or Off and turns Unknown into Off.
func (p *Poller) apply(enabled, found bool) {
	cur := p.State()
	next := cur
	switch {
	case found && enabled:
		next = On
	case found:
		next = Off
	case cur == Unknown:
		next = Off
	}
	if next == cur {
		return
	}
	p.state.Store(int32(next))
	setStateMetric(next)
	select {
	case p.changed <- struct{}{}:
	default:
	}
	log.WithFields(log.Fields{"from": cur.String(), "to": next.String()}).
		Info("accountflag: collection flag changed")
}

type command struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type flagArguments struct {
	Enabled  *bool  `json:"enabled"`
	Category string `json:"category"`
	Flag     string `json:"flag"`
}

// ParseCommands reads a command API response and returns the value of the last
// set_feature_flag command for category and flag. found is false when no such command
// carries an enabled value. A set_feature_flag command whose arguments do not decode is
// skipped, because nothing in it can be matched to the flag. The error never quotes body.
func ParseCommands(body []byte, category, flag string) (enabled, found bool, err error) {
	var payload struct {
		ReturnValue []command `json:"return_value"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return false, false, fmt.Errorf("command API response is not valid JSON (offset %d)", syntaxErr.Offset)
		}
		return false, false, errors.New("command API response is not an object with a return_value list")
	}
	for _, c := range payload.ReturnValue {
		if c.Name != setFlagCommand {
			continue
		}
		var a flagArguments
		if json.Unmarshal(c.Arguments, &a) != nil {
			continue
		}
		if a.Category == category && a.Flag == flag && a.Enabled != nil {
			enabled, found = *a.Enabled, true
		}
	}
	return enabled, found, nil
}

// The metrics are created unregistered, so they appear on /metrics only after
// RegisterMetrics.
var (
	metricState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nri_kube_events_objects_account_flag_state",
		Help: "Collection flag state from the infrastructure command API: 1 for the current state, 0 for the others.",
	}, []string{"state"})
	metricPollFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nri_kube_events_objects_account_flag_poll_failures_total",
		Help: "Command API polls that failed with a transport error, a non-2xx status or an undecodable body.",
	})
)

// RegisterMetrics registers the account flag metrics with r. It returns the errors of the
// registrations that failed, joined.
func RegisterMetrics(r prometheus.Registerer) error {
	var errs []error
	for _, c := range collectors() {
		if err := r.Register(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func collectors() []prometheus.Collector {
	return []prometheus.Collector{metricState, metricPollFailures}
}

func setStateMetric(current State) {
	for _, s := range []State{Unknown, Off, On} {
		v := 0.0
		if s == current {
			v = 1
		}
		metricState.WithLabelValues(s.String()).Set(v)
	}
}
