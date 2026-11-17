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

	// Shard ConfigMaps are named nri-kube-events-hashes-g<gen>-<i>.
	shardNamePrefix = "nri-kube-events-hashes-g"
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
	HMACSecretName = "nri-kube-events-hmac"
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
	client    kubernetes.Interface
	namespace string

	// flushMu serializes Load and Flush; mu guards the fields below.
	flushMu sync.Mutex
	mu      sync.Mutex
	entries map[string]Entry
	shards  int
	// dirty holds the shard indexes changed since the last successful write.
	dirty map[int]struct{}
	// rewrite is set when the entries must be written into a new generation: after a
	// change of shard count, a failed reshard, or a Load that could not trust the layout.
	rewrite bool
	// gen is the generation the pointer names; hasPointer reports whether the pointer
	// ConfigMap exists. maxGen is the highest generation seen on the cluster or attempted
	// by a reshard, so every reshard, retries included, writes a number never used before.
	gen, maxGen int
	hasPointer  bool
	// cleanup is set while shard ConfigMaps outside the current generation may exist.
	cleanup bool
	// historyStart is the pointer's history-start annotation, set by Load.
	historyStart time.Time
}

// NewStore returns an empty store with one shard. Call Load to read the stored entries.
func NewStore(client kubernetes.Interface, namespace string) *Store {
	return &Store{
		client:    client,
		namespace: namespace,
		entries:   map[string]Entry{},
		shards:    1,
		dirty:     map[int]struct{}{},
	}
}

func shardOf(uid string, shards int) int {
	h := fnv.New32()
	_, _ = h.Write([]byte(uid))
	return int(h.Sum32() % uint32(shards))
}

func shardName(gen, i int) string {
	return shardNamePrefix + strconv.Itoa(gen) + "-" + strconv.Itoa(i)
}

