package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

const tableJSON = `{
  "kind": "Table", "apiVersion": "meta.k8s.io/v1",
  "metadata": {"resourceVersion": "42", "continue": "next"},
  "columnDefinitions": [{"name": "Name"}, {"name": "Type"}, {"name": "Data"}, {"name": "Age"}],
  "rows": [
    {"cells": ["db-creds", "Opaque", 3, "5d"]},
    {"cells": ["sh.helm.release.v1.app.v7", "helm.sh/release.v1", 1, "2h"]}
  ]
}`

func TestWatchEventWithoutColumnsUsesEarlierColumns(t *testing.T) {
	// A table watch sends the column definitions with its first event only.
	cols, err := secretColumns([]byte(tableJSON), nil)
	require.NoError(t, err)
	later := []byte(`{"kind":"Table","rows":[{"cells":["gateway-cert","kubernetes.io/tls",2,"1s"]}]}`)
	tbl, err := parseSecretTable(later, cols)
	require.NoError(t, err)
	assert.Equal(t, []secretRow{{Name: "gateway-cert", Type: "kubernetes.io/tls", Keys: 2}}, tbl.Rows)

	_, err = parseSecretTable(later, nil)
	assert.ErrorContains(t, err, "Name")
}

func TestCountChangeCountsEventsItCannotRead(t *testing.T) {
	c := newSecretCounter(testCluster, testVersion)
	c.countEvent("ADDED", []byte(`{"kind":"Table","rows":[{"cells":["a","Opaque",1,"1s"]}]}`))
	assert.Equal(t, 1, c.summary().UnparsedEvents)
	c.countEvent("ADDED", []byte(tableJSON))
	c.countEvent("DELETED", []byte(`{"kind":"Table","rows":[{"cells":["a","Opaque",1,"1s"]}]}`))
	s := c.summary()
	assert.Equal(t, int64(2), s.Changes["added"].Count)
	assert.Equal(t, int64(1), s.Changes["deleted"].Count)
	assert.Equal(t, 1, s.UnparsedEvents)
}

func TestParseSecretTableReadsNameTypeAndKeyCount(t *testing.T) {
	tbl, err := parseSecretTable([]byte(tableJSON), nil)
	require.NoError(t, err)
	assert.Equal(t, "42", tbl.ResourceVersion)
	assert.Equal(t, "next", tbl.Continue)
	assert.Equal(t, []secretRow{
		{Name: "db-creds", Type: "Opaque", Keys: 3},
		{Name: "sh.helm.release.v1.app.v7", Type: "helm.sh/release.v1", Keys: 1},
	}, tbl.Rows)
}

func TestParseSecretTableRejectsMissingColumns(t *testing.T) {
	_, err := parseSecretTable([]byte(`{"kind":"Table","columnDefinitions":[{"name":"Name"}],"rows":[]}`), nil)
	assert.ErrorContains(t, err, "Type")
}

func TestStandInSecretSizeMatchesTheCarrier(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	row := secretRow{Name: "db-creds", Type: "Opaque", Keys: 3}
	got, err := standInBytes(row, "payments", testCluster, testVersion, now)
	require.NoError(t, err)

	// The same stand-in built by hand and sent through the carrier's own redaction and
	// records, each sized with the attributes the infra sink adds.
	obj := standInSecret(row, "payments")
	recs, err := objects.BuildRecords(objects.RedactSecret(obj, standInKey), secretsGVR, objects.OpInitial, testCluster, now)
	require.NoError(t, err)
	var want int64
	for _, r := range recs {
		want += size(t, r)
	}
	assert.Equal(t, want, got)

	// More keys make a larger record; the redacted values hide nothing about their count.
	bigger, err := standInBytes(secretRow{Name: "db-creds", Type: "Opaque", Keys: 30}, "payments", testCluster, testVersion, now)
	require.NoError(t, err)
	assert.Greater(t, bigger, got)
}

