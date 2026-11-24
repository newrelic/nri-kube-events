package objects

import (
	"context"
	"crypto/rand"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// maxShardEntries is the most entries one shard ConfigMap holds.
	maxShardEntries = 5000
	// maxShardBytes is the most encoded data (key plus value bytes) one shard holds. Real
	// entries run 180 to 250 bytes and long names push one past 300, so 5,000 entries can
	// exceed the 1 MiB ConfigMap limit. This budget keeps a shard under the limit with room
	// for metadata.
	maxShardBytes = 900_000

	// Shard ConfigMaps are named nri-kube-events-hashes-<slot>-<i>, where slot is a or b
	// by the parity of the generation, which their generation label holds. Two slots and
	// at most MaxShards shards keep the set of names finite, so the chart's Role can name
	// every ConfigMap the carrier may update or delete.
	shardNamePrefix = "nri-kube-events-hashes-"
	// MaxShards is the most shards one generation holds: 640,000 objects at 5,000 per shard.
	// A store that would need more is not written, and the carrier logs it; the objects are
	// still sent, but a restart sends every one again.
	MaxShards = 128
	// pointerName is the ConfigMap naming the current generation and its shard count.
	pointerName       = "nri-kube-events-hashes-current"
	pointerGeneration = "generation"
	pointerShards     = "shards"
	// historyStartAnnotation on the pointer holds, in RFC 3339, when this hash store began
	// recording. It is set once and kept across restarts and reshards.
	historyStartAnnotation = "nri-kube-events.newrelic.com/history-start"

	storeLabelKey      = "app.kubernetes.io/managed-by"
	storeLabelValue    = "nri-kube-events-objects"
	generationLabelKey = "nri-kube-events/generation"

	// HMACSecretName is the Secret that holds the per-cluster redaction key.
	HMACSecretName = "nri-kube-events-hmac" //nolint:gosec // G101: a Secret's name, not a credential
	// HMACSecretKey is the data key of the HMAC key inside HMACSecretName.
	HMACSecretKey = "key"
	hmacKeyBytes  = 32
)

// Store keeps the last sent hash of every object, by uid, in sharded ConfigMaps so that a
// restarted carrier skips unchanged objects and names the objects deleted while it was down.
// It is safe for concurrent use.
//
// The shards belong to a generation, and the pointer ConfigMap names the current one. A
// Flush that keeps the shard count updates the current generation's shards in place. A
// Flush that changes the count writes every shard of a new generation, then moves the
// pointer, then deletes the older generations. A crash at any step leaves the pointer on a
// complete generation, and Load merges any newer one.
type Store struct {
	client kubernetes.Interface

	// mu guards every field but client, namespace and flushMu. The fields are ordered to
	// keep pointer data first.
	entries map[string]Entry
	// dirty holds, per shard index changed since its last successful write, when it first
	// changed.
	dirty map[int]time.Time
	// reshardLeft: see reshardGen. It maps each shard to when it first needed writing.
	reshardLeft map[int]time.Time
	// historyStart is the pointer's history-start annotation, set by Load.
	historyStart time.Time
	namespace    string

	// flushMu serializes Load and Flush.
	flushMu sync.Mutex
	mu      sync.Mutex
	shards  int
	// gen is the generation the pointer names; hasPointer reports whether the pointer
	// ConfigMap exists. maxGen is the highest generation seen on the cluster or attempted
	// by a reshard, so every reshard, retries included, writes a number never used before,
	// in the slot the pointer does not name.
	gen, maxGen int
	// reshardGen is the generation a reshard in progress writes, 0 when none is; it has
	// reshardShards shards, and reshardLeft holds those not written yet. A shard that
	// changes after it was written goes to dirty, and is written in place once the pointer
	// has moved. The pointer moves to it once reshardLeft is empty.
	reshardGen    int
	reshardShards int
	hasPointer    bool
	// rewrite is set when the entries must be written into a new generation: after a
	// change of shard count, or a Load that could not trust the layout.
	rewrite bool
	// cleanup is set while shard ConfigMaps outside the current generation may exist.
	cleanup bool
	// verifyPointer is set after a failed pointer write, which may have landed.
	verifyPointer bool
	// overCapacity is set while the store needs more than MaxShards shards.
	overCapacity bool
}

