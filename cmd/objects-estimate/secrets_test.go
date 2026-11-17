package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
