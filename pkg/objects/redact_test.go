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

func TestRedactConfigMapValuesKeepsKeys(t *testing.T) {
	key := []byte("k")
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":        "cfg",
			"labels":      map[string]any{"app": "web"},
			"annotations": map[string]any{lastApplied: `{"apiVersion":"v1","kind":"ConfigMap","data":{"k":"v"}}`},
		},
		"data":       map[string]any{"k": "v", "db.conf": "host=db"},
		"binaryData": map[string]any{"blob": b64("raw")},
	}
	out := Redact(cm, key)
	data := out["data"].(map[string]any)
	assert.Equal(t, expectRedacted(key, "v"), data["k"])
	assert.Equal(t, expectRedacted(key, "host=db"), data["db.conf"])
	assert.Equal(t, expectRedacted(key, "raw"), out["binaryData"].(map[string]any)["blob"])
	meta := out["metadata"].(map[string]any)
	assert.Equal(t, map[string]any{"app": "web"}, meta["labels"], "labels are kept")
	assert.Contains(t, meta["annotations"].(map[string]any)[lastApplied], expectRedacted(key, "v"))
	assert.NotContains(t, meta["annotations"].(map[string]any)[lastApplied], `"k":"v"`)
	assert.Equal(t, "v", cm["data"].(map[string]any)["k"], "the input is untouched")

	changed := Redact(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": map[string]any{"k": "v2"}}, key)
	assert.NotEqual(t, Hash(out), Hash(changed), "a changed value still changes the hash")
}

func TestRedactContainerEnvValuesKeepsNamesAndRefs(t *testing.T) {
	key := []byte("k")
	ref := map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "password"}}
	container := func() map[string]any {
		return map[string]any{
			"name":  "app",
			"image": "nginx:1.27",
			"env": []any{
				map[string]any{"name": "DB_PASSWORD", "value": "hunter2"},
				map[string]any{"name": "FROM_SECRET", "valueFrom": ref},
			},
		}
	}
	deploy := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "web"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers":     []any{container()},
			"initContainers": []any{container()},
		}}},
	}
	out := Redact(deploy, key)
	podSpec := out["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	for _, field := range []string{"containers", "initContainers"} {
		c := podSpec[field].([]any)[0].(map[string]any)
		env := c["env"].([]any)
		assert.Equal(t, "DB_PASSWORD", env[0].(map[string]any)["name"])
		assert.Equal(t, expectRedacted(key, "hunter2"), env[0].(map[string]any)["value"], field)
		assert.Equal(t, map[string]any{"name": "FROM_SECRET", "valueFrom": ref}, env[1], "valueFrom is a reference and is kept")
		assert.Equal(t, "nginx:1.27", c["image"])
	}
}

func TestRedactCredentialNamedFieldsAtAnyDepth(t *testing.T) {
	key := []byte("k")
	cr := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Database",
		"metadata":   map[string]any{"name": "db"},
		"spec": map[string]any{
			"host": "db.example.com",
			"auth": map[string]any{
				"adminPassword": "hunter2",
				"apiKey":        "abc123",
				"clientCert":    "cert-bytes",
				"bearerToken":   map[string]any{"value": "tok", "name": "kept-name"},
				"secretRef":     map[string]any{"name": "db-secret"},
				"secretName":    "db-secret",
				"tokens":        []any{"t1", "t2"},
			},
			"selector": map[string]any{"matchExpressions": []any{map[string]any{"key": "app", "operator": "In"}}},
		},
	}
	out := Redact(cr, key)
	spec := out["spec"].(map[string]any)
	auth := spec["auth"].(map[string]any)
	assert.Equal(t, "db.example.com", spec["host"])
	assert.Equal(t, expectRedacted(key, "hunter2"), auth["adminPassword"])
	assert.Equal(t, expectRedacted(key, "abc123"), auth["apiKey"])
	assert.Equal(t, expectRedacted(key, "cert-bytes"), auth["clientCert"])
	bearer := auth["bearerToken"].(map[string]any)
	assert.Equal(t, expectRedacted(key, "tok"), bearer["value"], "every string under a credential field")
	assert.Equal(t, "kept-name", bearer["name"], "identifiers under a credential field are kept")
	assert.Equal(t, map[string]any{"name": "db-secret"}, auth["secretRef"], "references are kept")
	assert.Equal(t, "db-secret", auth["secretName"])
	assert.Equal(t, []any{expectRedacted(key, "t1"), expectRedacted(key, "t2")}, auth["tokens"])
	assert.Equal(t, "app", spec["selector"].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)["key"], "a selector key is not a credential")
}

