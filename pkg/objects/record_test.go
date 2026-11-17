package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testNow = time.UnixMilli(1762430400123)

var (
	appsDeployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	cmGVR           = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// asciiText returns n runes cycling through the alphabet and digits, so every window differs.
func asciiText(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[(i*7+i/36)%len(alphabet)])
	}
	return b.String()
}

// multibyteText returns n runes cycling through alphabet.
func multibyteText(alphabet string, n int) string {
	a := []rune(alphabet)
	r := make([]rune, n)
	for i := range r {
		r[i] = a[(i*5+i/11)%len(a)]
	}
	return string(r)
}

// assertParts checks every part's mark, rune count and byte count, and that each part after
// the first starts with the last Overlap runes of the part before it.
func assertParts(t *testing.T, parts []string) {
	t.Helper()
	for i, p := range parts {
		assert.LessOrEqual(t, utf8.RuneCountInString(p), PartChars, "part %d runes", i)
		assert.LessOrEqual(t, len(p), MaxAttrBytes, "part %d bytes", i)
		assert.True(t, strings.HasPrefix(p, Mark), "part %d", i)
		if i > 0 {
			prev := []rune(parts[i-1])
			cur := []rune(strings.TrimPrefix(p, Mark))
			require.Greater(t, len(cur), Overlap, "part %d adds at least one new rune", i)
			assert.Equal(t, string(prev[len(prev)-Overlap:]), string(cur[:Overlap]), "part %d overlap", i)
		}
	}
}

func TestConstants(t *testing.T) {
	assert.Equal(t, 4094, PartChars)
	assert.Equal(t, 256, Overlap)
	assert.Equal(t, 3837, Step)
	assert.Equal(t, "~", Mark)
	assert.Equal(t, 110, PartsPerRecord)
	assert.Equal(t, 450_000, MaxRecordBytes)
	assert.Equal(t, 4095, MaxAttrBytes)
	assert.Equal(t, "K8sObject", ObjectType)
	assert.Equal(t, "K8sObjectCarrier", HeartbeatType)
	assert.Equal(t, "kubernetes.objectState", Category)
}

func TestSplitJoinRoundTrip(t *testing.T) {
	cases := []struct {
		n     int
		parts int
	}{
		{0, 0},
		{1, 1},
		{Step, 1},
		{Step + 1, 2},
		{200 * Step, 200},
		{200*Step + 1, 201},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.n), func(t *testing.T) {
			s := asciiText(c.n)
			parts := Split(s)
			assert.Len(t, parts, c.parts)
			assertParts(t, parts)
			assert.Equal(t, s, Join(parts))
		})
	}
}

func TestSplitPartBounds(t *testing.T) {
	s := asciiText(3 * Step)
	parts := Split(s)
	require.Len(t, parts, 3)
	r := []rune(s)
	assert.Equal(t, Mark+string(r[0:Step]), parts[0])
	assert.Equal(t, Mark+string(r[Step-Overlap:2*Step]), parts[1])
	assert.Equal(t, Mark+string(r[2*Step-Overlap:3*Step]), parts[2])
	assert.Equal(t, PartChars, utf8.RuneCountInString(parts[1]))
}

func TestSplitKeepsForkCutsForASCII(t *testing.T) {
	for _, n := range []int{0, 1, Step, Step + 1, 5*Step + 17} {
		s := asciiText(n)
		r := []rune(s)
		parts := Split(s)
		require.Len(t, parts, (n+Step-1)/Step, "length %d", n)
		for i, p := range parts {
			start := max(0, i*Step-Overlap)
			end := min(n, (i+1)*Step)
			assert.Equal(t, Mark+string(r[start:end]), p, "length %d part %d", n, i)
		}
	}
}

func TestSplitByteCapsMultibyteParts(t *testing.T) {
	s := strings.Repeat("😀", 20_000)
	parts := Split(s)
	// A part of 4-byte runes ends at (MaxAttrBytes-len(Mark))/4 = 1023 runes, well short of Step.
	assert.Greater(t, len(parts), (20_000+Step-1)/Step)
	assertParts(t, parts)
	assert.Equal(t, s, Join(parts))

	// A mixed alphabet, so the overlap check compares distinct runes.
	s = multibyteText("é中😀a", 20_000)
	parts = Split(s)
	assertParts(t, parts)
	assert.Equal(t, s, Join(parts))
}

