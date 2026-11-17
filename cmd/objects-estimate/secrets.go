package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

// Secrets are never listed as objects. The API server's table view with includeObject=None
// returns each Secret's name, type and key count and nothing else, so no value and no
// annotation (last-applied-configuration can hold values) reaches this machine. Each Secret
// is then sized as a stand-in object with the same name, namespace, type and key count,
// sent through the carrier's own redaction and record builder.

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

var (
	secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	// standInKey is the HMAC key of the stand-in redaction; the hash length, not its value,
	// is what reaches the record size.
	standInKey = []byte("objects-estimate-stand-in-key-32")
)

const (
	// standInValueBytes is the decoded size given to each stand-in value. Redaction replaces
	// it with "<redacted len=N hmac=...>", so only the digit count of N reaches the size.
	standInValueBytes = 64
	helmValueBytes    = 100_000
	// watchRetryWait is the pause before a failed Secrets watch is restarted.
	watchRetryWait = 5 * time.Second
	// watchTimeoutSeconds is the server-side length of one Secrets watch.
	watchTimeoutSeconds = 1800
)

type secretRow struct {
	Name, Type string
	Keys       int
}

type secretTable struct {
	ResourceVersion, Continue string
	Rows                      []secretRow
}

type rawTable struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	ColumnDefinitions []struct {
		Name string `json:"name"`
	} `json:"columnDefinitions"`
	Rows []struct {
		Cells []any `json:"cells"`
	} `json:"rows"`
}

// secretColumns returns the table's column indexes, or prev when the table carries no
// column definitions: a table watch sends them with its first event only.
func secretColumns(raw []byte, prev map[string]int) (map[string]int, error) {
	var t rawTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decoding secrets table: %w", err)
	}
	if len(t.ColumnDefinitions) == 0 {
		return prev, nil
	}
	col := map[string]int{}
	for i, c := range t.ColumnDefinitions {
		col[c.Name] = i
	}
	return col, nil
}

// parseSecretTable reads a table's rows, with the column indexes of its own definitions or,
// without them, of cols.
func parseSecretTable(raw []byte, cols map[string]int) (secretTable, error) {
	var t rawTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return secretTable{}, fmt.Errorf("decoding secrets table: %w", err)
	}
	col, err := secretColumns(raw, cols)
	if err != nil {
		return secretTable{}, err
	}
	for _, name := range []string{"Name", "Type", "Data"} {
		if _, ok := col[name]; !ok {
			return secretTable{}, fmt.Errorf("secrets table has no %s column", name)
		}
	}
	out := secretTable{ResourceVersion: t.Metadata.ResourceVersion, Continue: t.Metadata.Continue}
	for _, r := range t.Rows {
		cell := func(name string) any {
			if i := col[name]; i < len(r.Cells) {
				return r.Cells[i]
			}
			return nil
		}
		name, _ := cell("Name").(string)
		typ, _ := cell("Type").(string)
		keys, _ := cell("Data").(float64)
		out.Rows = append(out.Rows, secretRow{Name: name, Type: typ, Keys: int(keys)})
	}
	return out, nil
}

// standInSecret is a Secret with row's name, type and key count, the given namespace, and
// placeholder uid, resourceVersion and values of realistic length.
func standInSecret(row secretRow, namespace string) map[string]any {
	valueBytes := standInValueBytes
	if row.Type == "helm.sh/release.v1" {
		valueBytes = helmValueBytes
	}
	value := base64.StdEncoding.EncodeToString(make([]byte, valueBytes))
	data := map[string]any{}
	for i := 0; i < row.Keys; i++ {
		data[fmt.Sprintf("key-%02d", i)] = value
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       row.Type,
		"metadata": map[string]any{
			"name":              row.Name,
			"namespace":         namespace,
			"uid":               "00000000-0000-0000-0000-000000000000",
			"resourceVersion":   "1234567890",
			"creationTimestamp": "2026-01-01T00:00:00Z",
		},
		"data": data,
	}
}