func TestRedactKeepsSecretVolumeReferences(t *testing.T) {
	key := []byte("k")
	vol := map[string]any{"name": "creds", "secret": map[string]any{
		"secretName": "db", "defaultMode": int64(420), "items": []any{map[string]any{"key": "password", "path": "pw"}},
	}}
	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "web"},
		"spec":       map[string]any{"automountServiceAccountToken": false, "volumes": []any{deepCopy(vol)}},
	}
	out := Redact(pod, key)
	spec := out["spec"].(map[string]any)
	assert.Equal(t, vol, spec["volumes"].([]any)[0], "a Secret volume names the Secret and holds no value")
	assert.Equal(t, false, spec["automountServiceAccountToken"])
}

func TestRedactCredentialLookingValues(t *testing.T) {
	key := []byte("k")
	// The fake credentials are built by concatenation so secret scanners do not flag them.
	pem := "PRIVATE " + "KEY-----"
	//nolint:gosec // G101: test values, not credentials
	for name, value := range map[string]string{
		"private key":    "-----BEGIN RSA " + pem + "\nMIIE\n-----END RSA " + pem,
		"jwt":            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig",
		"aws access key": "id " + "AKIA" + "ABCDEFGHIJKLMNOP",
		"url password":   "postgres://app:hunter2@db:5432/app",
		"assignment":     "replicas: 2\npassword: hunter2\n",
	} {
		obj := map[string]any{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "Application",
			"metadata":   map[string]any{"name": "app", "annotations": map[string]any{"example.com/note": value}},
			"spec":       map[string]any{"source": map[string]any{"helm": map[string]any{"values": value}}},
		}
		out := Redact(obj, key)
		assert.Equal(t, expectRedacted(key, value), out["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["values"], name)
		assert.Equal(t, expectRedacted(key, value), out["metadata"].(map[string]any)["annotations"].(map[string]any)["example.com/note"], name)
	}
	plain := map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "web", "annotations": map[string]any{"example.com/owner": "team-a"}},
		"spec":       map[string]any{"type": "ClusterIP", "externalName": "db.example.com:5432"},
		"status":     map[string]any{"lastTransitionTime": "2026-11-17T10:00:00Z", "url": "https://example.com/path"},
	}
	assert.Equal(t, plain, Redact(plain, key), "ordinary values are kept")
}

