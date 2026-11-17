package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

// tally counts records and their marshaled bytes. Versions counts object versions: an
// object larger than one record sends several, and only the first has record.index 0.
type tally struct {
	Records  int64 `json:"records"`
	Versions int64 `json:"versions"`
	Bytes    int64 `json:"bytes"`
}

func (t *tally) add(bytes int64, first bool) {
	t.Records++
	t.Bytes += bytes
	if first {
		t.Versions++
	}
}

type phase struct {
	ByKind map[string]*tally `json:"byKind"`
	ByOp   map[string]*tally `json:"byOp,omitempty"`
	tally
}

func newPhase() phase {
	return phase{ByKind: map[string]*tally{}, ByOp: map[string]*tally{}}
}

func (p *phase) add(kind, op string, bytes int64, first bool) {
	p.tally.add(bytes, first)
	for m, k := range map[*map[string]*tally]string{&p.ByKind: kind, &p.ByOp: op} {
		t, ok := (*m)[k]
		if !ok {
			t = &tally{}
			(*m)[k] = t
		}
		t.add(bytes, first)
	}
}

// recentChanges is the snapshot cross-check of one kind: of the objects in the first send,
// how many a manager last wrote in the hour, and in the day, before the run started, by the
// newest metadata.managedFields time. An object written several times counts once, and a
// write the carrier's hash ignores still counts, so this is a check on the watched change
// rate, not a measure of it. Unknown counts objects too large for one record, which this
// check does not reassemble.
type recentChanges struct {
	Objects  int64 `json:"objects"`
	LastHour int64 `json:"lastHour"`
	LastDay  int64 `json:"lastDay"`
	Unknown  int64 `json:"unknown"`
}

// sinkSummary is what the counting sink saw. Initial is the first send of every object;
// Changes is every later add, update and delete.
type sinkSummary struct {
	FullListAt    *time.Time                `json:"fullListAt"`
	ChangedBefore map[string]*recentChanges `json:"changedBefore"`
	Initial       phase                     `json:"initial"`
	Changes       phase                     `json:"changes"`
	Heartbeat     tally                     `json:"heartbeat"`
}

// countingSink is the RecordSink of a dry run: it sizes each record as the infra sink
// would post it, and sends nothing.
type countingSink struct {
	// cluster and version are the values of the attributes the infra sink adds.
	cluster, version string
	// start is the run's start, the reference time of ChangedBefore.
	start time.Time
	sum   sinkSummary
	mu    sync.Mutex
}

func newCountingSink(cluster, version string) *countingSink {
	return &countingSink{start: time.Now(), cluster: cluster, version: version, sum: sinkSummary{
		Initial: newPhase(), Changes: newPhase(), ChangedBefore: map[string]*recentChanges{},
	}}
}

// eventBytes is the json.Marshal size of r with the attributes the infra sink adds to
// every event: the size of the InfrastructureEvent the carrier sends for r.
func eventBytes(r objects.Record, cluster, version string) (int64, error) {
	b, err := json.Marshal(objects.Decorate([]objects.Record{r}, cluster, version)[0])
	if err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

// noteChangedBefore adds one first-send record to the snapshot cross-check.
func (s *countingSink) noteChangedBefore(kind string, r objects.Record) {
	rc, ok := s.sum.ChangedBefore[kind]
	if !ok {
		rc = &recentChanges{}
		s.sum.ChangedBefore[kind] = rc
	}
	rc.Objects++
	if count, _ := r.Attributes["record.count"].(int); count != 1 {
		rc.Unknown++
		return
	}
	n, _ := r.Attributes["object.parts"].(int)
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p, _ := r.Attributes[fmt.Sprintf("object.p%03d", i)].(string)
		parts = append(parts, p)
	}
	var obj struct {
		Metadata struct {
			ManagedFields []struct {
				Time time.Time `json:"time"`
			} `json:"managedFields"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(objects.Join(parts)), &obj); err != nil {
		rc.Unknown++
		return
	}
	var newest time.Time
	for _, mf := range obj.Metadata.ManagedFields {
		if mf.Time.After(newest) {
			newest = mf.Time
		}
	}
	if !newest.Before(s.start.Add(-time.Hour)) {
		rc.LastHour++
	}
	if !newest.Before(s.start.Add(-24 * time.Hour)) {
		rc.LastDay++
	}
}

func (s *countingSink) Send(_ context.Context, recs []objects.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		n, err := eventBytes(r, s.cluster, s.version)
		if err != nil {
			return err
		}
		if r.Attributes["type"] == objects.HeartbeatType {
			s.sum.Heartbeat.add(n, true)
			if ms, _ := r.Attributes["lastFullListTime"].(int64); ms > 0 && s.sum.FullListAt == nil {
				t := time.UnixMilli(ms).UTC()
				s.sum.FullListAt = &t
			}
			continue
		}
		op, _ := r.Attributes["k8s.op"].(string)
		kind, _ := r.Attributes["k8s.kind"].(string)
		idx, _ := r.Attributes["record.index"].(int)
		if op == string(objects.OpInitial) {
			s.sum.Initial.add(kind, op, n, idx == 0)
			if idx == 0 {
				s.noteChangedBefore(kind, r)
			}
		} else {
			s.sum.Changes.add(kind, op, n, idx == 0)
		}
	}
	return nil
}

// summary returns a deep copy, safe to marshal while the sink keeps counting.
func (s *countingSink) summary() sinkSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.sum)
	var out sinkSummary
	_ = json.Unmarshal(b, &out)
	return out
}