// standInBytes is the size of the events the carrier would send for row's stand-in, each
// sized as eventBytes sizes it.
func standInBytes(row secretRow, namespace, cluster, version string, now time.Time) (int64, error) {
	recs, err := objects.BuildRecords(objects.RedactSecret(standInSecret(row, namespace), standInKey), secretsGVR, objects.OpInitial, cluster, now)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range recs {
		b, err := eventBytes(r, cluster, version)
		if err != nil {
			return 0, err
		}
		n += b
	}
	return n, nil
}

// secretType is the per-type total of the Secrets listed at the start.
type secretType struct {
	Count        int64 `json:"count"`
	Keys         int64 `json:"keys"`
	StandInBytes int64 `json:"standInBytes"`
}

// secretSummary is the Secret side of the estimate: the first send from the table listing,
// and the changes from a table watch. Changes count every add, modify and delete the watch
// reported; a modify that leaves the carrier's hash unchanged would send nothing, so the change
// bytes are an upper bound.
type secretSummary struct {
	ByType  map[string]*secretType `json:"byType"`
	Changes map[string]*secretType `json:"changes"`
	Error   string                 `json:"error,omitempty"`
	Initial secretType             `json:"initial"`
	// WatchGaps counts watch restarts; events between a drop and the relist are missed.
	WatchGaps int `json:"watchGaps"`
	// UnparsedEvents counts watch events whose rows could not be read.
	UnparsedEvents int `json:"unparsedEvents"`
}

type secretCounter struct {
	// cols are the column indexes of the change watch, from its first event.
	cols map[string]int
	// cluster and version are the values of the attributes the infra sink adds.
	cluster, version string
	avgNS            string
	sum              secretSummary
	mu               sync.Mutex
}

func newSecretCounter(cluster, version string) *secretCounter {
	return &secretCounter{cluster: cluster, version: version, sum: secretSummary{ByType: map[string]*secretType{}, Changes: map[string]*secretType{}}}
}

func addTo(m map[string]*secretType, key string, keys int, bytes int64) {
	t, ok := m[key]
	if !ok {
		t = &secretType{}
		m[key] = t
	}
	t.Count++
	t.Keys += int64(keys)
	t.StandInBytes += bytes
}

func tableRequest(rc rest.Interface, namespace string) *rest.Request {
	req := rc.Get().Resource("secrets").Param("includeObject", "None").SetHeader("Accept", tableAccept)
	if namespace != "" {
		req = req.Namespace(namespace)
	}
	return req
}

// listInitial lists each namespace's Secrets as a table and sizes the first send. It returns
// the cluster-wide resourceVersion to start the change watch from.
func (c *secretCounter) listInitial(ctx context.Context, rc rest.Interface, namespaces []string, now time.Time) (string, error) {
	var nsChars int
	for _, ns := range namespaces {
		nsChars += len(ns)
		cont := ""
		for {
			raw, err := tableRequest(rc, ns).Param("limit", "500").Param("continue", cont).Do(ctx).Raw()
			if err != nil {
				return "", fmt.Errorf("listing secrets in %s: %w", ns, err)
			}
			tbl, err := parseSecretTable(raw, nil)
			if err != nil {
				return "", err
			}
			for _, row := range tbl.Rows {
				n, err := standInBytes(row, ns, c.cluster, c.version, now)
				if err != nil {
					return "", err
				}
				c.mu.Lock()
				c.sum.Initial.Count++
				c.sum.Initial.Keys += int64(row.Keys)
				c.sum.Initial.StandInBytes += n
				addTo(c.sum.ByType, row.Type, row.Keys, n)
				c.mu.Unlock()
			}
			if tbl.Continue == "" {
				break
			}
			cont = tbl.Continue
		}
	}
	if len(namespaces) > 0 {
		c.avgNS = strings.Repeat("n", nsChars/len(namespaces))
	}
	// A cluster-wide list of one row gives the resourceVersion the watch starts from.
	raw, err := tableRequest(rc, "").Param("limit", "1").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("listing secrets cluster-wide: %w", err)
	}
	tbl, err := parseSecretTable(raw, nil)
	if err != nil {
		return "", err
	}
	return tbl.ResourceVersion, nil
}