func TestAddOfAKnownSecretIsNotAChange(t *testing.T) {
	c := newSecretCounter(testCluster, testVersion)
	c.countEvent("ADDED", []byte(tableJSON))
	// A watch that replays existing Secrets sends an ADDED for each of them again.
	c.countEvent("ADDED", []byte(tableJSON))
	s := c.summary()
	assert.Equal(t, int64(2), s.Changes["added"].Count)
	assert.Equal(t, 2, s.SuppressedAdds)

	// A Secret deleted and created again is a change.
	c.countEvent("DELETED", []byte(tableJSON))
	c.countEvent("ADDED", []byte(tableJSON))
	s = c.summary()
	assert.Equal(t, int64(4), s.Changes["added"].Count)
	assert.Equal(t, 2, s.SuppressedAdds)
}

// watchServer serves the cluster-wide Secrets table: each watch request gets the next
// entry of watches (written, then the stream ends) and blocks once they run out; each
// relist gets the next entry of relists, then a table with resourceVersion "999".
type watchServer struct {
	watched chan string
	watches []string
	relists []int
	mu      sync.Mutex
}

func (f *watchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/secrets" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.Get("watch") == "true" {
		f.watched <- q.Get("resourceVersion")
		if len(f.watches) == 0 {
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		fmt.Fprint(w, f.watches[0])
		f.watches = f.watches[1:]
		return
	}
	if len(f.relists) > 0 {
		code := f.relists[0]
		f.relists = f.relists[1:]
		w.WriteHeader(code)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure"}`)
		return
	}
	fmt.Fprint(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","metadata":{"resourceVersion":"999"},
"columnDefinitions":[{"name":"Name"},{"name":"Type"},{"name":"Data"}],"rows":[]}`)
}

// runWatch runs watchChanges from rv against f until want watch requests arrived, and
// returns their resourceVersions and the summary.
func runWatch(t *testing.T, f *watchServer, c *secretCounter, rv string, want int) ([]string, secretSummary) {
	t.Helper()
	f.watched = make(chan string, want+1)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	conf := &rest.Config{Host: srv.URL}
	conf.Wrap(objects.ReadOnlyTransport)
	cs, err := kubernetes.NewForConfig(conf)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchChanges(ctx, cs.CoreV1().RESTClient(), rv)
	}()
	var got []string
	for len(got) < want {
		select {
		case v := <-f.watched:
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d watch requests %q, want %d", len(got), got, want)
		}
	}
	cancel()
	<-done
	return got, c.summary()
}

const addedEvent = `{"type":"ADDED","object":{"kind":"Table","apiVersion":"meta.k8s.io/v1",
"metadata":{"resourceVersion":"43"},
"columnDefinitions":[{"name":"Name"},{"name":"Type"},{"name":"Data"}],
"rows":[{"cells":["new-secret","Opaque",1]}]}}`

func TestWatchNeverStartsFromAnEmptyResourceVersion(t *testing.T) {
	f := &watchServer{
		watches: []string{`{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired"}}`},
		relists: []int{http.StatusInternalServerError, http.StatusServiceUnavailable},
	}
	c := newSecretCounter(testCluster, testVersion)
	c.retryWait, c.maxRetryWait = time.Millisecond, time.Millisecond
	// The relist failures are logged; keep them out of the test output.
	out := log.StandardLogger().Out
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(out) })

	got, s := runWatch(t, f, c, "42", 2)
	assert.Equal(t, []string{"42", "999"}, got)
	assert.Equal(t, 1, s.WatchRestarts)
	assert.Equal(t, 1, s.WatchGaps)
	assert.Equal(t, 3, s.Relists)
	assert.Equal(t, 2, s.RelistFailures)
}

func TestCleanWatchEndResumesWithoutAGap(t *testing.T) {
	// The second watch ends cleanly with no event, so it resumes from its own start.
	f := &watchServer{watches: []string{addedEvent, ""}}
	c := newSecretCounter(testCluster, testVersion)
	// A pause this long would time the test out.
	c.retryWait, c.maxRetryWait = time.Hour, time.Hour

	got, s := runWatch(t, f, c, "42", 3)
	assert.Equal(t, []string{"42", "43", "43"}, got)
	assert.Equal(t, 2, s.WatchRestarts)
	assert.Equal(t, 0, s.WatchGaps)
	assert.Equal(t, 0, s.Relists)
	assert.Equal(t, int64(1), s.Changes["added"].Count)
}