func TestSplitJoinRoundTripMultibyte(t *testing.T) {
	s := multibyteText("日本語😀🚀a", 20000)
	parts := Split(s)
	assertParts(t, parts)
	assert.Equal(t, s, Join(parts))
}

func TestOverlapCoversBoundaryTerm(t *testing.T) {
	for _, s := range []string{asciiText(3*Step + 500), multibyteText("é中😀a", 3*Step+500)} {
		r := []rune(s)
		parts := Split(s)
		contains := func(window string) bool {
			for _, p := range parts {
				if strings.Contains(p, window) {
					return true
				}
			}
			return false
		}
		for start := 0; start+Overlap <= len(r); start += 13 {
			require.True(t, contains(string(r[start:start+Overlap])), "window at %d not inside any part", start)
		}
		// The windows that straddle each boundary exactly.
		end := 0
		for i, p := range parts {
			n := utf8.RuneCountInString(p) - len(Mark)
			if i > 0 {
				n -= Overlap
			}
			end += n
			for start := max(0, end-Overlap+1); start < end && start+Overlap <= len(r); start++ {
				require.True(t, contains(string(r[start:start+Overlap])), "boundary window at %d not inside any part", start)
			}
		}
	}
}

func TestNoPartIsValidJSON(t *testing.T) {
	obj := `{"a":"` + strings.Repeat("x", 251-len(`{"a":""}`)) + `"}`
	require.Equal(t, 251, len(obj))
	require.True(t, json.Valid([]byte(obj)))
	parts := Split(obj)
	require.NotEmpty(t, parts)
	for i, p := range parts {
		assert.False(t, json.Valid([]byte(p)), "part %d", i)
	}
}

func smallDeployment() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":            "web",
			"namespace":       "shop",
			"uid":             "uid-1",
			"resourceVersion": "42",
			"generation":      int64(3),
			"labels":          map[string]any{"tier": "api", "app": "web"},
			"ownerReferences": []any{
				map[string]any{"kind": "Thing", "name": "not-controller", "uid": "o-1"},
				map[string]any{"kind": "App", "name": "ctl", "uid": "o-2", "controller": true},
			},
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{"replicas": int64(2), "note": "<b>&</b>"},
		"status": map[string]any{
			"observedGeneration": int64(3),
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True"},
				map[string]any{"type": "ReplicaFailure", "status": "True", "reason": "Quota"},
			},
		},
	}
}

func smallConfigMap() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            "settings",
			"namespace":       "shop",
			"uid":             "uid-cm",
			"resourceVersion": "9",
		},
		"data": map[string]any{"filler": ""},
	}
}

// assertObjectRecord checks the attributes every object record and tombstone carries.
func assertObjectRecord(t *testing.T, r Record, cluster string, now time.Time) {
	t.Helper()
	assert.Equal(t, Category, r.Category)
	assert.Equal(t, ObjectType, r.Attributes["type"])
	assert.Equal(t, cluster, r.Attributes["k8s.cluster.name"])
	assert.Equal(t, now.Unix(), r.Attributes["timestamp"])
	assert.NotContains(t, r.Attributes, "logtype")
}