// watchChanges counts Secret adds, modifies and deletes until ctx ends. A watch event's row
// carries no namespace, so its stand-in takes a namespace of the cluster's average length.
// A delete is sized as the carrier's tombstone.
func (c *secretCounter) watchChanges(ctx context.Context, rc rest.Interface, rv string) {
	for ctx.Err() == nil {
		next, err := c.watchOnce(ctx, rc, rv)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.sum.WatchGaps++
		c.mu.Unlock()
		if err != nil {
			time.Sleep(watchRetryWait)
		}
		rv = next
	}
}

func (c *secretCounter) watchOnce(ctx context.Context, rc rest.Interface, rv string) (string, error) {
	stream, err := tableRequest(rc, "").Param("watch", "true").Param("resourceVersion", rv).
		Param("timeoutSeconds", strconv.Itoa(watchTimeoutSeconds)).Stream(ctx)
	if err != nil {
		return c.relist(ctx, rc), err
	}
	defer stream.Close()
	dec := json.NewDecoder(bufio.NewReader(stream))
	for {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			return c.relist(ctx, rc), err
		}
		if ev.Type == "ERROR" {
			return c.relist(ctx, rc), fmt.Errorf("secrets watch: %s", ev.Object)
		}
		c.countEvent(ev.Type, ev.Object)
	}
}

// countEvent counts one watch event's rows. An event whose rows cannot be read counts as
// unparsed, so a missed change shows in the summary.
func (c *secretCounter) countEvent(evType string, object []byte) {
	if evType != "ADDED" && evType != "MODIFIED" && evType != "DELETED" {
		return
	}
	c.mu.Lock()
	cols, err := secretColumns(object, c.cols)
	if err == nil {
		c.cols = cols
	}
	c.mu.Unlock()
	tbl, err := parseSecretTable(object, cols)
	if err != nil {
		c.mu.Lock()
		c.sum.UnparsedEvents++
		c.mu.Unlock()
		return
	}
	for _, row := range tbl.Rows {
		c.countChange(evType, row)
	}
}

// relist returns a fresh cluster-wide resourceVersion after a watch ends.
func (c *secretCounter) relist(ctx context.Context, rc rest.Interface) string {
	raw, err := tableRequest(rc, "").Param("limit", "1").Do(ctx).Raw()
	if err != nil {
		return ""
	}
	tbl, err := parseSecretTable(raw, nil)
	if err != nil {
		return ""
	}
	return tbl.ResourceVersion
}

func (c *secretCounter) countChange(evType string, row secretRow) {
	now := time.Now()
	var n int64
	switch evType {
	case "ADDED", "MODIFIED":
		var err error
		if n, err = standInBytes(row, c.avgNS, c.cluster, c.version, now); err != nil {
			return
		}
	case "DELETED":
		var err error
		if n, err = eventBytes(objects.Tombstone("00000000-0000-0000-0000-000000000000", objects.Entry{
			GVR: objects.GVRString(secretsGVR), Kind: "Secret", Namespace: c.avgNS, Name: row.Name,
		}, c.cluster, now), c.cluster, c.version); err != nil {
			return
		}
	default:
		return
	}
	c.mu.Lock()
	addTo(c.sum.Changes, strings.ToLower(evType), row.Keys, n)
	c.mu.Unlock()
}

func (c *secretCounter) summary() secretSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(c.sum)
	var out secretSummary
	_ = json.Unmarshal(b, &out)
	return out
}

func (c *secretCounter) fail(err error) {
	c.mu.Lock()
	c.sum.Error = err.Error()
	c.mu.Unlock()
}
