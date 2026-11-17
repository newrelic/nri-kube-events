package objects

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyRefusesWrites(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Method)
	}))
	defer srv.Close()
	client := &http.Client{Transport: ReadOnlyTransport(http.DefaultTransport)}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
		req, err := http.NewRequestWithContext(t.Context(), m, srv.URL, nil)
		require.NoError(t, err)
		resp, err := client.Do(req) //nolint:bodyclose // the transport refuses, so there is no body
		assert.Nil(t, resp, m)
		assert.ErrorContains(t, err, "read-only", m)
	}
	// Only the GET reached the server.
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{http.MethodGet}, seen)
}