func TestBuildRecordsSmallObject(t *testing.T) {
	obj := smallDeployment()
	recs, err := BuildRecords(obj, appsDeployments, OpUpdated, "test-wet-mop-green", testNow)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	r := recs[0]
	a := r.Attributes

	assert.Equal(t, "Deployment shop/web updated rv 42", r.Summary)
	assertObjectRecord(t, r, "test-wet-mop-green", testNow)
	assert.Equal(t, "apps", a["k8s.group"])
	assert.Equal(t, "v1", a["k8s.version"])
	assert.Equal(t, "Deployment", a["k8s.kind"])
	assert.Equal(t, "deployments", a["k8s.resource"])
	assert.Equal(t, "shop", a["k8s.namespace"])
	assert.Equal(t, "web", a["k8s.name"])
	assert.Equal(t, "uid-1", a["k8s.uid"])
	assert.Equal(t, "42", a["k8s.resourceVersion"])
	assert.Equal(t, "updated", a["k8s.op"])
	assert.Equal(t, ",app=web,tier=api,", a["k8s.labels"])
	assert.NotContains(t, a, "k8s.labels.truncated")
	assert.Equal(t, int64(3), a["k8s.generation"])
	assert.Equal(t, int64(3), a["k8s.observedGeneration"])
	assert.Equal(t, true, a["k8s.ready"])
	assert.NotContains(t, a, "k8s.phase")
	assert.NotContains(t, a, "k8s.deletionTimestamp")
	assert.Equal(t, ",ReplicaFailure=True:Quota,", a["k8s.conditions.unhealthy"])
	assert.Equal(t, false, a["k8s.stale"])
	assert.Equal(t, true, a["k8s.unhealthy"])
	assert.Equal(t, Hash(obj), a["object.hash"])
	assert.Equal(t, 1, a["object.parts"])
	assert.Equal(t, 0, a["record.index"])
	assert.Equal(t, 1, a["record.count"])
	assert.Equal(t, 0, a["record.firstPart"])

	// The part holds compact JSON with HTML escaping off and managedFields kept.
	p0, ok := a["object.p000"].(string)
	require.True(t, ok)
	text := Join([]string{p0})
	assert.Contains(t, text, `"note":"<b>&</b>"`)
	assert.Contains(t, text, `"managedFields"`)
	assert.False(t, strings.HasSuffix(text, "\n"))
	sum := sha256.Sum256([]byte(text))
	assert.Equal(t, hex.EncodeToString(sum[:]), a["object.sha256"])
	assert.Equal(t, utf8.RuneCountInString(text), a["object.chars"])
	var back map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &back))
	assert.Equal(t, "web", back["metadata"].(map[string]any)["name"])
}

func TestBuildRecordsBodyIsTheRawObject(t *testing.T) {
	// Normalize drops metadata.generation and status.reconciledAt from an Argo CD
	// Application. The hash ignores them; the body and k8s.generation keep them.
	obj := argoApp()
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	recs, err := BuildRecords(obj, gvr, OpUpdated, "c", testNow)
	require.NoError(t, err)
	a := recs[0].Attributes
	text, err := serialize(obj)
	require.NoError(t, err)
	assert.Equal(t, text, Join([]string{a["object.p000"].(string)}))
	assert.Contains(t, text, `"reconciledAt"`)
	gen, ok := number(obj["metadata"].(map[string]any)["generation"])
	require.True(t, ok)
	assert.Equal(t, int64(gen), a["k8s.generation"])
	assert.Equal(t, Hash(obj), a["object.hash"])
}

func TestBuildRecordsOptionalAttributes(t *testing.T) {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":              "gone",
			"uid":               "uid-ns",
			"resourceVersion":   "7",
			"deletionTimestamp": "2026-11-06T10:00:00Z",
		},
		"status": map[string]any{"phase": "Terminating"},
	}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	recs, err := BuildRecords(obj, gvr, OpInitial, "c", testNow)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	a := recs[0].Attributes
	assert.Equal(t, "Namespace /gone initial rv 7", recs[0].Summary)
	assertObjectRecord(t, recs[0], "c", testNow)
	assert.Equal(t, "", a["k8s.group"])
	assert.Equal(t, "", a["k8s.namespace"])
	assert.Equal(t, "", a["k8s.labels"])
	assert.Equal(t, "", a["k8s.conditions.unhealthy"])
	assert.Equal(t, false, a["k8s.stale"])
	assert.Equal(t, false, a["k8s.unhealthy"])
	assert.Equal(t, "Terminating", a["k8s.phase"])
	assert.Equal(t, "2026-11-06T10:00:00Z", a["k8s.deletionTimestamp"])
	for _, k := range []string{"k8s.owner.kind", "k8s.owner.name", "k8s.owner.uid", "k8s.generation",
		"k8s.observedGeneration", "k8s.ready"} {
		assert.NotContains(t, a, k)
	}
}

