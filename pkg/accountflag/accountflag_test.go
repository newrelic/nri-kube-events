package accountflag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKey = "abc123licensekey"

type scripted struct {
	body   string
	status int
}

func ok(body string) scripted { return scripted{status: http.StatusOK, body: body} }

func flagCmd(category, flag string, enabled bool) string {
	return fmt.Sprintf(`{"id":0,"name":"set_feature_flag","arguments":{"category":%q,"flag":%q,"enabled":%t}}`,
		category, flag, enabled)
}

func commands(cmds ...string) string {
	return `{"return_value":[` + strings.Join(cmds, ",") + `]}`
}

var (
	flagOn  = ok(commands(flagCmd(Category, Flag, true)))
	flagOff = ok(commands(flagCmd(Category, Flag, false)))
	noFlag  = ok(`{"return_value":[]}`)
	fail500 = scripted{status: http.StatusInternalServerError, body: "oops"}
)

type recordedRequest struct {
	Header http.Header
	Method string
}

// cmdServer serves script in order and then repeats its last entry.
type cmdServer struct {
	*httptest.Server
	script []scripted
	reqs   []recordedRequest
	mu     sync.Mutex
}

func newCmdServer(t *testing.T, script ...scripted) *cmdServer {
	t.Helper()
	s := &cmdServer{script: script}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		i := min(len(s.reqs), len(s.script)-1)
		s.reqs = append(s.reqs, recordedRequest{Method: r.Method, Header: r.Header.Clone()})
		resp := s.script[i]
		s.mu.Unlock()
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *cmdServer) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.reqs...)
}

func (s *cmdServer) count(method string) int {
	n := 0
	for _, r := range s.requests() {
		if method == "" || r.Method == method {
			n++
		}
	}
	return n
}

func newTestPoller(t *testing.T, url string, mods ...func(*Config)) *Poller {
	t.Helper()
	cfg := Config{
		URL:            url,
		LicenseKey:     testKey,
		Interval:       10 * time.Millisecond,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	}
	for _, m := range mods {
		m(&cfg)
	}
	p, err := NewPoller(cfg)
	require.NoError(t, err)
	return p
}

// runUntil runs p until done reports true, then stops it and waits for Run to return.
func runUntil(t *testing.T, p *Poller, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(stopped)
	}()
	require.Eventually(t, done, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// runScript runs p until srv has answered every scripted response and one more, so the
// state reflects the whole script.
func runScript(t *testing.T, p *Poller, srv *cmdServer) {
	t.Helper()
	runUntil(t, p, func() bool { return srv.count("") > len(srv.script) })
}

func runFor(t *testing.T, p *Poller, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	p.Run(ctx)
}

func drained(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func metricValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var d dto.Metric
	require.NoError(t, m.Write(&d))
	if d.GetCounter() != nil {
		return d.GetCounter().GetValue()
	}
	return d.GetGauge().GetValue()
}

func failures(t *testing.T) float64 { return metricValue(t, metricPollFailures) }

func TestPollRequestShape(t *testing.T) {
	srv := newCmdServer(t, ok(`{"return_value":[]}`))
	p := newTestPoller(t, srv.URL, func(c *Config) { c.UserAgent = "nri-kube-events/test" })
	runFor(t, p, 50*time.Millisecond)
	reqs := srv.requests()
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, testKey, r.Header.Get("X-License-Key"))
		assert.Equal(t, "0", r.Header.Get("X-NRI-Agent-Entity-Id"))
		assert.Equal(t, "nri-kube-events/test", r.Header.Get("User-Agent"))
	}
	assert.Zero(t, srv.count(http.MethodPost)) // never acks
}

func TestUnknownUntilFirstAnswer(t *testing.T) {
	srv := newCmdServer(t, fail500)
	p := newTestPoller(t, srv.URL)
	assert.Equal(t, Unknown, p.State())
	runUntil(t, p, func() bool { return srv.count("") >= 3 })
	assert.Equal(t, Unknown, p.State())
	assert.False(t, drained(p.Changed()))
}

func TestFlagOn(t *testing.T) {
	srv := newCmdServer(t, flagOn)
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, On, p.State())
	assert.True(t, drained(p.Changed()))
}