// NewStore returns an empty store with one shard. Call Load to read the stored entries.
func NewStore(client kubernetes.Interface, namespace string) *Store {
	return &Store{
		client:    client,
		namespace: namespace,
		entries:   map[string]Entry{},
		shards:    1,
		dirty:     map[int]time.Time{},
	}
}

func shardOf(uid string, shards int) int {
	h := fnv.New32()
	_, _ = h.Write([]byte(uid))
	return int(h.Sum32() % uint32(shards)) //nolint:gosec // G115: shards is at least 1 and far below 2^32
}

// slotOf is the name slot of generation gen.
func slotOf(gen int) string {
	if gen%2 == 1 { //nolint:mnd // parity
		return "a"
	}
	return "b"
}

func shardName(gen, i int) string {
	return shardNamePrefix + slotOf(gen) + "-" + strconv.Itoa(i)
}

// parseShard returns the generation, from its label, and the index of a shard ConfigMap.
// A name outside the slots and MaxShards indexes, or a generation whose slot is not the name's, does not parse.
func parseShard(cm *corev1.ConfigMap) (gen, i int, ok bool) {
	rest, found := strings.CutPrefix(cm.Name, shardNamePrefix)
	if !found {
		return 0, 0, false
	}
	slot, idx, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	gen, err1 := strconv.Atoi(cm.Labels[generationLabelKey])
	i, err2 := strconv.Atoi(idx)
	if err1 != nil || err2 != nil || gen < 1 || i < 0 || i >= MaxShards || slotOf(gen) != slot {
		return 0, 0, false
	}
	return gen, i, true
}

func parsePointer(cm *corev1.ConfigMap) (gen, shards int, ok bool) {
	if cm == nil {
		return 0, 0, false
	}
	gen, err1 := strconv.Atoi(cm.Data[pointerGeneration])
	shards, err2 := strconv.Atoi(cm.Data[pointerShards])
	if err1 != nil || err2 != nil || gen < 1 || shards < 1 {
		return 0, 0, false
	}
	return gen, shards, true
}

// entryFields is the number of |-separated fields of an encoded Entry.
const entryFields = 6

func encodeEntry(e Entry) string {
	return strings.Join([]string{e.GVR, e.Kind, e.Namespace, e.Name, e.Hash, strconv.FormatInt(e.LastSent, 10)}, "|")
}

