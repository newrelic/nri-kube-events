package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

const (
	testCluster = "c1"
	testVersion = "1.2.3"
)

var configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

func rec(op, kind string, index int, summary string) objects.Record {
	return objects.Record{
		Summary:  summary,
		Category: objects.Category,
		Attributes: map[string]any{
			"type": objects.ObjectType, "k8s.op": op, "k8s.kind": kind, "record.index": index,
		},
	}
}

// size is the marshaled size of r with the four attributes the infra sink adds to every
// event, written out here so that the test pins them.
func size(t *testing.T, r objects.Record) int64 {
	t.Helper()
	attrs := map[string]any{}
	for k, v := range r.Attributes {
		attrs[k] = v
	}
	attrs["clusterName"] = testCluster
	attrs["integrationName"] = "kube_events"
	attrs["integrationVersion"] = testVersion
	attrs["eventRouterVersion"] = testVersion
	b, err := json.Marshal(objects.Record{Summary: r.Summary, Category: r.Category, Attributes: attrs})
	require.NoError(t, err)
	return int64(len(b))
}

func TestCountingSinkSplitsFirstSendFromChanges(t *testing.T) {
	s := newCountingSink(testCluster, testVersion)
	first := []objects.Record{rec("initial", "Pod", 0, "a"), rec("initial", "Pod", 1, "a2")}
	change := rec("updated", "Deployment", 0, "b")
	hb := objects.Record{Summary: "hb", Category: objects.Category, Attributes: map[string]any{
		"type": objects.HeartbeatType, "lastFullListTime": int64(5000),
	}}

	require.NoError(t, s.Send(context.Background(), first))
	require.NoError(t, s.Send(context.Background(), []objects.Record{change}))
	require.NoError(t, s.Send(context.Background(), []objects.Record{hb}))

	sum := s.summary()
	assert.Equal(t, int64(2), sum.Initial.Records)
	assert.Equal(t, int64(1), sum.Initial.Versions)
	assert.Equal(t, size(t, first[0])+size(t, first[1]), sum.Initial.Bytes)
	assert.Equal(t, int64(2), sum.Initial.ByKind["Pod"].Records)

	assert.Equal(t, int64(1), sum.Changes.Records)
	assert.Equal(t, size(t, change), sum.Changes.ByOp["updated"].Bytes)
	assert.Equal(t, int64(1), sum.Changes.ByKind["Deployment"].Versions)

	assert.Equal(t, int64(1), sum.Heartbeat.Records)
	assert.Equal(t, size(t, hb), sum.Heartbeat.Bytes)
	assert.Equal(t, time.UnixMilli(5000).UTC(), *sum.FullListAt)
}

func TestCountingSinkCountsALargeObjectAsOneVersionOfTwoRecords(t *testing.T) {
	obj := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "big", "namespace": "ns", "uid": "uid-big", "resourceVersion": "1"},
		"data":     map[string]any{"blob": strings.Repeat("x", 111*objects.Step)},
	}
	recs, err := objects.BuildRecords(obj, configMapsGVR, objects.OpInitial, testCluster, time.Unix(0, 0))
	require.NoError(t, err)
	require.Len(t, recs, 2)

	s := newCountingSink(testCluster, testVersion)
	require.NoError(t, s.Send(context.Background(), recs))

	sum := s.summary()
	assert.Equal(t, int64(2), sum.Initial.Records)
	assert.Equal(t, int64(1), sum.Initial.Versions)
	assert.Equal(t, size(t, recs[0])+size(t, recs[1]), sum.Initial.Bytes)
	// The snapshot cross-check does not reassemble an object of several records.
	assert.Equal(t, recentChanges{Objects: 1, Unknown: 1}, *sum.ChangedBefore["ConfigMap"])
}

func cmWithManagedTime(t *testing.T, name, at string) []objects.Record {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": "ns", "uid": "uid-" + name, "resourceVersion": "1",
			"managedFields": []any{
				map[string]any{"manager": "old", "time": "2020-01-01T00:00:00Z"},
				map[string]any{"manager": "new", "time": at},
			},
		},
	}
	recs, err := objects.BuildRecords(obj, configMapsGVR, objects.OpInitial, testCluster, time.Unix(0, 0))
	require.NoError(t, err)
	return recs
}

func TestCountingSinkCountsObjectsChangedBeforeTheRun(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s := newCountingSink(testCluster, testVersion)
	s.start = start
	for name, at := range map[string]string{
		"hour":  "2026-11-08T11:30:00Z", // within the hour before start
		"day":   "2026-11-08T01:00:00Z", // within the day, not the hour
		"older": "2026-11-01T00:00:00Z",
	} {
		require.NoError(t, s.Send(context.Background(), cmWithManagedTime(t, name, at)))
	}

	got := s.summary().ChangedBefore["ConfigMap"]
	require.NotNil(t, got)
	assert.Equal(t, recentChanges{Objects: 3, LastHour: 1, LastDay: 2}, *got)
}

func TestCountingSinkHasNoFullListBeforeHeartbeatReportsOne(t *testing.T) {
	s := newCountingSink(testCluster, testVersion)
	hb := objects.Record{Attributes: map[string]any{"type": objects.HeartbeatType, "lastFullListTime": int64(0)}}
	require.NoError(t, s.Send(context.Background(), []objects.Record{hb}))
	assert.Nil(t, s.summary().FullListAt)
}

func TestWriteJSONCreatesTheOutputDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "summary.json")
	require.NoError(t, writeJSON(path, map[string]int{"a": 1}))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]int
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, map[string]int{"a": 1}, got)
}
