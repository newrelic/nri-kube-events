package objects

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Every kind other than Secret goes through redactObjectCopy, which replaces with the
// same HMAC marker as Secret values (see redactedValue), so a change of a redacted value
// still changes the record's hash:
//
//   - every value of a ConfigMap's data and binaryData (keys are kept);
//   - the value of every env entry of every container, init container and ephemeral
//     container, wherever a pod spec sits in the object (names and valueFrom are kept);
//   - every string under a field whose name looks like a credential (credentialName), at
//     any depth outside metadata, and every string of a map or list under such a field;
//   - every string, at any depth outside metadata, whose value looks like a credential
//     (credentialValue): a private key, a JWT, an AWS access key id, a URL with a
//     password, or a "password: ..." style assignment;
//   - every annotation whose name looks like a credential or whose value does, and the
//     last-applied-configuration annotation with the same rules applied to the object it
//     holds.
//
// Limits: a credential in a string whose field name and value both look ordinary, such as
// a password inside a container's args or a free-text field, is sent as it is. Labels are
// never redacted, because their values are short identifiers that selectors match on.

// credentialWords are the lowercase, punctuation-free fragments that mark a field or
// annotation name as holding a credential.
var credentialWords = []string{
	"password", "passwd", "passphrase", "secret", "token", "credential", "apikey",
	"privatekey", "accesskey", "secretkey", "clientkey", "sshkey", "tlskey", "cert",
}

// referenceSuffixes end the lowercase, punctuation-free names of fields that refer to a
// credential by name and do not hold it, such as secretRef, secretName or tokenSecretRef.
var referenceSuffixes = []string{"ref", "refs", "name", "names", "namespace"}

// referenceLeaves are the field names, inside a credential-named map, whose strings are
// identifiers of the credential and not its value, as in secretKeyRef: {name, key}.
var referenceLeaves = map[string]bool{
	"name": true, "namespace": true, "key": true, "kind": true, "apiVersion": true,
	"optional": true, "path": true, "mountPath": true, "mode": true, "items": true,
}

// isReference reports whether a field below a credential-named field holds an identifier
// of the credential, such as name or secretName, and not its value.
func isReference(field string) bool {
	if referenceLeaves[field] {
		return true
	}
	n := normalizeName(field)
	for _, s := range referenceSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// credentialName reports whether a field or annotation name looks like it holds a
// credential, and does not merely refer to one.
func credentialName(name string) bool {
	n := normalizeName(name)
	matched := false
	for _, w := range credentialWords {
		if strings.Contains(n, w) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, s := range referenceSuffixes {
		if strings.HasSuffix(n, s) {
			return false
		}
	}
	return true
}

func normalizeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var credentialValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`),
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key)["']?\s*[=:]\s*["']?[^\s"',}]+`),
}

// minCredentialValue is the shortest string credentialValue checks.
const minCredentialValue = 8

