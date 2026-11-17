package objects

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// lastAppliedAnnotation holds the object as kubectl last applied it, values included.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// redactedWhole replaces a field whose shape the carrier does not recognize. Redaction
// fails closed: a value that cannot be redacted key by key is never passed through.
const redactedWhole = "<redacted>"

// Redact returns a deep copy of obj. When obj is a Secret, every value in data and
// stringData is replaced with "<redacted len=N hmac=XXXXXXXXXXXXXXXX>", in the copy and in
// the copy's last-applied-configuration annotation. Keys are kept. A Helm release Secret
// (type helm.sh/release.v1) needs no special case: its whole release is data.release.
// With an empty key the marker is "<redacted len=N>". The input is never modified, so the
// caller may pass the watch's own object.
func Redact(obj map[string]any, key []byte) map[string]any {
	out, _ := deepCopy(obj).(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	if kind, _ := out["kind"].(string); kind != "Secret" {
		return out
	}
	return redactSecretCopy(out, key)
}

// RedactSecret returns a redacted deep copy of obj as Redact does for a Secret, whatever
// obj's own kind field says. The watcher calls it for every object of core v1 secrets, so
// an object that arrives without kind is still redacted.
func RedactSecret(obj map[string]any, key []byte) map[string]any {
	out, _ := deepCopy(obj).(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return redactSecretCopy(out, key)
}

// redactSecretCopy redacts out, a deep copy the caller owns, in place and returns it.
func redactSecretCopy(out map[string]any, key []byte) map[string]any {
	redactValues(out, key)

	meta, ok := out["metadata"].(map[string]any)
	if !ok {
		return out
	}
	switch ann := meta["annotations"].(type) {
	case nil:
	case map[string]any:
		if v, present := ann[lastAppliedAnnotation]; present {
			if raw, isString := v.(string); isString {
				ann[lastAppliedAnnotation] = redactLastApplied(raw, key)
			} else {
				ann[lastAppliedAnnotation] = redactedWhole
			}
		}
	case map[string]string:
		// deepCopy does not copy this map type, so build a new one and leave the input alone.
		if raw, present := ann[lastAppliedAnnotation]; present {
			copied := make(map[string]string, len(ann))
			for k, v := range ann {
				copied[k] = v
			}
			copied[lastAppliedAnnotation] = redactLastApplied(raw, key)
			meta["annotations"] = copied
		}
	default:
		meta["annotations"] = redactedWhole
	}
	return out
}

// redactValues replaces the values of secret's data (base64) and stringData (plain) in
// place. A data or stringData field in any shape other than map[string]any is replaced
// whole with redactedWhole.
func redactValues(secret map[string]any, key []byte) {
	for _, field := range []string{"data", "stringData"} {
		v, present := secret[field]
		if !present || v == nil {
			continue
		}
		values, ok := v.(map[string]any)
		if !ok {
			secret[field] = redactedWhole
			continue
		}
		for k, val := range values {
			if field == "data" {
				values[k] = redactedValue(decodeData(val), key)
			} else {
				values[k] = redactedValue(plainValue(val), key)
			}
		}
	}
}

// redactLastApplied redacts data and stringData in a last-applied-configuration value,
// whatever kind the embedded JSON declares, because it sits on a Secret. A value that does
// not parse as a JSON object is replaced whole, because the carrier cannot tell which part
// of it is secret.
func redactLastApplied(raw string, key []byte) string {
	var applied map[string]any
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		return redactedValue([]byte(raw), key)
	}
	redactValues(applied, key)
	// serialize leaves HTML characters unescaped, so the marker reads <redacted here too.
	s, err := serialize(applied)
	if err != nil {
		return redactedValue([]byte(raw), key)
	}
	return s
}

// decodeData returns the decoded bytes of a data value. A string that is not valid base64
// is used as it is.
func decodeData(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	s, _ := v.(string)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	return []byte(s)
}

// plainValue returns the bytes of a stringData value.
func plainValue(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	s, _ := v.(string)
	return []byte(s)
}

// redactedValue builds the marker for one value. With an empty key, anyone can recompute
// the HMAC, so a short secret could be found by brute force; the hmac part is then omitted.
func redactedValue(value, key []byte) string {
	if len(key) == 0 {
		return fmt.Sprintf("<redacted len=%d>", len(value))
	}
	m := hmac.New(sha256.New, key)
	m.Write(value)
	return fmt.Sprintf("<redacted len=%d hmac=%s>", len(value), hex.EncodeToString(m.Sum(nil))[:16])
}

// serialize writes obj as compact JSON without HTML escaping and without a trailing newline.
func serialize(obj map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
