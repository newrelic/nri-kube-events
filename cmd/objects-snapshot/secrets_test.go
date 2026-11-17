package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

// tablePage is a Table page of the named Secrets, continuing at cont when it is set.
func tablePage(cont string, names ...string) string {
	rows := make([]string, 0, len(names))
	for _, n := range names {
		rows = append(rows, fmt.Sprintf(`{"cells": [%q, "Opaque", 1, "5d"]}`, n))
	}
	return fmt.Sprintf(`{"kind": "Table", "apiVersion": "meta.k8s.io/v1",
  "metadata": {"resourceVersion": "42", "continue": %q},
  "columnDefinitions": [{"name": "Name"}, {"name": "Type"}, {"name": "Data"}, {"name": "Age"}],
  "rows": [%s]}`, cont, strings.Join(rows, ","))
}

// fakeAPIServer serves a namespace list of namespaces and, for each namespace, the Secrets
// table that secrets returns for it and the request's continue token. Every request it gets
// is recorded.
type fakeAPIServer struct {
	secrets    func(namespace, cont string) (int, string)
	namespaces []string
	requests   []*http.Request
	mu         sync.Mutex
}

func (f *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/namespaces" {
		items := make([]string, 0, len(f.namespaces))
		for _, ns := range f.namespaces {
			items = append(items, fmt.Sprintf(`{"metadata": {"name": %q}}`, ns))
		}
		fmt.Fprintf(w, `{"kind": "NamespaceList", "apiVersion": "v1", "metadata": {}, "items": [%s]}`, strings.Join(items, ","))
		return
	}
	ns, ok := strings.CutPrefix(r.URL.Path, "/api/v1/namespaces/")
	ns, ok2 := strings.CutSuffix(ns, "/secrets")
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	code, body := f.secrets(ns, r.URL.Query().Get("continue"))
	w.WriteHeader(code)
	fmt.Fprint(w, body)
}

func (f *fakeAPIServer) secretRequests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.requests {
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			out = append(out, r)
		}
	}
	return out
}

func newFakeAPIServer(t *testing.T, f *fakeAPIServer) kubernetes.Interface {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	conf := &rest.Config{Host: srv.URL}
	conf.Wrap(objects.ReadOnlyTransport)
	cs, err := kubernetes.NewForConfig(conf)
	require.NoError(t, err)
	return cs
}

func TestSecretNamesFromTable(t *testing.T) {
	f := &fakeAPIServer{secrets: func(_, cont string) (int, string) {
		if cont == "" {
			return http.StatusOK, tablePage("page-2", "db-creds", "sh.helm.release.v1.app.v7")
		}
		return http.StatusOK, tablePage("", "gateway-cert")
	}}
	cs := newFakeAPIServer(t, f)

	got, err := listSecretNames(t.Context(), cs.CoreV1().RESTClient(), "ns1")
	require.NoError(t, err)
	assert.Equal(t, []string{"db-creds", "sh.helm.release.v1.app.v7", "gateway-cert"}, got)

	reqs := f.secretRequests()
	require.Len(t, reqs, 2)
	for _, r := range reqs {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/namespaces/ns1/secrets", r.URL.Path)
		assert.Equal(t, tableAccept, r.Header.Get("Accept"))
		q := r.URL.Query()
		assert.Equal(t, "None", q.Get("includeObject"))
		assert.Equal(t, "500", q.Get("limit"))
	}
	assert.Empty(t, reqs[0].URL.Query().Get("continue"))
	assert.Equal(t, "page-2", reqs[1].URL.Query().Get("continue"))
}

func TestSecretNamesRejectsTableWithoutName(t *testing.T) {
	f := &fakeAPIServer{secrets: func(string, string) (int, string) {
		return http.StatusOK, `{"kind": "Table", "columnDefinitions": [{"name": "Type"}], "rows": []}`
	}}
	cs := newFakeAPIServer(t, f)
	_, err := listSecretNames(t.Context(), cs.CoreV1().RESTClient(), "ns1")
	assert.ErrorContains(t, err, "Name")
}

func TestSecretRowsHaveNoUIDOrHash(t *testing.T) {
	rows := secretRows(testCarrierNS, "ns1", []string{"s1"})
	require.Equal(t, []row{{kind: "Secret", namespace: "ns1", name: "s1"}}, rows)
	var buf bytes.Buffer
	require.NoError(t, writeRows(&buf, rows))
	assert.Equal(t, "uid\tkind\tgroup\tnamespace\tname\tobject.hash\n\tSecret\t\tns1\ts1\t\n", buf.String())
}

func TestSecretRowsLeaveOutTheCarrierKey(t *testing.T) {
	rows := secretRows(testCarrierNS, testCarrierNS, []string{objects.HMACSecretName, "other"})
	assert.Equal(t, []string{"other"}, names(rows))
	// The same name in another namespace is an ordinary Secret.
	rows = secretRows(testCarrierNS, "ns1", []string{objects.HMACSecretName})
	assert.Equal(t, []string{objects.HMACSecretName}, names(rows))
}

func TestRunPrintsObjectsAndSecretsAndExitsTwoOnFailedList(t *testing.T) {
	f := &fakeAPIServer{namespaces: []string{"ns1", "ns2"}, secrets: func(ns, _ string) (int, string) {
		if ns == "ns2" {
			return http.StatusForbidden, `{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Forbidden", "code": 403}`
		}
		return http.StatusOK, tablePage("", "s1")
	}}
	cs := newFakeAPIServer(t, f)
	dyn, disc := fakeCluster(t, configMap("ns1", "a"))

	var out, errOut bytes.Buffer
	code := run(t.Context(), dyn, disc, cs, testCarrierNS, &out, &errOut)
	assert.Equal(t, exitListFailed, code)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, "uid\tkind\tgroup\tnamespace\tname\tobject.hash", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "uid-cm-ns1-a\tConfigMap\t\tns1\ta\t"), lines[1])
	assert.Equal(t, "\tSecret\t\tns1\ts1\t", lines[2])
	assert.Contains(t, errOut.String(), "/v1/secrets in ns2")

	// Every request was a GET, and no Secret was read as an object.
	for _, r := range f.requests {
		assert.Equal(t, http.MethodGet, r.Method)
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			assert.Equal(t, url.Values{"includeObject": {"None"}, "limit": {"500"}}, r.URL.Query())
		}
	}
}

func TestRunExitsZeroWhenEveryListSucceeds(t *testing.T) {
	f := &fakeAPIServer{namespaces: []string{"ns1"}, secrets: func(string, string) (int, string) {
		return http.StatusOK, tablePage("")
	}}
	cs := newFakeAPIServer(t, f)
	dyn, disc := fakeCluster(t, configMap("ns1", "a"))
	var out, errOut bytes.Buffer
	assert.Equal(t, 0, run(t.Context(), dyn, disc, cs, testCarrierNS, &out, &errOut))
	assert.Empty(t, errOut.String())
}