// credentialValue reports whether s looks like it holds a credential. A cheap substring
// check skips the regular expressions for most strings.
func credentialValue(s string) bool {
	if len(s) < minCredentialValue {
		return false
	}
	lower := strings.ToLower(s)
	if !strings.Contains(s, "PRIVATE KEY") && !strings.Contains(s, "eyJ") && !strings.Contains(s, "AKIA") &&
		!strings.Contains(s, "ASIA") && !strings.Contains(s, "://") && !strings.ContainsAny(lower, "=:") {
		return false
	}
	for _, re := range credentialValuePatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// redactObjectCopy redacts out, a deep copy of an object of any kind other than Secret
// that the caller owns, in place, and returns it.
func redactObjectCopy(out map[string]any, key []byte) map[string]any {
	kind, _ := out["kind"].(string)
	apiVersion, _ := out["apiVersion"].(string)
	if kind == "ConfigMap" && apiVersion == "v1" {
		redactConfigMapData(out, key)
	}
	for field, v := range out {
		switch field {
		case "metadata", "apiVersion", "kind":
			continue
		}
		out[field] = redactTree(field, v, key, false)
	}
	if meta, ok := out["metadata"].(map[string]any); ok {
		redactObjectAnnotations(meta, key)
	}
	return out
}

// redactConfigMapData replaces every value of data and binaryData. A field in another
// shape than map[string]any is replaced whole.
func redactConfigMapData(cm map[string]any, key []byte) {
	for _, field := range []string{"data", "binaryData"} {
		v, present := cm[field]
		if !present || v == nil {
			continue
		}
		values, ok := v.(map[string]any)
		if !ok {
			cm[field] = redactedWhole
			continue
		}
		for k, val := range values {
			if field == "binaryData" {
				values[k] = redactedValue(decodeData(val), key)
			} else {
				values[k] = redactedValue(plainValue(val), key)
			}
		}
	}
}

// redactTree returns v, the value of field, with its credentials replaced. inCredential is
// set below a credential-named field, where every string is redacted except the identifier
// fields in referenceLeaves.
func redactTree(field string, v any, key []byte, inCredential bool) any {
	cred := inCredential || credentialName(field)
	switch t := v.(type) {
	case string:
		if t == "" || strings.HasPrefix(t, "<redacted") {
			return t
		}
		switch {
		case credentialValue(t), credentialName(field):
			return redactedValue([]byte(t), key)
		case inCredential && !isReference(field):
			return redactedValue([]byte(t), key)
		}
		return t
	case map[string]any:
		if isContainerList(field) {
			return t
		}
		for k, val := range t {
			t[k] = redactTree(k, val, key, cred)
		}
		return t
	case []any:
		if isContainerList(field) {
			for _, c := range t {
				if cm, ok := c.(map[string]any); ok {
					redactContainer(cm, key)
				}
			}
			return t
		}
		for i, val := range t {
			t[i] = redactTree(field, val, key, cred)
		}
		return t
	default:
		return v
	}
}

func isContainerList(field string) bool {
	return field == "containers" || field == "initContainers" || field == "ephemeralContainers"
}

// redactContainer redacts the value of every env entry of a container, then the rest of
// the container by the general rules.
func redactContainer(c map[string]any, key []byte) {
	env, _ := c["env"].([]any)
	for _, e := range env {
		if entry, ok := e.(map[string]any); ok {
			redactEnvValue(entry, key)
		}
	}
	for k, val := range c {
		if k == "env" {
			continue
		}
		c[k] = redactTree(k, val, key, false)
	}
}

// redactEnvValue replaces the value of one env entry. A value of another type than string
// is replaced whole.
func redactEnvValue(entry map[string]any, key []byte) {
	val, present := entry["value"]
	switch s, isString := val.(string); {
	case !present || val == nil || (isString && s == ""):
	case isString:
		entry["value"] = redactedValue([]byte(s), key)
	default:
		entry["value"] = redactedWhole
	}
}

// redactObjectAnnotations redacts the annotations of a non-Secret object: the
// last-applied-configuration by the object rules, and any other whose name or value looks
// like a credential.
func redactObjectAnnotations(meta map[string]any, key []byte) {
	redact := func(name, value string) string {
		switch {
		case name == lastAppliedAnnotation:
			return redactLastAppliedObject(value, key)
		case credentialName(name) || credentialValue(value):
			return redactedValue([]byte(value), key)
		}
		return value
	}
	switch ann := meta["annotations"].(type) {
	case map[string]any:
		for k, v := range ann {
			if s, ok := v.(string); ok {
				ann[k] = redact(k, s)
			}
		}
	case map[string]string:
		copied := make(map[string]string, len(ann))
		for k, v := range ann {
			copied[k] = redact(k, v)
		}
		meta["annotations"] = copied
	}
}

// redactLastAppliedObject applies Redact to the object a last-applied-configuration of a
// non-Secret holds. A value that does not parse as a JSON object is replaced whole.
func redactLastAppliedObject(raw string, key []byte) string {
	var applied map[string]any
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		return redactedValue([]byte(raw), key)
	}
	s, err := serialize(Redact(applied, key))
	if err != nil {
		return redactedValue([]byte(raw), key)
	}
	return s
}