// parseShardName returns the generation and index of a shard ConfigMap name.
func parseShardName(name string) (gen, i int, ok bool) {
	rest, found := strings.CutPrefix(name, shardNamePrefix)
	if !found {
		return 0, 0, false
	}
	g, idx, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	gen, err1 := strconv.Atoi(g)
	i, err2 := strconv.Atoi(idx)
	if err1 != nil || err2 != nil || gen < 1 || i < 0 {
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

func encodeEntry(e Entry) string {
	return strings.Join([]string{e.GVR, e.Kind, e.Namespace, e.Name, e.Hash, strconv.FormatInt(e.LastSent, 10)}, "|")
}

func decodeEntry(v string) (Entry, bool) {
	f := strings.Split(v, "|")
	if len(f) != 6 {
		return Entry{}, false
	}
	ts, err := strconv.ParseInt(f[5], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	return Entry{GVR: f[0], Kind: f[1], Namespace: f[2], Name: f[3], Hash: f[4], LastSent: ts}, true
}

// shardCount is the smallest count of at least max(1, ceil(entries/5000)) at which no shard
// passes 5,000 entries or 900,000 bytes.
func shardCount(entries map[string]Entry) int {
	n := max(1, (len(entries)+maxShardEntries-1)/maxShardEntries)
	for overfull(groupEntries(entries, n)) {
		n++
	}
	return n
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

	var ptr *corev1.ConfigMap
	byGen := map[int]map[int]*corev1.ConfigMap{}
	maxGen := 0
	for k := range list.Items {
		cm := &list.Items[k]
		if cm.Name == pointerName {
			ptr = cm
			continue
		}
		g, i, ok := parseShardName(cm.Name)
		if !ok {
			continue
		}
		if byGen[g] == nil {
			byGen[g] = map[int]*corev1.ConfigMap{}
		}
		byGen[g][i] = cm
		maxGen = max(maxGen, g)
	}

	ptrGen, ptrShards, ptrOK := parsePointer(ptr)
	complete := ptrOK && maxGen <= ptrGen
	for i := 0; complete && i < ptrShards; i++ {
		_, complete = byGen[ptrGen][i]
	}

	entries := map[string]Entry{}
	relayout := false
	cleanup := false
	if complete {
		for i := 0; i < ptrShards; i++ {
			for uid, v := range byGen[ptrGen][i].Data {
				e, ok := decodeEntry(v)
				_, dup := entries[uid]
				if !ok || dup || shardOf(uid, ptrShards) != i {
					relayout = true
				}
				if ok {
					entries[uid] = e
				}
			}
		}
		// Shards of older generations, or beyond the count, are leftovers of a reshard
		// that moved the pointer and then stopped before deleting them.
		cleanup = len(byGen) > 1 || len(byGen[ptrGen]) > ptrShards
	} else {
		gens := make([]int, 0, len(byGen))
		for g := range byGen {
			gens = append(gens, g)
		}
		sort.Ints(gens)
		for _, g := range gens {
			for _, cm := range byGen[g] {
				for uid, v := range cm.Data {
					if e, ok := decodeEntry(v); ok {
						entries[uid] = e
					}
				}
			}
		}
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
	s.dirty = map[int]struct{}{}
	s.rewrite = relayout || (complete && shards != ptrShards)
	s.hasPointer = ptr != nil
	s.gen = 0
	if complete {
		s.gen = ptrGen
	}
	s.maxGen = max(maxGen, ptrGen)
	s.cleanup = cleanup
	s.historyStart = historyStart
	metricHashStoreEntries.Set(float64(len(entries)))
	return nil
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
	s.dirty[shardOf(uid, s.shards)] = struct{}{}
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
	s.dirty[shardOf(uid, s.shards)] = struct{}{}
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

// Missing returns the stored uids of kind gvr that are absent from listed, the uids of that
// kind's initial list. When the list failed (listOK false) it returns nil, so a kind that
// could not be listed emits no deletes.
func (s *Store) Missing(gvr string, listed map[string]struct{}, listOK bool) []string {
	if !listOK {
		return nil
	}
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

// Flush persists the changes since the last Flush. With nothing changed it makes no API
// calls. If the shard count holds, Flush updates the current generation's dirty shards in
// place, and a shard that fails stays dirty for the next Flush. If a shard would pass 5,000
// entries or 900,000 bytes, Flush raises the count and reshards: it writes every shard of a
// new generation, moves the pointer to it, and only then deletes the older generations. A
// failure before the pointer moves leaves the old generation in use and untouched, and the
// next Flush retries the reshard.
func (s *Store) Flush(ctx context.Context) (err error) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	defer func() {
		if err == nil {
			metricHashStoreLastFlush.Set(float64(time.Now().Unix()))
		}
	}()

	s.mu.Lock()
	metricHashStoreEntries.Set(float64(len(s.entries)))
	if len(s.dirty) == 0 && !s.rewrite && !s.cleanup {
		s.mu.Unlock()
		return nil
	}
	groups := s.group(s.shards)
	for overfull(groups) {
		s.shards++
		s.rewrite = true
		groups = s.group(s.shards)
	}
	if s.rewrite || !s.hasPointer {
		newGen := s.maxGen + 1
		s.dirty = map[int]struct{}{}
		s.rewrite = false
		s.mu.Unlock()
		return s.reshard(ctx, newGen, groups)
	}
	gen := s.gen
	write := make([]int, 0, len(s.dirty))
	for i := range s.dirty {
		write = append(write, i)
	}
	s.dirty = map[int]struct{}{}
	cleanup := s.cleanup
	s.mu.Unlock()

	var firstErr error
	for _, i := range write {
		if err := s.writeShard(ctx, gen, i, groups[i], true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.mu.Lock()
			s.dirty[i] = struct{}{}
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

// reshard writes groups as generation gen, moves the pointer to it, and deletes every other
// shard ConfigMap. It stops at the first failure.
func (s *Store) reshard(ctx context.Context, gen int, groups []map[string]string) error {
	// A failed pointer write may still have been applied (a timeout or a lost response), so
	// the pointer can already name gen. The retry must never rewrite gen in place, so every
	// failure retires gen: the retry writes gen+1, and gen is deleted only after a later
	// pointer move succeeds.
	fail := func(err error) error {
		s.mu.Lock()
		s.rewrite = true
		s.maxGen = max(s.maxGen, gen)
		s.mu.Unlock()
		return err
	}
	// Create falls back to Update if a shard of gen already exists.
	for i := range groups {
		if err := s.writeShard(ctx, gen, i, groups[i], false); err != nil {
			return fail(err)
		}
	}
	if err := s.writePointer(ctx, gen, len(groups)); err != nil {
		return fail(err)
	}
	s.mu.Lock()
	s.gen = gen
	s.maxGen = gen
	s.hasPointer = true
	s.cleanup = true
	s.mu.Unlock()
	return s.deleteOldGenerations(ctx, gen, len(groups))
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
		g, i, ok := parseShardName(cm.Name)
		if !ok || (g == gen && i < shards) {
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
// nri-kube-events-hmac, creating the Secret with 32 random bytes when it is absent. If
// another writer creates it first, the existing key is used.
func LoadOrCreateHMACKey(ctx context.Context, client kubernetes.Interface, namespace string) ([]byte, error) {
	secrets := client.CoreV1().Secrets(namespace)
	sec, err := secrets.Get(ctx, HMACSecretName, metav1.GetOptions{})
	if err == nil {
		return keyFromSecret(sec)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading secret %s: %w", HMACSecretName, err)
	}

	key := make([]byte, hmacKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating hmac key: %w", err)
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
		return key, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating secret %s: %w", HMACSecretName, err)
	}
	sec, err = secrets.Get(ctx, HMACSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading secret %s after create race: %w", HMACSecretName, err)
	}
	return keyFromSecret(sec)
}

func keyFromSecret(sec *corev1.Secret) ([]byte, error) {
	key := sec.Data[HMACSecretKey]
	if len(key) < hmacKeyBytes {
		return nil, fmt.Errorf("secret %s data key %q is missing or shorter than %d bytes", HMACSecretName, HMACSecretKey, hmacKeyBytes)
	}
	return key, nil
}