func decodeEntry(v string) (Entry, bool) {
	f := strings.Split(v, "|")
	if len(f) != entryFields {
		return Entry{}, false
	}
	ts, err := strconv.ParseInt(f[5], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	return Entry{GVR: f[0], Kind: f[1], Namespace: f[2], Name: f[3], Hash: f[4], LastSent: ts}, true
}

// readPointer reads the pointer and takes the generation it names as the one in use, so a
// reshard after a failed one never writes into the slot the pointer names.
func (s *Store) readPointer(ctx context.Context) error {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, pointerName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading hash store pointer: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifyPointer = false
	if gen, _, ok := parsePointer(cm); err == nil && ok {
		s.gen = gen
		s.hasPointer = true
		s.maxGen = max(s.maxGen, gen)
		if gen == s.reshardGen {
			// The failed pointer write landed: the reshard is done.
			s.reshardGen = 0
			s.reshardLeft = nil
			s.cleanup = true
		}
	}
	return nil
}

// shardCount is the smallest count of at least max(1, ceil(entries/5000)) at which no shard
// passes 5,000 entries or 900,000 bytes.
func shardCount(entries map[string]Entry) int {
	n := max(1, (len(entries)+maxShardEntries-1)/maxShardEntries)
	for n < MaxShards && overfull(groupEntries(entries, n)) {
		n++
	}
	return min(n, MaxShards)
}

// Load replaces the store's contents with the stored entries. When the pointer names a
// complete generation and no newer generation exists, Load reads that generation. Otherwise
// it reads every shard of every generation, keeps the newest generation's copy of each uid,
// and marks a rewrite into a new generation. Load fixes the shard count with shardCount;
// if that differs from the stored count, the next Flush reshards. Values that do not parse
// are dropped, and the objects they named are sent again as new.
func (s *Store) Load(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	list, err := s.client.CoreV1().ConfigMaps(s.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: storeLabelKey + "=" + storeLabelValue})
	if err != nil {
		return fmt.Errorf("listing hash store shards: %w", err)
	}

	ptr, byGen, maxGen := splitStoreConfigMaps(list.Items)
	ptrGen, ptrShards, ptrOK := parsePointer(ptr)
	complete := ptrOK && maxGen <= ptrGen
	for i := 0; complete && i < ptrShards; i++ {
		_, complete = byGen[ptrGen][i]
	}

	var entries map[string]Entry
	var relayout, cleanup bool
	if complete {
		entries, relayout = readGeneration(byGen[ptrGen], ptrShards)
		// Shards of older generations, or beyond the count, are leftovers of a reshard
		// that moved the pointer and then stopped before deleting them.
		cleanup = len(byGen) > 1 || len(byGen[ptrGen]) > ptrShards
	} else {
		entries = mergeGenerations(byGen)
		// Nothing stored at all is a fresh install: the first Flush with data writes
		// generation 1, and an empty store writes nothing.
		relayout = len(byGen) > 0 || ptr != nil
		cleanup = len(byGen) > 0
	}
	shards := shardCount(entries)

	historyStart, annotated := parseHistoryStart(ptr)
	if !annotated {
		historyStart = time.Now().UTC().Truncate(time.Second)
	}
	// A pointer without the annotation gets it now. With no pointer, the first write of
	// the pointer records it.
	if ptr != nil && !annotated {
		if err := annotatePointer(ctx, s.client.CoreV1().ConfigMaps(s.namespace), ptr, historyStart); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = entries
	s.shards = shards
	s.dirty = map[int]time.Time{}
	s.rewrite = relayout || (complete && shards != ptrShards)
	s.hasPointer = ptr != nil
	// The pointer's generation is the one in use even when it is incomplete, so a reshard
	// never writes into the slot it names.
	s.gen = 0
	if ptrOK {
		s.gen = ptrGen
	}
	s.reshardGen = 0
	s.reshardLeft = nil
	s.maxGen = max(maxGen, ptrGen)
	s.cleanup = cleanup
	s.historyStart = historyStart
	metricHashStoreEntries.Set(float64(len(entries)))
	return nil
}

// splitStoreConfigMaps returns the pointer among items, or nil, the shards by generation and
// index, and the highest generation of a shard. Names that parse as neither are left out.
func splitStoreConfigMaps(items []corev1.ConfigMap) (*corev1.ConfigMap, map[int]map[int]*corev1.ConfigMap, int) {
	var ptr *corev1.ConfigMap
	byGen := map[int]map[int]*corev1.ConfigMap{}
	maxGen := 0
	for k := range items {
		cm := &items[k]
		if cm.Name == pointerName {
			ptr = cm
			continue
		}
		g, i, ok := parseShard(cm)
		if !ok {
			continue
		}
		if byGen[g] == nil {
			byGen[g] = map[int]*corev1.ConfigMap{}
		}
		byGen[g][i] = cm
		maxGen = max(maxGen, g)
	}
	return ptr, byGen, maxGen
}

// readGeneration returns the entries of a complete generation's shards, and whether its
// layout needs a rewrite: a value that does not parse, a uid stored twice, or a uid in a shard
// other than its own.
func readGeneration(shards map[int]*corev1.ConfigMap, count int) (map[string]Entry, bool) {
	entries := map[string]Entry{}
	relayout := false
	for i := 0; i < count; i++ {
		for uid, v := range shards[i].Data {
			e, ok := decodeEntry(v)
			_, dup := entries[uid]
			if !ok || dup || shardOf(uid, count) != i {
				relayout = true
			}
			if ok {
				entries[uid] = e
			}
		}
	}
	return entries, relayout
}

// mergeGenerations returns the entries of every shard of every generation, the newest
// generation's copy of each uid winning.
func mergeGenerations(byGen map[int]map[int]*corev1.ConfigMap) map[string]Entry {
	gens := make([]int, 0, len(byGen))
	for g := range byGen {
		gens = append(gens, g)
	}
	sort.Ints(gens)
	entries := map[string]Entry{}
	for _, g := range gens {
		for _, cm := range byGen[g] {
			for uid, v := range cm.Data {
				if e, ok := decodeEntry(v); ok {
					entries[uid] = e
				}
			}
		}
	}
	return entries
}

// HistoryStart returns when this hash store began recording, read from the pointer by
// Load. A Load that finds no recorded value uses the current time and records it on the
// pointer: at once if the pointer exists, else at the first Flush that writes data, so a
// carrier that restarts before that Flush gets a new value. The zero time means neither
// Load nor a pointer write has run.
func (s *Store) HistoryStart() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyStart
}

