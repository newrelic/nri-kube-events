package objects

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// PartsPerRecord is the most part attributes one record carries. MaxRecordBytes can
	// close a record sooner.
	PartsPerRecord = 110
	// MaxRecordBytes is the largest json.Marshal size of one record. It leaves room under
	// the forwarder's 1,000,000-byte POST limit for the attributes the sink adds and the
	// batch envelope.
	MaxRecordBytes = 450_000
	// ObjectType is the type attribute of object records and tombstones.
	ObjectType = "K8sObject"
	// HeartbeatType is the type attribute of carrier heartbeats.
	HeartbeatType = "K8sObjectCarrier"
	// Category is the category of object records, tombstones and heartbeats.
	Category = "kubernetes.objectState"
)

// Op is the reason a record was sent.
type Op string

const (
	OpInitial Op = "initial"
	OpAdded   Op = "added"
	OpUpdated Op = "updated"
	OpDeleted Op = "deleted"
	// OpResync marks a record re-sent because the last send is older than resendAfter.
	OpResync Op = "resync"
)

// Record is one InfrastructureEvent. Its JSON shape is the SDK v3 event, so json.Marshal of
// a Record is the size of the event the sink sends. Attributes["timestamp"] is int64 Unix
// seconds.
type Record struct {
	Summary    string         `json:"summary"`
	Category   string         `json:"category"`
	Attributes map[string]any `json:"attributes"`
}

// Entry is what the hash store keeps per object uid: enough identity to send a tombstone,
// the last sent hash and when it was sent (Unix seconds).
type Entry struct {
	GVR, Kind, Namespace, Name, Hash string
	LastSent                         int64
}

// GVRString formats gvr as <group>/<version>/<resource>; the core group is empty, so a
// ConfigMap gives /v1/configmaps.
func GVRString(gvr schema.GroupVersionResource) string {
	return gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
}

// ParseGVR parses the output of GVRString.
func ParseGVR(s string) (schema.GroupVersionResource, error) {
	f := strings.Split(s, "/")
	if len(f) != 3 || f[1] == "" || f[2] == "" {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid GVR %q: want <group>/<version>/<resource>", s)
	}
	return schema.GroupVersionResource{Group: f[0], Version: f[1], Resource: f[2]}, nil
}

// Split cuts s into parts, each prefixed with Mark. Part 0 holds the first Step runes of s.
// Every later part starts Overlap runes before the previous part's end and adds up to Step
// new runes, so ASCII text keeps the cut points [max(0, i*Step-Overlap), (i+1)*Step). A
// part ends sooner when the next rune would take it, Mark included, past MaxAttrBytes
// bytes, but it always adds at least one new rune. An empty s gives no parts.
//
// The overlap is at most Overlap*utf8.UTFMax = 1,024 bytes, and part 0 holds at least
// (MaxAttrBytes-len(Mark))/utf8.UTFMax = 1,023 runes, so every later part can start
// Overlap runes back and Join can drop exactly Overlap runes from it.
func Split(s string) []string {
	r := []rune(s)
	var parts []string
	for end := 0; end < len(r); {
		start := 0
		if len(parts) > 0 {
			start = end - Overlap
		}
		size := len(Mark)
		for _, c := range r[start:end] {
			size += utf8.RuneLen(c)
		}
		next := end
		for next < len(r) && next-end < Step {
			n := utf8.RuneLen(r[next])
			if next > end && size+n > MaxAttrBytes {
				break
			}
			size += n
			next++
		}
		parts = append(parts, Mark+string(r[start:next]))
		end = next
	}
	return parts
}