func TestExplicitOffTurnsOff(t *testing.T) {
	srv := newCmdServer(t, flagOn, flagOff)
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, Off, p.State())
	assert.True(t, drained(p.Changed()))
	assert.False(t, drained(p.Changed()), "Changed is buffered 1")
}

func TestMissingFlagKeepsLastValue(t *testing.T) {
	srv := newCmdServer(t, flagOn, noFlag, ok(commands(flagCmd(Category, "other_flag", false))))
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, On, p.State())
}

func TestMissingFlagBeforeAnyValueIsOff(t *testing.T) {
	srv := newCmdServer(t, noFlag)
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, Off, p.State())
	assert.True(t, drained(p.Changed()))
}

func TestOtherCategoryIgnored(t *testing.T) {
	srv := newCmdServer(t, flagOn, ok(commands(flagCmd("Other", Flag, false))))
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, On, p.State())
}

func TestLastCommandWins(t *testing.T) {
	srv := newCmdServer(t, ok(commands(flagCmd(Category, Flag, true), flagCmd(Category, Flag, false))))
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	assert.Equal(t, Off, p.State())
}

func TestPollFailureKeepsStateAndBacksOff(t *testing.T) {
	srv := newCmdServer(t, flagOn, fail500, fail500, fail500, fail500, flagOn)
	p := newTestPoller(t, srv.URL, func(c *Config) { c.Interval = 15 * time.Millisecond })
	var mu sync.Mutex
	var waits []time.Duration
	p.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return time.After(d)
	}
	before := failures(t)

	runScript(t, p, srv)

	assert.Equal(t, On, p.State())
	assert.InDelta(t, 4, failures(t)-before, 0)
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(waits), 6)
	ms := time.Millisecond
	assert.Equal(t, []time.Duration{15 * ms, 5 * ms, 10 * ms, 20 * ms, 20 * ms, 15 * ms}, waits[:6])
}

func TestMalformedBodyIsFailure(t *testing.T) {
	srv := newCmdServer(t, flagOn, ok("not json"))
	p := newTestPoller(t, srv.URL)
	before := failures(t)
	runScript(t, p, srv)
	assert.Equal(t, On, p.State())
	assert.GreaterOrEqual(t, failures(t)-before, 1.0)
}

type echoKeyTransport struct {
	mu sync.Mutex
	n  int
}

func (e *echoKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	return nil, errors.New("dial failed for key " + r.Header.Get("X-License-Key"))
}

func (e *echoKeyTransport) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

func assertLogsOmitKey(t *testing.T, hook *logtest.Hook) {
	t.Helper()
	entries := hook.AllEntries()
	require.NotEmpty(t, entries, "a poll failure is logged")
	for _, e := range entries {
		line, err := e.String()
		require.NoError(t, err)
		assert.NotContains(t, line, testKey)
		assert.NotContains(t, line, "secret-body")
	}
}

func TestPollErrorOmitsLicenseKey(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		hook := logtest.NewGlobal()
		defer hook.Reset()
		tr := &echoKeyTransport{}
		p := newTestPoller(t, "http://command-api.invalid/agent_commands/v1/commands", func(c *Config) {
			c.HTTPClient = &http.Client{Transport: tr}
		})
		runUntil(t, p, func() bool { return tr.count() >= 2 })
		assertLogsOmitKey(t, hook)
	})
	t.Run("403 echoing the key", func(t *testing.T) {
		hook := logtest.NewGlobal()
		defer hook.Reset()
		srv := newCmdServer(t, scripted{status: http.StatusForbidden, body: "secret-body rejected " + testKey})
		p := newTestPoller(t, srv.URL)
		runUntil(t, p, func() bool { return srv.count("") >= 2 })
		assertLogsOmitKey(t, hook)
	})
}

func TestRedirectNotFollowed(t *testing.T) {
	target := newCmdServer(t, flagOn)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	for _, client := range []*http.Client{nil, {}} {
		p := newTestPoller(t, redirect.URL, func(c *Config) { c.HTTPClient = client })
		before := failures(t)
		runUntil(t, p, func() bool { return failures(t)-before >= 2 })
		assert.Zero(t, target.count(""), "the license key header must not follow a redirect")
		assert.Equal(t, Unknown, p.State())
	}
}