func TestBuildRecordsResyncOp(t *testing.T) {
	recs, err := BuildRecords(smallDeployment(), appsDeployments, OpResync, "c", testNow)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "resync", recs[0].Attributes["k8s.op"])
	assert.Equal(t, "Deployment shop/web resync rv 42", recs[0].Summary)
}

func TestBuildRecordsLabelsTruncated(t *testing.T) {
	obj := smallDeployment()
	labels := map[string]any{}
	for i := 0; i < 400; i++ {
		labels[fmt.Sprintf("key-%04d", i)] = "value-value"
	}
	obj["metadata"].(map[string]any)["labels"] = labels
	recs, err := BuildRecords(obj, appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	a := recs[0].Attributes
	l := a["k8s.labels"].(string)
	assert.LessOrEqual(t, utf8.RuneCountInString(l), PartChars)
	assert.LessOrEqual(t, len(l), MaxAttrBytes)
	assert.True(t, strings.HasPrefix(l, ",key-0000=value-value,"))
	assert.True(t, strings.HasSuffix(l, ","), "truncation keeps whole labels")
	assert.Equal(t, true, a["k8s.labels.truncated"])
}

func TestBuildRecordsConditionsUnhealthyTruncated(t *testing.T) {
	obj := smallDeployment()
	recs, err := BuildRecords(obj, appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	_, present := recs[0].Attributes["k8s.conditions.unhealthy.truncated"]
	assert.False(t, present)

	var conds []any
	for i := 0; i < 300; i++ {
		conds = append(conds, map[string]any{"type": fmt.Sprintf("Check%04d", i), "status": "False", "reason": "SomeLongReasonText"})
	}
	obj["status"].(map[string]any)["conditions"] = conds
	recs, err = BuildRecords(obj, appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	a := recs[0].Attributes
	u := a["k8s.conditions.unhealthy"].(string)
	assert.LessOrEqual(t, utf8.RuneCountInString(u), PartChars)
	assert.True(t, strings.HasSuffix(u, ","))
	assert.Equal(t, true, a["k8s.conditions.unhealthy.truncated"])
	assert.Equal(t, true, a["k8s.unhealthy"])
}

func TestBuildRecordsSpansRecords(t *testing.T) {
	obj := smallDeployment()
	// Grow the object until its JSON needs 111 parts.
	target := 110*Step + 1
	obj["spec"].(map[string]any)["filler"] = ""
	withEmpty, err := serialize(obj)
	require.NoError(t, err)
	obj["spec"].(map[string]any)["filler"] = strings.Repeat("f", target-utf8.RuneCountInString(withEmpty))
	text, err := serialize(obj)
	require.NoError(t, err)
	require.Equal(t, target, utf8.RuneCountInString(text))

	recs, err := BuildRecords(obj, appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	first := countParts(recs[0].Attributes)
	// About 4,111 bytes per full part: the byte cap closes record 0 before the part cap.
	assert.Equal(t, 109, first)
	assert.Equal(t, 0, recs[0].Attributes["record.firstPart"])
	assert.Equal(t, first, recs[1].Attributes["record.firstPart"])

	for i, r := range recs {
		assert.Equal(t, 111, r.Attributes["object.parts"], "record %d", i)
		assert.Equal(t, recs[0].Attributes["timestamp"], r.Attributes["timestamp"])
		assert.Equal(t, recs[0].Attributes["object.sha256"], r.Attributes["object.sha256"])
		assertObjectRecord(t, r, "c", testNow)
	}
	assert.Contains(t, recs[1].Attributes, partKey(first))
	assert.NotContains(t, recs[0].Attributes, partKey(first))
	assert.Contains(t, recs[1].Attributes, "object.p110")
	assertRecordsRebuild(t, obj, recs)
}

func TestPartCapBindsForShortParts(t *testing.T) {
	parts := make([]string, 250)
	for i := range parts {
		parts[i] = Mark + asciiText(9)
	}
	groups := mustGroup(t, parts)
	assert.Equal(t, []partGroup{{0, 110}, {110, 220}, {220, 250}}, groups)
}

func TestPartBoundaries(t *testing.T) {
	// 0 parts: Split("") is empty and groupParts(nil, base) still gives one group, the fork's rule.
	require.Empty(t, Split(""))
	require.Len(t, mustGroup(t, nil), 1)
	for _, tc := range []struct {
		name      string
		parts     int
		multibyte bool
	}{
		{"one", 1, false}, {"110 ascii", 110, false}, {"111 ascii", 111, false},
		{"110 multibyte", 110, true}, {"111 multibyte", 111, true},
	} {
		obj := configMapForParts(t, tc.parts, tc.multibyte)
		recs, err := BuildRecords(obj, cmGVR, OpInitial, "c", time.Unix(1, 0))
		require.NoError(t, err)
		assert.Equal(t, tc.parts, recs[0].Attributes["object.parts"], tc.name)
		for _, r := range recs {
			assert.LessOrEqual(t, countParts(r.Attributes), PartsPerRecord, tc.name)
			assert.LessOrEqual(t, mustMarshalLen(t, r), MaxRecordBytes, tc.name)
		}
		// 1 part gives one record. 110 and 111 full parts give two: about 4,111 bytes per
		// full part means the 450,000-byte cap closes record 0 after 109 parts, before the part cap.
		assert.Equal(t, map[int]int{1: 1, 110: 2, 111: 2}[tc.parts], len(recs), tc.name)
		assertRecordsRebuild(t, obj, recs)
	}
}

func TestNoStringAttributeExceedsForwarderLimit(t *testing.T) {
	obj := smallConfigMap()
	labels := map[string]any{}
	for i := 0; i < 300; i++ {
		labels[fmt.Sprintf("k%03d", i)] = strings.Repeat("中", 20)
	}
	obj["metadata"].(map[string]any)["labels"] = labels
	obj["data"].(map[string]any)["filler"] = multibyteText("é中😀", 30_000)

	recs, err := BuildRecords(obj, cmGVR, OpAdded, "c", testNow)
	require.NoError(t, err)
	assert.Equal(t, true, recs[0].Attributes["k8s.labels.truncated"])
	for i, r := range recs {
		for k, v := range r.Attributes {
			if s, ok := v.(string); ok {
				assert.LessOrEqual(t, len(s), MaxAttrBytes, "record %d attribute %s", i, k)
			}
		}
	}
	assertRecordsRebuild(t, obj, recs)
}

func TestRecordTypeAndCategory(t *testing.T) {
	recs, err := BuildRecords(smallDeployment(), appsDeployments, OpAdded, "c1", testNow)
	require.NoError(t, err)
	for _, r := range recs {
		assertObjectRecord(t, r, "c1", testNow)
	}
	ts := Tombstone("uid-1", Entry{GVR: "apps/v1/deployments", Kind: "Deployment", Namespace: "shop", Name: "web"}, "c1", testNow)
	assertObjectRecord(t, ts, "c1", testNow)

	// The JSON shape is the SDK v3 event: summary, category and attributes.
	var shape map[string]any
	require.NoError(t, json.Unmarshal(mustMarshal(t, ts), &shape))
	assert.ElementsMatch(t, []string{"summary", "category", "attributes"}, keys(shape))
}

func TestBuildRecordsOwnerController(t *testing.T) {
	recs, err := BuildRecords(smallDeployment(), appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	a := recs[0].Attributes
	assert.Equal(t, "App", a["k8s.owner.kind"])
	assert.Equal(t, "ctl", a["k8s.owner.name"])
	assert.Equal(t, "o-2", a["k8s.owner.uid"])

	obj := smallDeployment()
	obj["metadata"].(map[string]any)["ownerReferences"] = []any{
		map[string]any{"kind": "Thing", "name": "n", "uid": "o-1", "controller": false},
	}
	recs, err = BuildRecords(obj, appsDeployments, OpAdded, "c", testNow)
	require.NoError(t, err)
	for _, k := range []string{"k8s.owner.kind", "k8s.owner.name", "k8s.owner.uid"} {
		assert.NotContains(t, recs[0].Attributes, k)
	}
}

func TestTombstone(t *testing.T) {
	e := Entry{GVR: "apps/v1/deployments", Kind: "Deployment", Namespace: "shop", Name: "web", Hash: "abc", LastSent: 5}
	r := Tombstone("uid-1", e, "c", testNow)
	a := r.Attributes
	assert.Equal(t, "Deployment shop/web deleted", r.Summary)
	assertObjectRecord(t, r, "c", testNow)
	assert.Equal(t, "apps", a["k8s.group"])
	assert.Equal(t, "v1", a["k8s.version"])
	assert.Equal(t, "deployments", a["k8s.resource"])
	assert.Equal(t, "Deployment", a["k8s.kind"])
	assert.Equal(t, "shop", a["k8s.namespace"])
	assert.Equal(t, "web", a["k8s.name"])
	assert.Equal(t, "uid-1", a["k8s.uid"])
	assert.Equal(t, "deleted", a["k8s.op"])
	assert.Equal(t, 0, a["object.parts"])
	assert.Equal(t, 0, a["record.index"])
	assert.Equal(t, 1, a["record.count"])
	assert.Equal(t, 0, a["record.firstPart"])
	assert.Zero(t, countParts(a), "tombstone carries no parts")

	core := Tombstone("u", Entry{GVR: "/v1/namespaces", Kind: "Namespace", Name: "gone"}, "c", testNow)
	assert.Equal(t, "Namespace /gone deleted", core.Summary)
	assert.Equal(t, "", core.Attributes["k8s.group"])
}

func TestTombstoneUsesObservedTime(t *testing.T) {
	e := Entry{GVR: "/v1/configmaps", Kind: "ConfigMap", Namespace: "shop", Name: "settings"}
	r := Tombstone("uid-cm", e, "c", time.Unix(1700000000, 0))
	assert.Equal(t, int64(1700000000), r.Attributes["timestamp"])
}

func TestGVRStringParse(t *testing.T) {
	cases := map[string]schema.GroupVersionResource{
		"/v1/configmaps":                    cmGVR,
		"apps/v1/deployments":               appsDeployments,
		"argoproj.io/v1alpha1/applications": {Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"},
	}
	for s, gvr := range cases {
		assert.Equal(t, s, GVRString(gvr))
		got, err := ParseGVR(s)
		require.NoError(t, err)
		assert.Equal(t, gvr, got)
	}
	for _, bad := range []string{"", "v1/configmaps", "a/b/c/d", "apps//deployments", "apps/v1/"} {
		_, err := ParseGVR(bad)
		assert.Error(t, err, bad)
	}
}

// assertRecordsRebuild checks that every record encodes within MaxRecordBytes, that the
// record.* attributes are consistent with variable parts per record, and that the parts of
// all records, joined in index order, rebuild the object's serialized JSON and match
// object.sha256.
func assertRecordsRebuild(t *testing.T, obj map[string]any, recs []Record) {
	t.Helper()
	text, err := serialize(obj)
	require.NoError(t, err)
	nparts := recs[0].Attributes["object.parts"].(int)
	var parts []string
	for i, r := range recs {
		assert.LessOrEqual(t, mustMarshalLen(t, r), MaxRecordBytes, "record %d", i)
		assert.Less(t, len(r.Attributes), 254, "record %d", i)
		assert.Equal(t, i, r.Attributes["record.index"])
		assert.Equal(t, len(recs), r.Attributes["record.count"])
		assert.Equal(t, len(parts), r.Attributes["record.firstPart"], "record %d starts where the last ended", i)
		n := 0
		for j := len(parts); ; j++ {
			p, ok := r.Attributes[partKey(j)]
			if !ok {
				break
			}
			parts = append(parts, p.(string))
			n++
		}
		assert.Positive(t, n, "record %d carries parts", i)
		assert.Equal(t, n, countParts(r.Attributes), "record %d holds only consecutive parts", i)
		assert.LessOrEqual(t, n, PartsPerRecord)
	}
	require.Len(t, parts, nparts)
	joined := Join(parts)
	assert.Equal(t, text, joined)
	sum := sha256.Sum256([]byte(joined))
	assert.Equal(t, hex.EncodeToString(sum[:]), recs[0].Attributes["object.sha256"])
}

// fillToParts sets the filler through set to the longest prefix of text for which Split
// cuts obj's serialized JSON into n parts, so the last part is full.
func fillToParts(t *testing.T, obj map[string]any, set func(string), text string, n int) {
	t.Helper()
	r := []rune(text)
	partsAt := func(l int) int {
		set(string(r[:l]))
		s, err := serialize(obj)
		require.NoError(t, err)
		return len(Split(s))
	}
	// Find the shortest prefix that gives more than n parts; one rune less gives n.
	lo, hi := 0, len(r)
	require.Greater(t, partsAt(hi), n, "text too short for %d full parts", n)
	for lo < hi {
		mid := (lo + hi) / 2
		if partsAt(mid) > n {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	require.Equal(t, n, partsAt(lo-1))
}

// configMapForParts builds a ConfigMap whose serialized JSON Split cuts into exactly parts
// full parts. With multibyte its data mixes 'é', '中' and '😀', so the byte cap ends every part.
func configMapForParts(t *testing.T, parts int, multibyte bool) map[string]any {
	t.Helper()
	obj := smallConfigMap()
	data := obj["data"].(map[string]any)
	text := asciiText((parts + 2) * Step)
	if multibyte {
		text = multibyteText("é中😀", (parts+2)*Step)
	}
	fillToParts(t, obj, func(s string) { data["filler"] = s }, text, parts)
	return obj
}

func TestBuildRecordsEscapeHeavyUnderPostLimit(t *testing.T) {
	obj := smallDeployment()
	spec := obj["spec"].(map[string]any)
	spec["filler"] = ""
	empty, err := serialize(obj)
	require.NoError(t, err)
	// Each `"` or newline serializes to two runes, so this fills the object to 110 parts.
	n := (110*Step - utf8.RuneCountInString(empty)) / 2
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			b.WriteString(`"`)
		} else {
			b.WriteString("\n")
		}
	}
	spec["filler"] = b.String()

	recs, err := BuildRecords(obj, appsDeployments, OpUpdated, "c", testNow)
	require.NoError(t, err)
	require.Equal(t, 110, recs[0].Attributes["object.parts"])
	assert.Greater(t, len(recs), 1, "110 escape-heavy parts do not fit one record")
	assertRecordsRebuild(t, obj, recs)
}

func TestBuildRecordsMultibyteUnderPostLimit(t *testing.T) {
	obj := smallDeployment()
	spec := obj["spec"].(map[string]any)
	fillToParts(t, obj, func(s string) { spec["filler"] = s }, multibyteText("日本語😀🚀漢字", 112*Step), 110)

	recs, err := BuildRecords(obj, appsDeployments, OpUpdated, "c", testNow)
	require.NoError(t, err)
	require.Equal(t, 110, recs[0].Attributes["object.parts"])
	assert.Greater(t, len(recs), 1)
	assertRecordsRebuild(t, obj, recs)
}

// mustGroup runs groupParts with a small base record.
func mustGroup(t *testing.T, parts []string) []partGroup {
	t.Helper()
	base := Record{Summary: "ConfigMap shop/settings initial rv 9", Category: Category, Attributes: map[string]any{"type": ObjectType}}
	groups, err := groupParts(parts, base)
	require.NoError(t, err)
	return groups
}

// countParts counts the object.pNNN attributes.
func countParts(attrs map[string]any) int {
	n := 0
	for k := range attrs {
		if suffix, ok := strings.CutPrefix(k, "object.p"); ok && suffix != "" && strings.Trim(suffix, "0123456789") == "" {
			n++
		}
	}
	return n
}

func mustMarshal(t *testing.T, r Record) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	return b
}

func mustMarshalLen(t *testing.T, r Record) int {
	t.Helper()
	return len(mustMarshal(t, r))
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