// Join rebuilds the string that Split cut: it strips Mark from every part and drops the
// first Overlap runes of every part after the first.
func Join(parts []string) string {
	var b strings.Builder
	for i, p := range parts {
		p = strings.TrimPrefix(p, Mark)
		if i > 0 {
			r := []rune(p)
			if len(r) > Overlap {
				p = string(r[Overlap:])
			} else {
				p = ""
			}
		}
		b.WriteString(p)
	}
	return b.String()
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

// BuildRecords serializes obj, which must already be redacted, and returns the records of
// this version, all with the same timestamp and identity. Each record holds at most
// PartsPerRecord parts and encodes to at most MaxRecordBytes. The body and every attribute
// except object.hash come from obj as it is; Hash normalizes its own copy.
func BuildRecords(obj map[string]any, gvr schema.GroupVersionResource, op Op, cluster string, now time.Time) ([]Record, error) {
	text, err := serialize(obj)
	if err != nil {
		return nil, fmt.Errorf("serializing object: %w", err)
	}
	parts := Split(text)
	sum := sha256.Sum256([]byte(text))

	meta, _ := obj["metadata"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	kind, _ := obj["kind"].(string)
	namespace, _ := meta["namespace"].(string)
	name, _ := meta["name"].(string)
	uid, _ := meta["uid"].(string)
	rv, _ := meta["resourceVersion"].(string)

	fixed := identity(cluster, gvr, kind, namespace, name, uid, op, now)
	fixed["k8s.resourceVersion"] = rv

	if owner := controllerOwner(meta); owner != nil {
		for _, k := range []string{"kind", "name", "uid"} {
			if v, ok := owner[k].(string); ok {
				fixed["k8s.owner."+k] = v
			}
		}
	}

	labels, truncated := labelString(meta)
	fixed["k8s.labels"] = labels
	if truncated {
		fixed["k8s.labels.truncated"] = true
	}

	if g, ok := number(meta["generation"]); ok {
		fixed["k8s.generation"] = int64(g)
	}
	if g, ok := number(status["observedGeneration"]); ok {
		fixed["k8s.observedGeneration"] = int64(g)
	}
	if ts, ok := meta["deletionTimestamp"].(string); ok && ts != "" {
		fixed["k8s.deletionTimestamp"] = ts
	}

	h := ComputeHealth(obj)
	if h.Phase != "" {
		fixed["k8s.phase"] = h.Phase
	}
	if h.Ready != nil {
		fixed["k8s.ready"] = *h.Ready
	}
	fixed["k8s.conditions.unhealthy"] = h.ConditionsUnhealthy
	if h.ConditionsTruncated {
		fixed["k8s.conditions.unhealthy.truncated"] = true
	}
	fixed["k8s.stale"] = h.Stale
	fixed["k8s.unhealthy"] = h.Unhealthy

	fixed["object.hash"] = Hash(obj)
	fixed["object.sha256"] = hex.EncodeToString(sum[:])
	fixed["object.chars"] = utf8.RuneCountInString(text)
	fixed["object.parts"] = len(parts)

	summary := fmt.Sprintf("%s %s/%s %s rv %s", kind, namespace, name, op, rv)
	groups, err := groupParts(parts, Record{Summary: summary, Category: Category, Attributes: fixed})
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(groups))
	for ri, g := range groups {
		attrs := make(map[string]any, len(fixed)+3+g.last-g.first)
		for k, v := range fixed {
			attrs[k] = v
		}
		attrs["record.index"] = ri
		attrs["record.count"] = len(groups)
		attrs["record.firstPart"] = g.first
		for i := g.first; i < g.last; i++ {
			attrs[partKey(i)] = parts[i]
		}
		records = append(records, Record{Summary: summary, Category: Category, Attributes: attrs})
	}
	return records, nil
}

// partGroup is the half-open range of part indexes [first, last) that one record carries.
type partGroup struct{ first, last int }

func partKey(i int) string { return fmt.Sprintf("object.p%03d", i) }

// groupParts assigns parts to records in order. A record is closed when it holds
// PartsPerRecord parts, or when the next part would push its json.Marshal size above
// MaxRecordBytes. Sizes come from json.Marshal of each key and part; json.Marshal also
// escapes HTML characters, so the estimate does not undercount the sender's encoding. A part
// is never split across records, so every record holds at least one part. An object with no
// parts still gets one record.
func groupParts(parts []string, base Record) ([]partGroup, error) {
	// Measure the base with the three record.* attributes at a width no real value reaches.
	attrs := make(map[string]any, len(base.Attributes)+3)
	for k, v := range base.Attributes {
		attrs[k] = v
	}
	attrs["record.index"] = 999999
	attrs["record.count"] = 999999
	attrs["record.firstPart"] = 999999
	base.Attributes = attrs
	b, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("measuring record: %w", err)
	}
	baseSize := len(b)

	groups := []partGroup{{0, 0}}
	size := baseSize
	for i, p := range parts {
		k, err := json.Marshal(partKey(i))
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		add := len(k) + 1 + len(v) + 1 // key, colon, value, comma
		g := &groups[len(groups)-1]
		if n := g.last - g.first; n > 0 && (n == PartsPerRecord || size+add > MaxRecordBytes) {
			groups = append(groups, partGroup{i, i})
			g = &groups[len(groups)-1]
			size = baseSize
		}
		g.last = i + 1
		size += add
	}
	return groups, nil
}

// Tombstone is the record sent when the object with uid is deleted. It carries the identity
// kept in e and no parts. Its timestamp is observedAt, when the carrier saw the deletion.
func Tombstone(uid string, e Entry, cluster string, observedAt time.Time) Record {
	gvr, err := ParseGVR(e.GVR)
	if err != nil {
		// The store only holds strings that GVRString wrote; keep the raw value visible.
		gvr = schema.GroupVersionResource{Resource: e.GVR}
	}
	attrs := identity(cluster, gvr, e.Kind, e.Namespace, e.Name, uid, OpDeleted, observedAt)
	attrs["object.parts"] = 0
	attrs["record.index"] = 0
	attrs["record.count"] = 1
	attrs["record.firstPart"] = 0
	return Record{
		Summary:    fmt.Sprintf("%s %s/%s %s", e.Kind, e.Namespace, e.Name, OpDeleted),
		Category:   Category,
		Attributes: attrs,
	}
}

func identity(cluster string, gvr schema.GroupVersionResource, kind, namespace, name, uid string, op Op, at time.Time) map[string]any {
	return map[string]any{
		"type":             ObjectType,
		"timestamp":        at.Unix(),
		"k8s.cluster.name": cluster,
		"k8s.group":        gvr.Group,
		"k8s.version":      gvr.Version,
		"k8s.resource":     gvr.Resource,
		"k8s.kind":         kind,
		"k8s.namespace":    namespace,
		"k8s.name":         name,
		"k8s.uid":          uid,
		"k8s.op":           string(op),
	}
}

// controllerOwner returns the ownerReference with controller: true, or nil.
func controllerOwner(meta map[string]any) map[string]any {
	refs, _ := meta["ownerReferences"].([]any)
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := ref["controller"].(bool); c {
			return ref
		}
	}
	return nil
}

// labelString formats the labels as ",k=v,k=v," with keys sorted, or "" when there are none.
// capList drops whole labels from the end to stay within PartChars runes and MaxAttrBytes
// bytes, so the result still ends in "," and LIKE '%,k=v,%' never matches a cut label, and
// reports truncated.
func labelString(meta map[string]any) (string, bool) {
	labels, _ := meta["labels"].(map[string]any)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]string, len(keys))
	for i, k := range keys {
		v, _ := labels[k].(string)
		items[i] = k + "=" + v
	}
	return capList(items)
}