// parseHistoryStart returns the pointer's history-start annotation. A missing pointer, a
// missing annotation and a value that is not RFC 3339 all report false.
func parseHistoryStart(cm *corev1.ConfigMap) (time.Time, bool) {
	if cm == nil {
		return time.Time{}, false
	}
	v, ok := cm.Annotations[historyStartAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		log.WithError(err).WithField("value", v).Warn("objects: the hash store's history-start annotation is not RFC 3339; recording a new history start")
		return time.Time{}, false
	}
	return t, true
}

// annotatePointer adds the history-start annotation to the stored pointer ptr and keeps
// its data.
func annotatePointer(ctx context.Context, cms configMapWriter, ptr *corev1.ConfigMap, historyStart time.Time) error {
	cm := ptr.DeepCopy()
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[historyStartAnnotation] = historyStart.Format(time.RFC3339)
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("annotating hash store pointer %s: %w", pointerName, err)
	}
	return nil
}

// Get returns the entry stored for uid.
func (s *Store) Get(uid string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uid]
	return e, ok
}

// Put stores e for uid. No field of e may contain "|".
func (s *Store) Put(uid string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[uid]; ok && old == e {
		return
	}
	s.entries[uid] = e
	s.markDirty(shardOf(uid, s.shards))
}

// Len returns the number of entries in the store.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Delete removes uid from the store.
func (s *Store) Delete(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[uid]; !ok {
		return
	}
	delete(s.entries, uid)
	s.markDirty(shardOf(uid, s.shards))
}

// entriesWhere returns a copy of the entries for which keep returns true.
func (s *Store) entriesWhere(keep func(Entry) bool) map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Entry{}
	for uid, e := range s.entries {
		if keep(e) {
			out[uid] = e
		}
	}
	return out
}

// Missing returns the stored uids of kind gvr that are absent from listed, the uids of a
// complete list of that kind. A list that failed never gets here, so a kind that could not
// be listed emits no deletes.
func (s *Store) Missing(gvr string, listed map[string]struct{}) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for uid, e := range s.entries {
		if e.GVR != gvr {
			continue
		}
		if _, ok := listed[uid]; !ok {
			out = append(out, uid)
		}
	}
	return out
}

// markDirty records that shard i changed, keeping the time of its first change. The
// caller holds s.mu.
func (s *Store) markDirty(i int) {
	if _, ok := s.dirty[i]; !ok {
		s.dirty[i] = time.Now()
	}
}

// Flush persists every change since the last Flush; see FlushBudget.
func (s *Store) Flush(ctx context.Context) error {
	return s.FlushBudget(ctx, 0)
}

