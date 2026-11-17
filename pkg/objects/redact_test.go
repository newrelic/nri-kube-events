package objects

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

var redactedRE = regexp.MustCompile(`^<redacted len=\d+ hmac=[0-9a-f]{16}>$`)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func expectRedacted(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return fmt.Sprintf("<redacted len=%d hmac=%s>", len(value), hex.EncodeToString(m.Sum(nil))[:16])
}

func secret(data, stringData map[string]any) map[string]any {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "db", "namespace": "default"},
		"type":       "Opaque",
	}
	if data != nil {
		obj["data"] = data
	}
	if stringData != nil {
		obj["stringData"] = stringData
	}
	return obj
}

func TestRedactDataAndStringData(t *testing.T) {
	key := []byte("cluster-key-1")
	obj := secret(
		map[string]any{"password": b64("hunter2"), "user": b64("admin"), "copy": b64("hunter2")},
		map[string]any{"token": "plain-token"},
	)
	out := Redact(obj, key)

	data := out["data"].(map[string]any)
	require.Len(t, data, 3)
	assert.Equal(t, expectRedacted(key, "hunter2"), data["password"])
	assert.Equal(t, expectRedacted(key, "admin"), data["user"])
	assert.Equal(t, data["password"], data["copy"], "same value gives the same hmac")
	assert.Regexp(t, redactedRE, data["password"])
	assert.Contains(t, data["password"], "len=7 ")

	sd := out["stringData"].(map[string]any)
	assert.Equal(t, expectRedacted(key, "plain-token"), sd["token"])

	other := Redact(obj, []byte("cluster-key-2"))
	assert.NotEqual(t, data["password"], other["data"].(map[string]any)["password"], "different key gives a different hmac")

	assert.Equal(t, "db", out["metadata"].(map[string]any)["name"])
	assert.Equal(t, "Opaque", out["type"])
}

func TestRedactInvalidBase64UsesRawValue(t *testing.T) {
	key := []byte("k")
	out := Redact(secret(map[string]any{"bad": "not base64!!"}, nil), key)
	assert.Equal(t, expectRedacted(key, "not base64!!"), out["data"].(map[string]any)["bad"])
}

func TestRedactLastAppliedSecretValues(t *testing.T) {
	key := []byte("k")
	applied := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "db", "namespace": "default"},
		"data":       map[string]any{"password": b64("hunter2")},
		"stringData": map[string]any{"token": "plain-token"},
	}
	raw, err := json.Marshal(applied)
	require.NoError(t, err)

	obj := secret(map[string]any{"password": b64("hunter2")}, nil)
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{
		lastApplied: string(raw) + "\n",
		"team":      "payments",
	}
	out := Redact(obj, key)

	ann := out["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, "payments", ann["team"])
	s := ann[lastApplied].(string)
	assert.NotContains(t, s, b64("hunter2"))
	assert.NotContains(t, s, "plain-token")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &parsed))
	assert.Equal(t, expectRedacted(key, "hunter2"), parsed["data"].(map[string]any)["password"])
	assert.Equal(t, expectRedacted(key, "plain-token"), parsed["stringData"].(map[string]any)["token"])
	assert.Equal(t, "db", parsed["metadata"].(map[string]any)["name"])
}

func TestRedactLastAppliedUnparseableIsReplaced(t *testing.T) {
	obj := secret(nil, nil)
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{lastApplied: `{"kind":"Secret","data":{"p":"aHVudGVyMg=="`}
	out := Redact(obj, []byte("k"))
	s := out["metadata"].(map[string]any)["annotations"].(map[string]any)[lastApplied].(string)
	assert.NotContains(t, s, "aHVudGVyMg==")
	assert.Regexp(t, redactedRE, s)
}

func TestRedactHelmRelease(t *testing.T) {
	key := []byte("k")
	release := b64("H4sIAAAAAAAA-gzipped-release-payload")
	obj := secret(map[string]any{"release": release}, nil)
	obj["type"] = "helm.sh/release.v1"
	obj["metadata"].(map[string]any)["labels"] = map[string]any{"owner": "helm", "name": "web"}

	out := Redact(obj, key)
	got := out["data"].(map[string]any)["release"]
	assert.Equal(t, expectRedacted(key, "H4sIAAAAAAAA-gzipped-release-payload"), got)
	assert.Equal(t, "helm.sh/release.v1", out["type"])
	assert.Equal(t, "helm", out["metadata"].(map[string]any)["labels"].(map[string]any)["owner"])
}

func TestRedactLeavesInputUntouched(t *testing.T) {
	obj := secret(map[string]any{"password": b64("hunter2")}, map[string]any{"token": "plain-token"})
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{
		lastApplied: `{"kind":"Secret","data":{"password":"aHVudGVyMg=="}}`,
	}
	before, err := json.Marshal(obj)
	require.NoError(t, err)

	out := Redact(obj, []byte("k"))
	after, err := json.Marshal(obj)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))

	out["metadata"].(map[string]any)["name"] = "changed"
	assert.Equal(t, "db", obj["metadata"].(map[string]any)["name"], "output shares no maps with input")
}

func TestRedactIgnoresNonSecret(t *testing.T) {
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":        "cfg",
			"annotations": map[string]any{lastApplied: `{"kind":"ConfigMap","data":{"k":"v"}}`},
		},
		"data": map[string]any{"k": "v"},
	}
	out := Redact(cm, []byte("k"))
	assert.Equal(t, cm, out)
	out["data"].(map[string]any)["k"] = "changed"
	assert.Equal(t, "v", cm["data"].(map[string]any)["k"], "a non-Secret is still a deep copy")
}

