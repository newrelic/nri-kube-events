package objects

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The constants and reservedFields below, and normalizeLikeAgent, copy the event
// normalization of the infrastructure agent forwarder, github.com/newrelic/infrastructure-agent
// at commit 3346a15: pkg/integrations/legacy/types.go (constants), pkg/event/event.go
// (reservedFields, IsReserved) and pkg/integrations/legacy/utils.go (NormalizeEvent).
const (
	agentDefaultEventCategory = "notifications"
	agentRequiredEventField   = "summary"
	agentEventType            = "InfrastructureEvent"
	agentAttributesPrefix     = "attr."
)

// agentReservedFields is the agent's reserved key set, lowercase. "eventytype" is the
// agent's spelling, so eventType itself is not reserved.
var agentReservedFields = map[string]struct{}{
	"":                         {},
	"timestamp":                {},
	"eventytype":               {},
	"entityid":                 {},
	"entityguid":               {},
	"entitykey":                {},
	"entityname":               {},
	"hostname":                 {},
	"fullhostname":             {},
	"displayname":              {},
	"agentname":                {},
	"corecount":                {},
	"agentversion":             {},
	"kernelversion":            {},
	"operatingsystem":          {},
	"windowsplatform":          {},
	"windowsfamily":            {},
	"windowsversion":           {},
	"instancetype":             {},
	"system/ram":               {},
	"transformmemorytobytes":   {},
	"processorcount":           {},
	"installedmemorymegabytes": {},
	"awsregion":                {},
	"regionname":               {},
	"zone":                     {},
	"regionid":                 {},
}

func agentIsReserved(field string) bool {
	if len(field) > len(agentAttributesPrefix) && field[:len(agentAttributesPrefix)] == agentAttributesPrefix {
		return true
	}
	_, ok := agentReservedFields[strings.ToLower(field)]
	return ok
}

// normalizeLikeAgent is NormalizeEvent without the labels, extra annotations and integration
// user, which the forwarder passes empty for /v1/data. It copies every unreserved top-level
// key, attributes included, and then flattens attributes into the same map, so the event the
// agent sends carries the attributes twice.
func normalizeLikeAgent(event map[string]any, entityKey string) map[string]any {
	if _, ok := event[agentRequiredEventField]; !ok {
		return nil
	}
	n := map[string]any{
		"eventType": agentEventType,
		"category":  agentDefaultEventCategory,
	}
	for k, v := range event {
		if !agentIsReserved(k) {
			n[k] = v
		}
	}
	n["entityKey"] = entityKey
	if attrs, ok := event["attributes"].(map[string]any); ok {
		for k, v := range attrs {
			if _, ok := n[k]; ok {
				n["attr."+k] = v
			} else {
				n[k] = v
			}
		}
	}
	delete(n, "hostname")
	return n
}

// largestObject is a ConfigMap that sets every optional record attribute at its largest:
// a controller owner, labels past the label string cap, a deletionTimestamp, a phase, a Ready
// condition, generation and observedGeneration, unhealthy conditions past their cap, and data
// for 300 parts.
func largestObject() map[string]any {
	obj := smallConfigMap()
	meta := obj["metadata"].(map[string]any)
	labels := map[string]any{}
	for i := 0; i < 400; i++ {
		labels[fmt.Sprintf("example.com/label-%04d", i)] = "value-value-value"
	}
	meta["labels"] = labels
	meta["ownerReferences"] = []any{
		map[string]any{"kind": "App", "name": "ctl", "uid": "o-2", "controller": true},
	}
	meta["deletionTimestamp"] = "2026-11-17T10:00:00Z"
	meta["generation"] = int64(7)

	conds := []any{map[string]any{"type": "Ready", "status": "False", "reason": "NotReady"}}
	for i := 0; i < 300; i++ {
		conds = append(conds, map[string]any{"type": fmt.Sprintf("Check%04d", i), "status": "False", "reason": "SomeLongReasonText"})
	}
	obj["status"] = map[string]any{
		"phase":              "Pending",
		"observedGeneration": int64(6),
		"conditions":         conds,
	}
	obj["data"].(map[string]any)["filler"] = asciiText(300 * Step)
	return obj
}