// FlushBudget persists the changes since the last flush, writing the dirty shards that
// changed first before the others. With a positive budget it stops before a shard whose
// write would take the bytes written in this call past budget, but always writes at least
// one shard; the shards left stay dirty for the next call. A budget of 0 writes every
// dirty shard. A reshard keeps the same budget.
//
// With nothing changed it makes no API calls. If the shard count holds, it updates the
// current generation's dirty shards in place, and a shard that fails stays dirty for the
// next flush. If a shard would pass 5,000 entries or 900,000 bytes, it raises the count
// and reshards (see reshard): over one or more flushes it writes every shard of a new
// generation, then moves the pointer to it, and only then deletes the older generations.
// Until the pointer moves the old generation stays in use and untouched.
func (s *Store) FlushBudget(ctx context.Context, budget int) (err error) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	defer func() {
		if err == nil {
			metricHashStoreLastFlush.Set(float64(time.Now().Unix()))
		}
	}()

	s.mu.Lock()
	metricHashStoreEntries.Set(float64(len(s.entries)))
	if len(s.dirty) == 0 && !s.rewrite && !s.cleanup && !s.verifyPointer && s.reshardGen == 0 {
		s.mu.Unlock()
		return nil
	}
	groups, err := s.layout()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if s.verifyPointer {
		s.mu.Unlock()
		if err := s.readPointer(ctx); err != nil {
			return err
		}
		s.mu.Lock()
	}
	if s.reshardGen == 0 && (s.rewrite || !s.hasPointer) {
		s.beginReshard()
	}
	if s.reshardGen != 0 {
		gen := s.reshardGen
		write := s.takeReshard(groups, budget)
		s.mu.Unlock()
		return s.reshard(ctx, gen, groups, write)
	}
	return s.writeInPlace(ctx, groups, budget)
}

