package objects

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Every kind other than Secret goes through redactObjectCopy, which replaces with the
// same HMAC marker as Secret values (see redactedValue), so a change of a redacted value
// still changes the record's hash:
//
//   - every value of a ConfigMap's data and binaryData (keys are kept);
//   - the value of every env entry of every container, wherever a container sits in the
//     object: under containers, initContainers, ephemeralContainers, sidecars or steps (a
//     list, or a map of name to container), or under a single container field (names and
//     valueFrom are kept);
//   - the value of every {name, value} entry of an env or extraEnv list, and, in any other
//     list of {name, value} or {key, value} entries (Argo CD Helm parameters, Tekton
//     params), the value of an entry whose name looks like a credential or whose value
//     looks like a token or key (tokenValue); an image tag or a replica count is kept;
//   - inside a string that holds a multi-line YAML mapping or a JSON object, such as Helm
//     values or a copy of a ConfigMap's data, the value of every key whose name looks like
//     a credential and every scalar that looks like one; the text is written back with the
//     rest kept;
//   - every string under a field whose name looks like a credential (credentialName), at
//     any depth outside metadata, and every string of a map or list under such a field;
//   - every string, at any depth outside metadata, whose value looks like a credential
//     (credentialValue): a private key, a JWT, an AWS access key id, a URL with a
//     password, or a "password: ..." style assignment whose value looks like a token and
//     not like a word of prose, so a condition message such as "failed to refresh token:
//     connection refused" is kept;
//   - every annotation whose name looks like a credential or whose value does; every
//     annotation that holds a JSON object, such as kubectl's last-applied-configuration or
//     kapp's kapp.k14s.io/original, with the same rules applied to the object it holds; and
//     every annotation that holds a YAML manifest (a mapping with apiVersion and kind),
//     replaced whole.
//
// A string that is already exactly a marker is kept. Any other string, including one that
// only starts like a marker, goes through the same rules.
//
// Limits: a credential in a string whose field name and value both look ordinary, such as
// a password inside a container's args or a free-text field, is sent as it is. So is a
// single-line "password=supersecret" whose value holds only letters: requiring a digit or
// symbol there keeps condition messages and other prose. Labels are
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

// credentialShortNames are short names that hold a credential only as the whole name.
var credentialShortNames = map[string]bool{"pass": true, "pw": true, "pwd": true}

// credentialName reports whether a field or annotation name looks like it holds a
// credential, and does not merely refer to one.
func credentialName(name string) bool {
	n := normalizeName(name)
	if credentialShortNames[n] {
		return true
	}
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
}