func TestRedactNonAnyMapFieldsReplacedWhole(t *testing.T) {
	obj := secret(nil, nil)
	obj["data"] = map[string][]byte{"password": []byte("hunter2")}
	obj["stringData"] = map[string]string{"token": "plain-token"}
	out := Redact(obj, []byte("k"))
	assert.Equal(t, "<redacted>", out["data"])
	assert.Equal(t, "<redacted>", out["stringData"])
	assert.Equal(t, map[string][]byte{"password": []byte("hunter2")}, obj["data"], "input untouched")
	assert.Equal(t, map[string]string{"token": "plain-token"}, obj["stringData"], "input untouched")

	other := secret(nil, nil)
	other["data"] = "aHVudGVyMg=="
	other["stringData"] = []any{"plain-token"}
	out = Redact(other, []byte("k"))
	assert.Equal(t, "<redacted>", out["data"])
	assert.Equal(t, "<redacted>", out["stringData"])
}

func TestRedactLastAppliedNonMapFieldsReplacedWhole(t *testing.T) {
	obj := secret(nil, nil)
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{
		lastApplied: `{"kind":"Secret","data":"aHVudGVyMg==","stringData":["plain-token"]}`,
	}
	out := Redact(obj, []byte("k"))
	s := out["metadata"].(map[string]any)["annotations"].(map[string]any)[lastApplied].(string)
	assert.NotContains(t, s, "aHVudGVyMg==")
	assert.NotContains(t, s, "plain-token")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &parsed))
	assert.Equal(t, "<redacted>", parsed["data"])
	assert.Equal(t, "<redacted>", parsed["stringData"])
}

func TestRedactStringStringAnnotations(t *testing.T) {
	obj := secret(map[string]any{"password": b64("hunter2")}, nil)
	ann := map[string]string{lastApplied: `{"kind":"Secret","data":{"password":"aHVudGVyMg=="}}`, "team": "payments"}
	obj["metadata"].(map[string]any)["annotations"] = ann
	out := Redact(obj, []byte("k"))
	got := out["metadata"].(map[string]any)["annotations"].(map[string]string)
	assert.NotContains(t, got[lastApplied], "aHVudGVyMg==")
	assert.Equal(t, "payments", got["team"])
	assert.Contains(t, ann[lastApplied], "aHVudGVyMg==", "input untouched")
}

func TestRedactEmptyKeyOmitsHmac(t *testing.T) {
	obj := secret(map[string]any{"password": b64("hunter2")}, map[string]any{"token": "plain-token"})
	for _, key := range [][]byte{nil, {}} {
		out := Redact(obj, key)
		assert.Equal(t, "<redacted len=7>", out["data"].(map[string]any)["password"])
		assert.Equal(t, "<redacted len=11>", out["stringData"].(map[string]any)["token"])
	}
}

func TestRedactLastAppliedAnyKindOnSecret(t *testing.T) {
	key := []byte("k")
	for name, raw := range map[string]string{
		"non-secret kind": `{"kind":"ConfigMap","data":{"password":"aHVudGVyMg=="},"stringData":{"token":"plain-token"}}`,
		"no kind":         `{"data":{"password":"aHVudGVyMg=="},"stringData":{"token":"plain-token"}}`,
	} {
		obj := secret(nil, nil)
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{lastApplied: raw}
		out := Redact(obj, key)
		s := out["metadata"].(map[string]any)["annotations"].(map[string]any)[lastApplied].(string)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(s), &parsed), name)
		assert.Equal(t, expectRedacted(key, "hunter2"), parsed["data"].(map[string]any)["password"], name)
		assert.Equal(t, expectRedacted(key, "plain-token"), parsed["stringData"].(map[string]any)["token"], name)
	}
}

// The marker inside last-applied-configuration reads as it does in data: a JSON encoder
// with HTML escaping would write <redacted instead.
func TestRedactLastAppliedMarkerNotEscaped(t *testing.T) {
	key := []byte("k")
	obj := secret(map[string]any{"password": b64("hunter2")}, nil)
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{
		lastApplied: `{"kind":"Secret","data":{"password":"aHVudGVyMg=="}}`,
	}
	out := Redact(obj, key)
	s := out["metadata"].(map[string]any)["annotations"].(map[string]any)[lastApplied].(string)
	assert.Contains(t, s, expectRedacted(key, "hunter2"))
	assert.Contains(t, s, "<redacted")
	assert.NotContains(t, s, "\\u003c")
	assert.False(t, strings.HasSuffix(s, "\n"), "no trailing newline from the encoder")
}

// RedactSecret redacts whatever the object's kind field says, so a watch object that
// arrives without kind is still redacted.
func TestRedactSecretIgnoresKindField(t *testing.T) {
	key := []byte("k")
	obj := secret(map[string]any{"password": b64("hunter2")}, map[string]any{"token": "plain-token"})
	delete(obj, "kind")
	delete(obj, "apiVersion")
	out := RedactSecret(obj, key)
	assert.Equal(t, expectRedacted(key, "hunter2"), out["data"].(map[string]any)["password"])
	assert.Equal(t, expectRedacted(key, "plain-token"), out["stringData"].(map[string]any)["token"])
	assert.Equal(t, b64("hunter2"), obj["data"].(map[string]any)["password"], "input untouched")
}