func TestPollerUsesProxy(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		_, _ = w.Write([]byte(flagOn.body))
	}))
	defer proxy.Close()
	target := "http://command-api.invalid/agent_commands/v1/commands"
	p := newTestPoller(t, target, func(c *Config) { c.Proxy = proxy.URL })
	runUntil(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, target, seen[0])
	assert.Equal(t, On, p.State())
}

func TestNewPollerValidation(t *testing.T) {
	good := Config{URL: "https://example.test/agent_commands/v1/commands", LicenseKey: testKey}
	cases := map[string]func(*Config){
		"unparseable proxy":  func(c *Config) { c.Proxy = "://bad" },
		"proxy without host": func(c *Config) { c.Proxy = "http://" },
		"proxy and client":   func(c *Config) { c.Proxy = "http://proxy.test:3128"; c.HTTPClient = &http.Client{} },
		"missing URL":        func(c *Config) { c.URL = "" },
		"relative URL":       func(c *Config) { c.URL = "/agent_commands/v1/commands" },
		"missing key":        func(c *Config) { c.LicenseKey = "" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mod(&cfg)
			_, err := NewPoller(cfg)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testKey)
		})
	}
	p, err := NewPoller(good)
	require.NoError(t, err)
	assert.Equal(t, DefaultInterval, p.cfg.Interval)
	assert.Equal(t, DefaultInitialBackoff, p.cfg.InitialBackoff)
	assert.Equal(t, DefaultMaxBackoff, p.cfg.MaxBackoff)
}

func TestParseCommands(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		enabled, found bool
		wantErr        bool
	}{
		{name: "on", body: flagOn.body, enabled: true, found: true},
		{name: "empty object", body: `{}`},
		{name: "other command name", body: `{"return_value":[{"name":"backoff_command_channel","arguments":{"delay":5}}]}`},
		{name: "enabled missing", body: commands(`{"name":"set_feature_flag","arguments":{"category":"Infra_Agent","flag":"k8s_object_state_enabled"}}`)},
		{name: "unrelated malformed arguments", body: commands(`{"name":"set_feature_flag","arguments":"x"}`, flagCmd(Category, Flag, true)), enabled: true, found: true},
		{name: "not json", body: "not json", wantErr: true},
		{name: "array", body: "[]", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enabled, found, err := ParseCommands([]byte(c.body), Category, Flag)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.enabled, enabled)
			assert.Equal(t, c.found, found)
		})
	}
}

func TestStateString(t *testing.T) {
	assert.Equal(t, "unknown", Unknown.String())
	assert.Equal(t, "off", Off.String())
	assert.Equal(t, "on", On.String())
}

func TestStateMetric(t *testing.T) {
	srv := newCmdServer(t, flagOn)
	p := newTestPoller(t, srv.URL)
	runScript(t, p, srv)
	require.Equal(t, On, p.State())
	assert.InDelta(t, 1, metricValue(t, metricState.WithLabelValues("on")), 0)
	assert.InDelta(t, 0, metricValue(t, metricState.WithLabelValues("off")), 0)
	assert.InDelta(t, 0, metricValue(t, metricState.WithLabelValues("unknown")), 0)
}

func TestRegisterMetrics(t *testing.T) {
	// Nothing registers globally: each collector can still be registered on the default
	// registry.
	for _, c := range collectors() {
		require.NoError(t, prometheus.DefaultRegisterer.Register(c), "an accountflag metric is registered globally")
		prometheus.DefaultRegisterer.Unregister(c)
	}
	r := prometheus.NewRegistry()
	require.NoError(t, RegisterMetrics(r))
	_, err := NewPoller(Config{URL: "https://example.test/x", LicenseKey: testKey})
	require.NoError(t, err)
	mfs, err := r.Gather()
	require.NoError(t, err)
	names := make([]string, 0, len(mfs))
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	assert.ElementsMatch(t, []string{
		"nri_kube_events_objects_account_flag_state",
		"nri_kube_events_objects_account_flag_poll_failures_total",
	}, names)
}