// assignmentPattern matches a "password: value" style assignment and captures the value.
var assignmentPattern = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key)["']?\s*[=:]\s*["']?([^\s"',}]+)`)

// markerPattern matches a marker redactedValue or redactedWhole wrote.
var markerPattern = regexp.MustCompile(`^<redacted(?: len=\d+(?: hmac=[0-9a-f]{16})?)?>$`)

// minAssignedValue is the shortest value of an assignment that counts as a credential.
const minAssignedValue = 6

// tokenLike reports whether the value of an assignment looks like a credential and not a
// word: in a multi-line string (a config file) any value of minAssignedValue characters,
// and in a single line one that also holds a digit or a symbol.
func tokenLike(value string, multiline bool) bool {
	if len(value) < minAssignedValue {
		return false
	}
	if multiline {
		return true
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	}) >= 0
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
	multiline := strings.Contains(s, "\n")
	for _, m := range assignmentPattern.FindAllStringSubmatch(s, -1) {
		if tokenLike(m[2], multiline) {
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
		return redactString(field, t, key, inCredential)
	case map[string]any:
		redactMap(field, t, key, cred)
		return t
	case []any:
		for i, val := range t {
			if cm, ok := val.(map[string]any); ok && isContainerList(field) {
				redactContainer(cm, key)
				continue
			}
			t[i] = redactTree(field, val, key, cred)
		}
		return t
	default:
		return v
	}
}

// redactString returns the string t, the value of field, with its credentials replaced.
func redactString(field, t string, key []byte, inCredential bool) string {
	if t == "" || markerPattern.MatchString(t) {
		return t
	}
	if credentialName(field) || (inCredential && !isReference(field)) {
		return redactedValue([]byte(t), key)
	}
	if text, ok := redactStructuredText(t, key); ok {
		return text
	}
	if credentialValue(t) {
		return redactedValue([]byte(t), key)
	}
	return t
}

// redactMap redacts t, the map value of field, in place: as a container, as a map of name
// to container, or by the tree rules. cred is set below a credential-named field.
func redactMap(field string, t map[string]any, key []byte, cred bool) {
	switch {
	case field == "container":
		redactContainer(t, key)
	case isContainerList(field):
		for k, c := range t {
			if cm, ok := c.(map[string]any); ok {
				redactContainer(cm, key)
			} else {
				t[k] = redactTree(k, c, key, cred)
			}
		}
	default:
		redactNameValue(field, t, key)
		for k, val := range t {
			t[k] = redactTree(k, val, key, cred)
		}
	}
}

// envLists are the list fields whose {name, value} entries are environment variables, all
// of whose values are redacted.
var envLists = map[string]bool{"env": true, "extraEnv": true}

// redactNameValue replaces the value of entry, an element of the list field, when entry is
// a {name, value} or {key, value} pair that may hold a credential: every pair of a list in
// envLists, and in any other list, such as Argo CD Helm parameters or Tekton params, a pair
// whose name looks like a credential or whose value looks like a token or key. Other
// parameters, such as an image tag or a replica count, are kept.
func redactNameValue(field string, entry map[string]any, key []byte) {
	name, isString := entry["name"].(string)
	if !isString {
		name, isString = entry["key"].(string)
	}
	val, has := entry["value"]
	if !has || !isString {
		return
	}
	str, _ := val.(string)
	if envLists[field] || credentialName(name) || credentialValue(str) || tokenValue(str) {
		redactEnvValue(entry, key)
	}
}

// minTokenValue is the shortest string tokenValue treats as a token.
const minTokenValue = 24

// tokenValue reports whether s looks like a generated token or key: at least minTokenValue
// characters with no space or colon, holding both letters and digits. A git commit or image
// digest, all hexadecimal, does not count.
func tokenValue(s string) bool {
	if len(s) < minTokenValue || strings.ContainsAny(s, " \t\n:") {
		return false
	}
	letters, digits, hexOnly := false, false, true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			letters = true
		case (r >= 'g' && r <= 'z') || (r >= 'G' && r <= 'Z'):
			letters, hexOnly = true, false
		default:
			hexOnly = false
		}
	}
	return letters && digits && !hexOnly
}

func isContainerList(field string) bool {
	switch field {
	case "containers", "initContainers", "ephemeralContainers", "sidecars", "steps":
		return true
	}
	return false
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
		case name == lastAppliedAnnotation || isJSONObject(value):
			return redactLastAppliedObject(value, key)
		case credentialName(name) || isYAMLManifest(value):
			return redactedValue([]byte(value), key)
		}
		if text, ok := redactStructuredText(value, key); ok {
			return text
		}
		if credentialValue(value) {
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

// redactLastAppliedObject applies Redact to the object that an annotation of a non-Secret
// holds as JSON, such as last-applied-configuration or kapp.k14s.io/original. A value that
// does not parse as a JSON object is replaced whole.
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

// isYAMLManifest reports whether some YAML document in s is a mapping with apiVersion and
// kind, the shape of an embedded Kubernetes manifest.
func isYAMLManifest(s string) bool {
	if !strings.Contains(s, "apiVersion") || !strings.Contains(s, "kind") {
		return false
	}
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			// A stream that does not decode as mappings is treated as a manifest when it
			// names both keys, so redaction fails closed.
			return !errors.Is(err, io.EOF) && strings.Contains(s, "apiVersion:") && strings.Contains(s, "kind:")
		}
		if _, ok := doc["apiVersion"]; ok {
			if _, ok := doc["kind"]; ok {
				return true
			}
		}
	}
}

// redactStructuredText redacts s when it is a multi-line YAML mapping or a JSON object, such
// as an Argo CD Application's Helm values or an annotation copy of a ConfigMap's data: the
// value of every key whose name looks like a credential, and every scalar that looks like
// one, is replaced with a marker, and the text is written back. It reports false when s is
// neither, and the caller then judges s as a plain string. A JSON object is written back as
// compact JSON; YAML is written back as YAML, comments kept.
func redactStructuredText(s string, key []byte) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) != nil {
			return "", false
		}
		for k, v := range obj {
			obj[k] = redactTree(k, v, key, false)
		}
		out, err := serialize(obj)
		if err != nil {
			return redactedValue([]byte(s), key), true
		}
		return out, true
	}
	if !strings.Contains(trimmed, "\n") || !strings.Contains(trimmed, ":") {
		return "", false
	}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(s), &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", false
	}
	if !redactYAMLNode(doc.Content[0], key, false) {
		return s, true
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return redactedValue([]byte(s), key), true
	}
	return string(out), true
}

// redactYAMLNode replaces, below n, the scalar values that the tree rules would redact, and
// reports whether it replaced any. inCredential is set below a credential-named key.
func redactYAMLNode(n *yaml.Node, key []byte, inCredential bool) bool {
	changed := false
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			cred := inCredential || credentialName(k.Value)
			if v.Kind == yaml.ScalarNode {
				redact := cred && (!inCredential || !isReference(k.Value))
				changed = markYAMLScalar(v, key, redact) || changed
				continue
			}
			changed = redactYAMLNode(v, key, cred) || changed
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				changed = markYAMLScalar(c, key, inCredential) || changed
				continue
			}
			changed = redactYAMLNode(c, key, inCredential) || changed
		}
	case yaml.DocumentNode, yaml.AliasNode, yaml.ScalarNode:
	}
	return changed
}

// markYAMLScalar replaces the scalar n with a marker when redact is set or its value looks
// like a credential, and reports whether it did.
func markYAMLScalar(n *yaml.Node, key []byte, redact bool) bool {
	if n.Value == "" || markerPattern.MatchString(n.Value) || (!redact && !credentialValue(n.Value)) {
		return false
	}
	n.Value, n.Tag, n.Style = redactedValue([]byte(n.Value), key), "!!str", yaml.DoubleQuotedStyle
	return true
}