// writeInPlace writes the dirty shards of the generation in use within budget, then
// deletes leftover generations when cleanup is set. The caller holds s.mu, which
// writeInPlace releases.
func (s *Store) writeInPlace(ctx context.Context, groups []map[string]string, budget int) error {
	gen := s.gen
	write := s.takeDirty(groups, budget)
	cleanup := s.cleanup
	metricHashStoreDirtyShards.Set(float64(len(s.dirty)))
	s.mu.Unlock()

	var firstErr error
	for _, d := range write {
		if err := s.writeShard(ctx, gen, d.shard, groups[d.shard], true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.mu.Lock()
			if _, again := s.dirty[d.shard]; !again {
				s.dirty[d.shard] = d.since
			}
			metricHashStoreDirtyShards.Set(float64(len(s.dirty)))
			s.mu.Unlock()
		}
	}
	if cleanup {
		if err := s.deleteOldGenerations(ctx, gen, len(groups)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// dirtyShard is a dirty shard index and when it first changed.
type dirtyShard struct {
	since time.Time
	shard int
}

// takeDirty removes from s.dirty, and returns oldest change first, the shards one flush
// writes within budget bytes (see FlushBudget). The caller holds s.mu.
func (s *Store) takeDirty(groups []map[string]string, budget int) []dirtyShard {
	all := make([]dirtyShard, 0, len(s.dirty))
	for i, since := range s.dirty {
		all = append(all, dirtyShard{shard: i, since: since})
	}
	sort.Slice(all, func(a, b int) bool {
		if !all[a].since.Equal(all[b].since) {
			return all[a].since.Before(all[b].since)
		}
		return all[a].shard < all[b].shard
	})
	var take []dirtyShard
	written := 0
	for _, d := range all {
		n := 0
		if d.shard < len(groups) {
			n = shardBytes(groups[d.shard])
		}
		if budget > 0 && len(take) > 0 && written+n > budget {
			break
		}
		take = append(take, d)
		written += n
		delete(s.dirty, d.shard)
	}
	return take
}

// layout returns the entries grouped by shard, raising the shard count, and marking a
// reshard, until no shard is overfull. A store that would need more than MaxShards shards
// is refused with an error, an error log the first time, and the over-capacity gauge. The
// caller holds s.mu.
func (s *Store) layout() ([]map[string]string, error) {
	groups := s.group(s.shards)
	for overfull(groups) {
		if s.shards == MaxShards {
			n := len(s.entries)
			if !s.overCapacity {
				log.WithFields(log.Fields{"entries": n, "maxShards": MaxShards}).Error(
					"objects: the hash store needs more shards than the chart's Role names; it is not written, so a restart sends every object again and cannot tombstone objects deleted while down")
			}
			s.overCapacity = true
			metricHashStoreOverCapacity.Set(1)
			return nil, fmt.Errorf("hash store of %d entries needs more than %d shards; not writing it", n, MaxShards)
		}
		s.shards++
		s.rewrite = true
		groups = s.group(s.shards)
	}
	if s.overCapacity {
		s.overCapacity = false
		metricHashStoreOverCapacity.Set(0)
	}
	return groups, nil
}

// beginReshard starts a reshard into a new generation, in the slot the pointer does not
// name, so the generation in use is never written in place. The caller holds s.mu.
func (s *Store) beginReshard() {
	newGen := s.maxGen + 1
	if s.hasPointer && slotOf(newGen) == slotOf(s.gen) {
		newGen++
	}
	s.maxGen = newGen
	s.reshardGen = newGen
	s.reshardShards = 0
	s.rewrite = false
}

// takeReshard returns the shards of the reshard in progress that one flush writes within
// budget bytes (at least one; all with a budget of 0), oldest first, and removes them from
// reshardLeft. A change of shard count since the reshard began starts its shards over. A
// change to a shard that is still left only moves its time earlier; a change to a shard
// already written stays in dirty, so every flush shrinks reshardLeft and the reshard ends
// within about one flush per shard however much the store changes. The caller holds s.mu.
func (s *Store) takeReshard(groups []map[string]string, budget int) []dirtyShard {
	if s.reshardShards != len(groups) {
		now := time.Now()
		s.reshardShards = len(groups)
		s.reshardLeft = make(map[int]time.Time, len(groups))
		for i := range groups {
			s.reshardLeft[i] = now
		}
		for i, since := range s.dirty {
			if since.Before(now) && i < len(groups) {
				s.reshardLeft[i] = since
			}
		}
		s.dirty = map[int]time.Time{}
	}
	for i, since := range s.dirty {
		if left, ok := s.reshardLeft[i]; ok {
			if since.Before(left) {
				s.reshardLeft[i] = since
			}
			delete(s.dirty, i)
		}
	}
	left := make([]dirtyShard, 0, len(s.reshardLeft))
	for i, since := range s.reshardLeft {
		left = append(left, dirtyShard{shard: i, since: since})
	}
	sort.Slice(left, func(a, b int) bool {
		if !left[a].since.Equal(left[b].since) {
			return left[a].since.Before(left[b].since)
		}
		return left[a].shard < left[b].shard
	})
	var take []dirtyShard
	written := 0
	for _, d := range left {
		n := shardBytes(groups[d.shard])
		if budget > 0 && len(take) > 0 && written+n > budget {
			break
		}
		take = append(take, d)
		written += n
		delete(s.reshardLeft, d.shard)
	}
	metricHashStoreReshardLeft.Set(float64(len(s.reshardLeft)))
	metricHashStoreDirtyShards.Set(float64(len(s.dirty)))
	return take
}

// reshard writes the shards write of generation gen, the reshard in progress, from groups.
// Once every shard of gen is written it moves the pointer to gen and deletes the older
// generations. Until then the pointer stays on the old generation, which is not written
// any more: a crash in between leaves both, and the next Load merges them, newest copy of
// each uid first. A shard that fails is written again at the next flush. A pointer write
// that fails may still have landed, so the next flush reads the pointer before it
// continues.
func (s *Store) reshard(ctx context.Context, gen int, groups []map[string]string, write []dirtyShard) error {
	for k, d := range write {
		// Create falls back to Update if a shard of gen already exists.
		if err := s.writeShard(ctx, gen, d.shard, groups[d.shard], false); err != nil {
			s.mu.Lock()
			for _, again := range write[k:] {
				s.reshardLeft[again.shard] = again.since
			}
			metricHashStoreReshardLeft.Set(float64(len(s.reshardLeft)))
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Lock()
	done := len(s.reshardLeft) == 0
	s.mu.Unlock()
	if !done {
		return nil
	}
	if err := s.writePointer(ctx, gen, len(groups)); err != nil {
		s.mu.Lock()
		s.verifyPointer = true
		s.mu.Unlock()
		return err
	}
	s.finishReshard(gen)
	return s.deleteOldGenerations(ctx, gen, len(groups))
}

// finishReshard makes gen, whose pointer write succeeded, the generation in use.
func (s *Store) finishReshard(gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen = gen
	s.maxGen = max(s.maxGen, gen)
	s.hasPointer = true
	s.cleanup = true
	s.reshardGen = 0
	s.reshardLeft = nil
	metricHashStoreReshardLeft.Set(0)
}

// deleteOldGenerations deletes every shard ConfigMap outside generation gen or beyond its
// shard count. It clears the cleanup flag only when every delete succeeded.
func (s *Store) deleteOldGenerations(ctx context.Context, gen, shards int) error {
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	list, err := cms.List(ctx, metav1.ListOptions{LabelSelector: storeLabelKey + "=" + storeLabelValue})
	if err != nil {
		return fmt.Errorf("listing hash store shards: %w", err)
	}
	var firstErr error
	for _, cm := range list.Items {
		g, i, ok := parseShard(&cm)
		if (ok && g == gen && i < shards) || cm.Name == pointerName || !strings.HasPrefix(cm.Name, shardNamePrefix) {
			continue
		}
		err := cms.Delete(ctx, cm.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("deleting hash store shard %s: %w", cm.Name, err)
		}
	}
	if firstErr == nil {
		s.mu.Lock()
		s.cleanup = false
		s.mu.Unlock()
	}
	return firstErr
}

// writePointer writes the pointer naming generation gen with the given shard count. It
// carries the history-start annotation forward.
func (s *Store) writePointer(ctx context.Context, gen, shards int) error {
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	s.mu.Lock()
	exists := s.hasPointer
	historyStart := s.historyStart
	s.mu.Unlock()
	if historyStart.IsZero() {
		// Flush without a Load: this write creates the store's history.
		historyStart = time.Now().UTC().Truncate(time.Second)
		s.mu.Lock()
		s.historyStart = historyStart
		s.mu.Unlock()
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pointerName,
			Namespace: s.namespace,
			Labels:    map[string]string{storeLabelKey: storeLabelValue},
			Annotations: map[string]string{
				historyStartAnnotation: historyStart.Format(time.RFC3339),
			},
		},
		Data: map[string]string{
			pointerGeneration: strconv.Itoa(gen),
			pointerShards:     strconv.Itoa(shards),
		},
	}
	if err := upsert(ctx, cms, cm, exists); err != nil {
		return fmt.Errorf("writing hash store pointer %s: %w", pointerName, err)
	}
	return nil
}

// group returns the encoded entries of each shard for the given shard count. The caller
// holds s.mu.
func (s *Store) group(shards int) []map[string]string {
	return groupEntries(s.entries, shards)
}

func groupEntries(entries map[string]Entry, shards int) []map[string]string {
	g := make([]map[string]string, shards)
	for i := range g {
		g[i] = map[string]string{}
	}
	for uid, e := range entries {
		g[shardOf(uid, shards)][uid] = encodeEntry(e)
	}
	return g
}

// overfull reports whether any shard holds more than maxShardEntries entries or more than
// maxShardBytes of encoded data.
func overfull(groups []map[string]string) bool {
	for _, g := range groups {
		if len(g) > maxShardEntries || shardBytes(g) > maxShardBytes {
			return true
		}
	}
	return false
}

// shardBytes is the encoded size of a shard's data: the sum of its key and value bytes.
func shardBytes(data map[string]string) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

// writeShard writes shard i of generation gen. With exists it tries Update first, else
// Create first; each falls back to the other.
func (s *Store) writeShard(ctx context.Context, gen, i int, data map[string]string, exists bool) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shardName(gen, i),
			Namespace: s.namespace,
			Labels: map[string]string{
				storeLabelKey:      storeLabelValue,
				generationLabelKey: strconv.Itoa(gen),
			},
		},
		Data: data,
	}
	if err := upsert(ctx, s.client.CoreV1().ConfigMaps(s.namespace), cm, exists); err != nil {
		return fmt.Errorf("writing hash store shard %s: %w", cm.Name, err)
	}
	metricHashStoreBytesWritten.Add(float64(shardBytes(data)))
	return nil
}

type configMapWriter interface {
	Create(context.Context, *corev1.ConfigMap, metav1.CreateOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

func upsert(ctx context.Context, cms configMapWriter, cm *corev1.ConfigMap, exists bool) error {
	var err error
	if exists {
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
		}
		return err
	}
	_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

// LoadOrCreateHMACKey returns the per-cluster redaction key from Secret
// nri-kube-events-hmac. The chart creates that Secret without data, so the carrier needs no
// right to create Secrets; the first start fills it with 32 random bytes by an update. If
// the Secret is absent, as in an install without the chart, it is created. If another
// writer fills or creates it first, that key is used.
//
// A new key while the hash store already exists means the old key was lost, for example
// because a forced sync replaced the Secret: every redaction marker changes, so every
// object with a redacted value is sent once more. newKeyGenerated logs that and counts it.
func LoadOrCreateHMACKey(ctx context.Context, client kubernetes.Interface, namespace string) ([]byte, error) {
	secrets := client.CoreV1().Secrets(namespace)
	sec, err := secrets.Get(ctx, HMACSecretName, metav1.GetOptions{})
	switch {
	case err == nil && len(sec.Data[HMACSecretKey]) > 0:
		return keyFromSecret(sec)
	case err == nil:
		key, fillErr := fillHMACKey(ctx, secrets, sec)
		if fillErr == nil {
			newKeyGenerated(ctx, client, namespace)
		}
		return key, fillErr
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading secret %s: %w", HMACSecretName, err)
	}

	key, err := newHMACKey()
	if err != nil {
		return nil, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HMACSecretName,
			Namespace: namespace,
			Labels:    map[string]string{storeLabelKey: storeLabelValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{HMACSecretKey: key},
	}, metav1.CreateOptions{})
	if err == nil {
		newKeyGenerated(ctx, client, namespace)
		return key, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating secret %s: %w", HMACSecretName, err)
	}
	sec, err = secrets.Get(ctx, HMACSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading secret %s after create race: %w", HMACSecretName, err)
	}
	if len(sec.Data[HMACSecretKey]) == 0 {
		return fillHMACKey(ctx, secrets, sec)
	}
	return keyFromSecret(sec)
}

// newKeyGenerated logs a new redaction key. When the hash store already holds data, the key
// replaced a lost one: it logs a warning and counts redaction_key_replaced_total.
func newKeyGenerated(ctx context.Context, client kubernetes.Interface, namespace string) {
	list, err := client.CoreV1().ConfigMaps(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: storeLabelKey + "=" + storeLabelValue, Limit: 1})
	if err != nil || len(list.Items) == 0 {
		log.Info("objects: generated the redaction key")
		return
	}
	metricRedactionKeyReplaced.Inc()
	log.Warn("objects: generated a new redaction key although the hash store exists; the old key was lost (the key Secret was replaced or emptied), so every redaction marker changes and every object with a redacted value is sent once more")
}

// fillHMACKey writes a new key into sec, an existing Secret without one, and returns it. An
// update that conflicts with another writer returns the key that writer stored.
func fillHMACKey(ctx context.Context, secrets secretWriter, sec *corev1.Secret) ([]byte, error) {
	key, err := newHMACKey()
	if err != nil {
		return nil, err
	}
	filled := sec.DeepCopy()
	if filled.Data == nil {
		filled.Data = map[string][]byte{}
	}
	filled.Data[HMACSecretKey] = key
	if _, err = secrets.Update(ctx, filled, metav1.UpdateOptions{}); err == nil {
		return key, nil
	}
	if !apierrors.IsConflict(err) {
		return nil, fmt.Errorf("writing the key into secret %s: %w", HMACSecretName, err)
	}
	latest, err := secrets.Get(ctx, HMACSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading secret %s after an update conflict: %w", HMACSecretName, err)
	}
	return keyFromSecret(latest)
}

type secretWriter interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.Secret, error)
	Update(context.Context, *corev1.Secret, metav1.UpdateOptions) (*corev1.Secret, error)
}

func newHMACKey() ([]byte, error) {
	key := make([]byte, hmacKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating hmac key: %w", err)
	}
	return key, nil
}

func keyFromSecret(sec *corev1.Secret) ([]byte, error) {
	key := sec.Data[HMACSecretKey]
	if len(key) < hmacKeyBytes {
		return nil, fmt.Errorf("secret %s data key %q is missing or shorter than %d bytes", HMACSecretName, HMACSecretKey, hmacKeyBytes)
	}
	return key, nil
}