func TestRedactCredentialNamedAnnotation(t *testing.T) {
	key := []byte("k")
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{"name": "sa", "annotations": map[string]any{
			"example.com/api-token": "abc",
			"example.com/owner":     "team-a",
		}},
	}
	ann := Redact(obj, key)["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, expectRedacted(key, "abc"), ann["example.com/api-token"])
	assert.Equal(t, "team-a", ann["example.com/owner"])
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
	//nolint:gosec // G101: test values, not credentials
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

func TestRedactSecretNonMapMetadataReplacedWhole(t *testing.T) {
	obj := secret(map[string]any{"password": b64("hunter2")}, nil)
	obj["metadata"] = map[string]string{"name": "db", "annotations": `{"data":{"password":"aHVudGVyMg=="}}`}
	out := Redact(obj, []byte("k"))
	assert.Equal(t, redactedWhole, out["metadata"])
	assert.IsType(t, map[string]string{}, obj["metadata"], "input untouched")

	obj = secret(map[string]any{"password": b64("hunter2")}, nil)
	delete(obj, "metadata")
	out = Redact(obj, []byte("k"))
	assert.NotContains(t, out, "metadata")
}

func TestRedactAppliedObjectAnnotations(t *testing.T) {
	key := []byte("k")
	kapp := `{"apiVersion":"v1","kind":"Secret","data":{"password":"aHVudGVyMg=="}}`
	yamlCopy := "apiVersion: v1\nkind: Secret\nstringData:\n  password: hunter2\n"
	long := strings.Repeat("a", 300)
	ann := map[string]any{
		"kapp.k14s.io/original":              kapp,
		"example.com/applied-yaml":           yamlCopy,
		"example.com/long":                   long,
		"kubernetes.io/service-account.name": "builder",
		"example.com/count":                  int64(3),
	}
	for _, annotations := range []any{ann, toStringMap(ann)} {
		obj := secret(map[string]any{"password": b64("hunter2")}, nil)
		obj["metadata"].(map[string]any)["annotations"] = annotations
		out := Redact(obj, key)
		got := map[string]string{}
		switch a := out["metadata"].(map[string]any)["annotations"].(type) {
		case map[string]any:
			for k, v := range a {
				got[k] = v.(string)
			}
		case map[string]string:
			got = a
		}
		assert.Equal(t, expectRedacted(key, kapp), got["kapp.k14s.io/original"])
		assert.Equal(t, expectRedacted(key, yamlCopy), got["example.com/applied-yaml"])
		assert.Equal(t, expectRedacted(key, long), got["example.com/long"])
		assert.Equal(t, "builder", got["kubernetes.io/service-account.name"])
	}
	// A non-string value under map[string]any fails closed.
	obj := secret(nil, nil)
	obj["metadata"].(map[string]any)["annotations"] = ann
	out := Redact(obj, key)
	assert.Equal(t, redactedWhole, out["metadata"].(map[string]any)["annotations"].(map[string]any)["example.com/count"])
	assert.Equal(t, kapp, ann["kapp.k14s.io/original"], "input untouched")
}

func TestRedactKeepsShortScalarAnnotations(t *testing.T) {
	obj := secret(nil, nil)
	obj["metadata"].(map[string]any)["annotations"] = map[string]any{
		"kubernetes.io/service-account.name": "builder",
		"example.com/list":                   "- a\n- b",
		"example.com/exact":                  strings.Repeat("中", 256),
	}
	out := Redact(obj, []byte("k"))
	got := out["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, "builder", got["kubernetes.io/service-account.name"])
	assert.Equal(t, "- a\n- b", got["example.com/list"])
	assert.Equal(t, strings.Repeat("中", 256), got["example.com/exact"], "256 runes is not longer than 256")
}

// Short structured values that are not one YAML mapping still hold objects: a multi-document
// stream and a JSON array of objects are redacted.
func TestRedactShortMultiDocumentAndArrayAnnotations(t *testing.T) {
	key := []byte("k")
	values := map[string]string{
		"example.com/multi-doc":      "---\npassword: hunter2\n",
		"example.com/second-doc":     "note\n---\npassword: hunter2\n",
		"example.com/json-array":     `[{"password":"hunter2"}]`,
		"example.com/nested-array":   `[["a", {"password":"hunter2"}]]`,
		"example.com/broken-2nd-doc": "note\n---\npassword: [hunter2\n",
	}
	ann := map[string]any{}
	for k, v := range values {
		ann[k] = v
	}
	obj := secret(nil, nil)
	obj["metadata"].(map[string]any)["annotations"] = ann
	got := Redact(obj, key)["metadata"].(map[string]any)["annotations"].(map[string]any)
	for k, v := range values {
		assert.Equal(t, expectRedacted(key, v), got[k], k)
	}
	assert.True(t, isYAMLMapping("a\n---\nb: c"))
	assert.False(t, isYAMLMapping(`["a", "b"]`))
	assert.False(t, isYAMLMapping("a\n---\nb"))
	assert.False(t, isYAMLMapping("foo: bar: baz"), "a first document that does not parse is plain text")
}

func toStringMap(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// The six cases a review probe found unredacted, plus the prose and marker rules.
func TestRedactReviewProbeCases(t *testing.T) {
	key := []byte("k")
	secretValue := "hunter2xyz"
	marked := expectRedacted(key, secretValue)

	t.Run("Argo CD Helm parameters", func(t *testing.T) {
		out := Redact(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"name": "a"},
			"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"parameters": []any{
				map[string]any{"name": "db.password", "value": secretValue},
				map[string]any{"name": "replicas", "value": "3"},
			}}}}}, key)
		params := out["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["parameters"].([]any)
		assert.Equal(t, marked, params[0].(map[string]any)["value"])
		assert.Equal(t, expectRedacted(key, "3"), params[1].(map[string]any)["value"], "every value of a parameters list")
		assert.Equal(t, "replicas", params[1].(map[string]any)["name"])
	})
	t.Run("credential-named pair in any list", func(t *testing.T) {
		out := Redact(map[string]any{"apiVersion": "x/v1", "kind": "X", "metadata": map[string]any{"name": "x"},
			"spec": map[string]any{"settings": []any{
				map[string]any{"name": "adminToken", "value": secretValue},
				map[string]any{"name": "region", "value": "us-east-2"},
			}}}, key)
		items := out["spec"].(map[string]any)["settings"].([]any)
		assert.Equal(t, marked, items[0].(map[string]any)["value"])
		assert.Equal(t, "us-east-2", items[1].(map[string]any)["value"])
	})
	t.Run("kapp original annotation", func(t *testing.T) {
		out := Redact(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c", "annotations": map[string]any{
			"kapp.k14s.io/original": `{"apiVersion":"v1","kind":"ConfigMap","data":{"dsn":"hunter2xyz"}}`,
			"example.com/manifest":  "apiVersion: v1\nkind: ConfigMap\ndata:\n  dsn: hunter2xyz\n",
			"example.com/note":      "Note: rotated weekly",
		}}, "data": map[string]any{"dsn": secretValue}}, key)
		ann := out["metadata"].(map[string]any)["annotations"].(map[string]any)
		assert.NotContains(t, ann["kapp.k14s.io/original"], secretValue)
		assert.Contains(t, ann["kapp.k14s.io/original"], marked)
		assert.True(t, redactedRE.MatchString(ann["example.com/manifest"].(string)), "a YAML manifest is replaced whole")
		assert.Equal(t, "Note: rotated weekly", ann["example.com/note"], "a short colon note is not a manifest")
	})
	t.Run("singular container", func(t *testing.T) {
		out := Redact(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow", "metadata": map[string]any{"name": "w"},
			"spec": map[string]any{"templates": []any{map[string]any{"container": map[string]any{
				"image": "busybox", "env": []any{map[string]any{"name": "DB_PASS_X", "value": secretValue}},
			}}}}}, key)
		c := out["spec"].(map[string]any)["templates"].([]any)[0].(map[string]any)["container"].(map[string]any)
		assert.Equal(t, marked, c["env"].([]any)[0].(map[string]any)["value"])
		assert.Equal(t, "busybox", c["image"])
	})
	t.Run("containers map", func(t *testing.T) {
		out := Redact(map[string]any{"apiVersion": "x/v1", "kind": "X", "metadata": map[string]any{"name": "x"},
			"spec": map[string]any{"containers": map[string]any{"app": map[string]any{
				"password": secretValue, "env": []any{map[string]any{"name": "A", "value": secretValue}},
			}}}}, key)
		app := out["spec"].(map[string]any)["containers"].(map[string]any)["app"].(map[string]any)
		assert.Equal(t, marked, app["password"])
		assert.Equal(t, marked, app["env"].([]any)[0].(map[string]any)["value"])
	})
	t.Run("marker prefix", func(t *testing.T) {
		prefixed := "<redacted>" + secretValue
		out := Redact(map[string]any{"apiVersion": "x/v1", "kind": "X", "metadata": map[string]any{"name": "x"},
			"spec": map[string]any{"password": prefixed, "note": "<redacted len=3 hmac=0123456789abcdef>"}}, key)
		spec := out["spec"].(map[string]any)
		assert.Equal(t, expectRedacted(key, prefixed), spec["password"], "a string that only starts like a marker is redacted")
		assert.Equal(t, "<redacted len=3 hmac=0123456789abcdef>", spec["note"], "an exact marker is kept")
	})
	t.Run("condition messages are prose", func(t *testing.T) {
		msg := "failed to refresh token: connection refused"
		out := Redact(map[string]any{"apiVersion": "x/v1", "kind": "X", "metadata": map[string]any{"name": "x"},
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": msg}}}}, key)
		cond := out["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
		assert.Equal(t, msg, cond["message"])
		assert.False(t, credentialValue("waiting for secret: not found yet"))
		assert.True(t, credentialValue("token=abc123def"), "a token-like single-line value")
		assert.True(t, credentialValue("user: app\npassword: correcthorse\n"), "any value in a config file")
	})
}