// firstEvent returns data[0].events[0] of an SDK v3 payload.
func firstEvent(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var p struct {
		Data []struct {
			Events []map[string]any `json:"events"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &p))
	require.NotEmpty(t, p.Data)
	require.NotEmpty(t, p.Data[0].Events)
	return p.Data[0].Events[0]
}

func TestLargestRecordFitsAfterNormalization(t *testing.T) {
	recs, err := BuildRecords(largestObject(), cmGVR, OpUpdated, "example-cluster-01", time.Unix(1700000000, 0))
	require.NoError(t, err)
	a := recs[0].Attributes
	for _, k := range []string{"k8s.owner.kind", "k8s.owner.name", "k8s.owner.uid", "k8s.labels.truncated",
		"k8s.deletionTimestamp", "k8s.phase", "k8s.ready", "k8s.generation", "k8s.observedGeneration",
		"k8s.conditions.unhealthy.truncated"} {
		require.Contains(t, a, k)
	}
	require.Greater(t, len(recs), 1)
	// Record 0 holds at most PartsPerRecord parts and MaxRecordBytes bytes, and the byte cap
	// closed it: with the next part added, measured as groupParts measures, it would exceed
	// MaxRecordBytes.
	recSize := mustMarshalLen(t, recs[0])
	assert.LessOrEqual(t, countParts(a), PartsPerRecord)
	assert.LessOrEqual(t, recSize, MaxRecordBytes)
	next := recs[1].Attributes["record.firstPart"].(int)
	require.Equal(t, countParts(a), next)
	grown := make(map[string]any, len(a)+1)
	for k, v := range a {
		grown[k] = v
	}
	grown["record.index"], grown["record.count"], grown["record.firstPart"] = 999999, 999999, 999999
	grown[partKey(next)] = recs[1].Attributes[partKey(next)]
	assert.Greater(t, mustMarshalLen(t, Record{Summary: recs[0].Summary, Category: recs[0].Category, Attributes: grown}), MaxRecordBytes,
		"record 0 was not closed by the byte cap")

	body, err := newTestSink(t, "http://unused", "example-cluster-01").payload(recs[:1])
	require.NoError(t, err)
	n := normalizeLikeAgent(firstEvent(t, body), "k8s:example-cluster-01:objectstate")
	require.NotNil(t, n)

	assert.LessOrEqual(t, len(n), 200)
	for k := range n {
		assert.False(t, strings.HasPrefix(k, "attr."), "collision on %s", k)
	}
	assert.Equal(t, "kubernetes.objectState", n["category"])
	assert.Equal(t, "K8sObject", n["type"])
	assert.EqualValues(t, 1700000000, n["timestamp"])
	assert.Equal(t, "InfrastructureEvent", n["eventType"])

	b, err := json.Marshal(n)
	require.NoError(t, err)
	// The agent refuses an event whose JSON is larger than its 1,000,000-byte batch size
	// (internal/agent/event_sender_vortex.go:206 and internal/agent/event_sender.go:187 at 3346a15).
	assert.Less(t, len(b), 1_000_000)
	// The agent keeps attributes as a key and also flattens it, so the event carries the record
	// twice. overhead is what normalization adds beyond that; a record at MaxRecordBytes must
	// still fit, so raising MaxRecordBytes past about 499,000 fails here.
	overhead := len(b) - 2*recSize
	assert.Less(t, 2*MaxRecordBytes+overhead, 1_000_000)
	t.Logf("record 0: %d parts, %d bytes; normalized: %d keys, %d bytes, overhead %d bytes",
		countParts(a), recSize, len(n), len(b), overhead)
}

func TestHeartbeatFitsAfterNormalization(t *testing.T) {
	cfg := testConfig()
	cfg.ClusterName = "example-cluster-01"
	cfg.AccountFlag = func() string { return "on" }
	w, _, store, _ := newTestWatcher(t, cfg, nil)
	require.NoError(t, store.Load(context.Background()))
	ctx := context.Background()
	for i := 0; i < 400; i++ {
		kind := fmt.Sprintf("Kind%s%03d", strings.Repeat("x", 12), i)
		gvr := schema.GroupVersionResource{Group: "listed.example.com", Version: "v1", Resource: strings.ToLower(kind) + "s"}
		w.track(gvr, kind)
		require.NoError(t, w.newObjectStore(ctx, gvr).Replace(nil, "1"))
	}
	var failing []schema.GroupVersionResource
	for i := 0; i < 300; i++ {
		failing = append(failing, schema.GroupVersionResource{Group: "failing.example.com", Version: "v1", Resource: fmt.Sprintf("resource%04d", i)})
	}
	markFailing(w, failing...)

	hb := w.HeartbeatRecord(time.Unix(1700000000, 0))
	require.Contains(t, hb.Attributes, "carrier.kinds.p001", "the kinds need more than one part")
	for k, v := range hb.Attributes {
		if s, ok := v.(string); ok {
			assert.LessOrEqual(t, len(s), MaxAttrBytes, k)
		}
	}

	body, err := newTestSink(t, "http://unused", cfg.ClusterName).payload([]Record{hb})
	require.NoError(t, err)
	n := normalizeLikeAgent(firstEvent(t, body), EntityName(cfg.ClusterName))
	require.NotNil(t, n)
	assert.LessOrEqual(t, len(n), 200)
	for k := range n {
		assert.False(t, strings.HasPrefix(k, "attr."), "collision on %s", k)
	}
	assert.Equal(t, "K8sObjectCarrier", n["type"])
	assert.Equal(t, "kubernetes.objectState", n["category"])
	assert.Equal(t, "InfrastructureEvent", n["eventType"])
	assert.EqualValues(t, 1700000000, n["timestamp"])
	assert.Equal(t, cfg.ClusterName, n["clusterName"])
	assert.Equal(t, cfg.ClusterName, n["k8s.cluster.name"])
	assert.Equal(t, 300, int(n["carrier.failedKindCount"].(float64)))

	b, err := json.Marshal(n)
	require.NoError(t, err)
	assert.Less(t, len(b), 1_000_000)
	t.Logf("heartbeat: %d attributes; normalized: %d keys, %d bytes", len(hb.Attributes), len(n), len(b))
}
