package objects

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

const (
	DefaultDiscoveryInterval = 5 * time.Minute
	DefaultHeartbeatInterval = 60 * time.Second
	DefaultFlushInterval     = 30 * time.Second
	DefaultResendInterval    = time.Minute

	// AccountFlagNotRequired is the heartbeat's carrier.accountFlag when the carrier runs
	// without the account collection flag.
	AccountFlagNotRequired = "not_required"

	// discoveryRetry is how soon a discovery that failed outright is tried again.
	discoveryRetry = 10 * time.Second
	// shutdownFlushTimeout bounds the store flush after the run context is canceled.
	shutdownFlushTimeout = 30 * time.Second
	// heartbeatSendTimeout bounds one heartbeat send.
	heartbeatSendTimeout = 30 * time.Second

	crdGroup    = "apiextensions.k8s.io"
	crdResource = "customresourcedefinitions"
)

// Config configures a Watcher. A zero interval takes its default.
type Config struct {
	// Skip lists resources the watcher never watches. The objects-estimate tool skips
	// Secrets so that no Secret value reaches the machine it runs on.
	Skip map[schema.GroupVersionResource]bool
	// AccountFlag reports the account collection flag for the heartbeat's
	// carrier.accountFlag. Nil reports AccountFlagNotRequired.
	AccountFlag func() string

	ClusterName, Namespace                                              string
	DiscoveryInterval, HeartbeatInterval, FlushInterval, ResendInterval time.Duration
	// ResendAfter is the age of the last send after which an unchanged object is sent
	// again. Zero turns the resend off; it takes no default.
	ResendAfter time.Duration
}

func (c Config) withDefaults() Config {
	if c.DiscoveryInterval <= 0 {
		c.DiscoveryInterval = DefaultDiscoveryInterval
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.ResendInterval <= 0 {
		c.ResendInterval = DefaultResendInterval
	}
	return c
}

// gvrState is the watcher's view of one watched kind.
type gvrState struct {
	// cancel stops the kind's reflector. It is nil for a kind tracked without one.
	cancel context.CancelFunc
	// store is the kind's reflector store. Its mu orders the kind's watch events, and a
	// retry of one of the kind's objects takes it too. It is nil without a reflector.
	store *objectStore
	kind  string
	// listed is set when the kind's first list reached the store.
	listed bool
	// resolved is set when the kind's first list succeeded or failed.
	resolved bool
	// listFailing is set while the kind's most recent list failed.
	listFailing bool
}

// Watcher discovers every listable and watchable kind, watches each with a reflector, and
// sends a record whenever an object's hash changes, a tombstone when an object goes, and a
// heartbeat on a timer. The reflectors keep no objects; the Store keeps one Entry per uid.
type Watcher struct {
	dyn   dynamic.Interface
	disc  discovery.DiscoveryInterface
	sink  RecordSink
	store *Store
	now   func() time.Time

	// rediscover asks Run for a discovery pass. It holds at most one pending request.
	rediscover chan struct{}
	// afterRetryGet, when set, is called by a retry after it fetched an object and before
	// it sends it. Tests use it to deliver a watch event at that point.
	afterRetryGet func(uid string)
	// resendGetTimeout bounds the GET of one resend; NewWatcher sets resendGetTimeout.
	resendGetTimeout time.Duration

	// mu guards gvrs, unsynced, startTime, lastFullList, failedSends and resyncCursor.
	// The fields are ordered to keep pointer data first, so the guarded ones are not
	// contiguous.
	gvrs map[schema.GroupVersionResource]*gvrState
	// unsynced holds, per uid, every object whose last send failed and has not succeeded
	// since. NRDB may hold an older version of these objects, or still hold a deleted one.
	unsynced     map[string]unsyncedObject
	startTime    time.Time
	lastFullList time.Time

	key []byte
	cfg Config

	mu          sync.Mutex
	failedSends int64
	// resyncCursor is where the next resend scan starts; the heartbeat reports it.
	resyncCursor uint64

	// reflectors counts running reflector goroutines, so shutdown can wait for them.
	reflectors sync.WaitGroup
	// background counts the heartbeat, retry and resend goroutines, so shutdown can wait for
	// them.
	background sync.WaitGroup
	// heartbeatBusy, retryBusy and resendBusy are set while a heartbeat send, a retry pass
	// or a resend scan runs.
	heartbeatBusy, retryBusy, resendBusy atomic.Bool
}

// unsyncedObject is what a retry needs: the object's identity, to fetch it again or to
// tombstone it, and the op of the send that failed. OpDeleted means a tombstone failed,
// and observedAt is when the carrier saw the deletion, the retried tombstone's timestamp.
// since is when the object first became unsynced; retries go oldest first.
type unsyncedObject struct {
	since      time.Time
	observedAt time.Time
	op         Op
	entry      Entry
}

// maxRetryPerPass is the most unsynced objects one retry pass sends. The rest wait for the
// next flush tick.
const maxRetryPerPass = 500

// NewWatcher returns a Watcher. The caller loads store before Run. hmacKey keys the Secret
// redaction and is never sent.
func NewWatcher(cfg Config, dyn dynamic.Interface, disc discovery.DiscoveryInterface, store *Store, sink RecordSink, hmacKey []byte) *Watcher {
	return &Watcher{
		cfg:              cfg.withDefaults(),
		dyn:              dyn,
		disc:             disc,
		store:            store,
		sink:             sink,
		key:              hmacKey,
		now:              time.Now,
		resendGetTimeout: resendGetTimeout,
		rediscover:       make(chan struct{}, 1),
		gvrs:             map[schema.GroupVersionResource]*gvrState{},
		unsynced:         map[string]unsyncedObject{},
	}
}

// Run starts a watch for every discovered kind, and runs discovery, heartbeat and flush on
// their timers until ctx is canceled. It then stops the watches and flushes the store once
// more, and returns that flush's error. Run never loads the store: main loads it once, with
// a startup deadline, and exits if that fails. Heartbeats and retries of failed sends run
// in their own goroutines, as do the resend scans of unchanged objects while ResendAfter is
// set, so the discovery and flush ticks never wait on the Log API.
func (w *Watcher) Run(ctx context.Context) error {
	w.dropOwnStoreEntries()
	w.mu.Lock()
	w.startTime = w.now()
	w.mu.Unlock()

	w.syncWatches(ctx)
	w.startHeartbeat(ctx)

	discoveryT := time.NewTicker(w.cfg.DiscoveryInterval)
	defer discoveryT.Stop()
	heartbeatT := time.NewTicker(w.cfg.HeartbeatInterval)
	defer heartbeatT.Stop()
	flushT := time.NewTicker(w.cfg.FlushInterval)
	defer flushT.Stop()
	// resendC stays nil, and never fires, while the resend is off.
	var resendC <-chan time.Time
	if w.cfg.ResendAfter > 0 {
		resendT := time.NewTicker(w.cfg.ResendInterval)
		defer resendT.Stop()
		resendC = resendT.C
	}

	for {
		select {
		case <-ctx.Done():
			return w.shutdown()
		case <-discoveryT.C:
			w.syncWatches(ctx)
		case <-w.rediscover:
			w.syncWatches(ctx)
		case <-heartbeatT.C:
			w.startHeartbeat(ctx)
		case <-flushT.C:
			w.startRetry(ctx)
			if err := w.store.Flush(ctx); err != nil {
				log.WithError(err).Warn("objects: flushing hash store")
			}
		case <-resendC:
			w.startResend(ctx)
		}
	}
}

// shutdown stops every watch, waits for the reflectors to return, and flushes the store.
func (w *Watcher) shutdown() error {
	w.mu.Lock()
	for _, st := range w.gvrs {
		if st.cancel != nil {
			st.cancel()
		}
	}
	w.mu.Unlock()
	w.reflectors.Wait()
	w.background.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
	defer cancel()
	if err := w.store.Flush(ctx); err != nil {
		return fmt.Errorf("flushing hash store on shutdown: %w", err)
	}
	return nil
}

func (w *Watcher) triggerRediscovery() {
	select {
	case w.rediscover <- struct{}{}:
	default:
	}
}

// discover runs Discover with the watcher's discovery client and skip list.
func (w *Watcher) discover() (map[schema.GroupVersionResource]string, map[string]struct{}, error) {
	return Discover(w.disc, w.cfg.Skip)
}

// Discover returns every resource with the list and watch verbs, in the version its group
// prefers, mapped to its kind. Subresources, both events resources and the resources in
// skip are left out. When some groups fail discovery, it returns the others and names the
// failed groups.
func Discover(disc discovery.DiscoveryInterface, skip map[schema.GroupVersionResource]bool) (found map[schema.GroupVersionResource]string, failedGroups map[string]struct{}, err error) {
	lists, err := discovery.ServerPreferredResources(disc)
	failed := map[string]struct{}{}
	if err != nil {
		var groupErr *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &groupErr) {
			return nil, nil, err
		}
		for gv, gerr := range groupErr.Groups {
			failed[gv.Group] = struct{}{}
			log.WithError(gerr).WithField("groupVersion", gv.String()).Warn("objects: group discovery failed")
		}
	}
	found = map[schema.GroupVersionResource]string{}
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			log.WithError(err).WithField("groupVersion", list.GroupVersion).Warn("objects: unparseable group version")
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !hasVerbs(r.Verbs, "list", "watch") {
				continue
			}
			if r.Name == "events" && (gv.Group == "" || gv.Group == "events.k8s.io") {
				continue
			}
			if skip[gv.WithResource(r.Name)] {
				continue
			}
			found[gv.WithResource(r.Name)] = r.Kind
		}
	}
	return found, failed, nil
}

func hasVerbs(verbs metav1.Verbs, want ...string) bool {
	for _, v := range want {
		if !slices.Contains(verbs, v) {
			return false
		}
	}
	return true
}

// syncWatches runs discovery, starts a watch for every new kind and stops the watch of
// every kind that left discovery. A kind whose group failed discovery keeps its watch.
// Stopping a watch sends nothing: a removed CRD's objects are tombstoned when the CRD
// watch delivers the CRD's delete.
func (w *Watcher) syncWatches(ctx context.Context) {
	found, failed, err := w.discover()
	if err != nil {
		log.WithError(err).Warn("objects: discovery failed")
		time.AfterFunc(discoveryRetry, w.triggerRediscovery)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	for gvr, kind := range found {
		if _, ok := w.gvrs[gvr]; !ok {
			w.startLocked(ctx, gvr, kind)
		}
	}
	removedPending := false
	for gvr, st := range w.gvrs {
		if _, ok := found[gvr]; ok {
			continue
		}
		if _, ok := failed[gvr.Group]; ok {
			continue
		}
		log.WithField("gvr", GVRString(gvr)).Info("objects: kind left discovery, stopping its watch")
		if st.cancel != nil {
			st.cancel()
		}
		if !st.resolved {
			removedPending = true
		}
		delete(w.gvrs, gvr)
	}
	if removedPending && len(w.gvrs) > 0 && w.allResolvedLocked() {
		w.lastFullList = w.now()
	}
}

// startLocked starts the reflector of gvr. The caller holds w.mu.
func (w *Watcher) startLocked(ctx context.Context, gvr schema.GroupVersionResource, kind string) {
	wctx, cancel := context.WithCancel(ctx)
	store := w.newObjectStore(wctx, gvr)
	w.gvrs[gvr] = &gvrState{kind: kind, cancel: cancel, store: store}
	res := w.dyn.Resource(gvr)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			list, err := res.List(ctx, opts)
			if err != nil {
				w.listFailed(gvr, err)
				return nil, err
			}
			return list, nil
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			return res.Watch(ctx, opts)
		},
	}
	r := cache.NewReflectorWithOptions(
		cache.ToListWatcherWithWatchListSemantics(lw, w.dyn),
		&unstructured.Unstructured{},
		store,
		cache.ReflectorOptions{Name: "objects " + GVRString(gvr)},
	)
	log.WithFields(log.Fields{"gvr": GVRString(gvr), "kind": kind}).Debug("objects: starting watch")
	w.reflectors.Add(1)
	go func() {
		defer w.reflectors.Done()
		r.RunWithContext(wctx)
	}()
}

// track records gvr as watched without starting a reflector.
func (w *Watcher) track(gvr schema.GroupVersionResource, kind string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.gvrs[gvr]; !ok {
		w.gvrs[gvr] = &gvrState{kind: kind}
	}
}

// watching reports whether gvr is watched.
func (w *Watcher) watching(gvr schema.GroupVersionResource) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.gvrs[gvr]
	return ok
}

func (w *Watcher) kindOf(gvr schema.GroupVersionResource) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, ok := w.gvrs[gvr]; ok {
		return st.kind
	}
	return ""
}

// listFailed marks the first list of gvr as finished without objects. The kind stays out
// of the heartbeat's kinds until a list succeeds. Only the first failure is logged.
func (w *Watcher) listFailed(gvr schema.GroupVersionResource, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	if !ok {
		return
	}
	st.listFailing = true
	if !st.resolved && err != nil {
		log.WithError(err).WithField("gvr", GVRString(gvr)).Warn("objects: list failed; the kind sends no deletes until a list succeeds")
	}
	w.resolveLocked(st)
}

// FailedGVRs returns, sorted, the watched resources whose most recent list failed.
func (w *Watcher) FailedGVRs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for gvr, st := range w.gvrs {
		if st.listFailing {
			out = append(out, GVRString(gvr))
		}
	}
	sort.Strings(out)
	return out
}

// markListed marks gvr as listed: its kind joins the heartbeat's kinds.
func (w *Watcher) markListed(gvr schema.GroupVersionResource) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	if !ok {
		return
	}
	st.listed = true
	st.listFailing = false
	w.resolveLocked(st)
}

// resolveLocked marks st's first list as finished. When that leaves no kind waiting for
// its first list, the full list is complete. The caller holds w.mu.
func (w *Watcher) resolveLocked(st *gvrState) {
	if st.resolved {
		return
	}
	st.resolved = true
	if w.allResolvedLocked() {
		w.lastFullList = w.now()
	}
}

func (w *Watcher) allResolvedLocked() bool {
	for _, st := range w.gvrs {
		if !st.resolved {
			return false
		}
	}
	return true
}

// canceled reports whether ctx was canceled. A send cut short by shutdown or by a stopped
// watch is not a delivery failure. A send that ran past its deadline is one.
func canceled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// countFailedSend counts a failed send, unless ctx was canceled.
func (w *Watcher) countFailedSend(ctx context.Context) {
	if canceled(ctx) {
		return
	}
	metricSendFailures.Inc()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failedSends++
}

// markUnsynced records that the send of uid failed, unless ctx was canceled. A canceled
// send leaves an existing record of uid in place.
func (w *Watcher) markUnsynced(ctx context.Context, uid string, u unsyncedObject) {
	if canceled(ctx) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// An object that fails again keeps its place in the retry order.
	if old, ok := w.unsynced[uid]; ok {
		u.since = old.since
	} else if u.since.IsZero() {
		u.since = w.now()
	}
	w.unsynced[uid] = u
}

// markSynced records that NRDB holds the version of uid that the store holds, or, after a
// tombstone, that NRDB knows uid is gone.
func (w *Watcher) markSynced(uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.unsynced, uid)
}

// startRetry starts a retry pass in its own goroutine, unless one is still running.
func (w *Watcher) startRetry(ctx context.Context) {
	if !w.retryBusy.CompareAndSwap(false, true) {
		return
	}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer w.retryBusy.Store(false)
		w.retryUnsynced(ctx)
	}()
}

// retryUnsynced resends every unsynced object. A failed tombstone is sent again as a
// tombstone. A failed record is sent again from the object as the apiserver serves it now;
// when the apiserver no longer serves it, or serves a new object of the same name, the
// object is tombstoned. Each success removes the uid from the unsynced set.
//
// One pass retries at most maxRetryPerPass objects, oldest failure first. Each retry holds
// the reflector store lock of the object's kind across its GET and its send, the same lock
// that orders the kind's watch events. Without it, a watch event that lands between the
// retry's GET and its send would be overwritten: a newer version by the older one the
// retry fetched, or a delete by a live version that puts the uid back in the store.
func (w *Watcher) retryUnsynced(ctx context.Context) {
	type pendingObject struct {
		uid string
		u   unsyncedObject
	}
	w.mu.Lock()
	pending := make([]pendingObject, 0, len(w.unsynced))
	for uid, u := range w.unsynced {
		pending = append(pending, pendingObject{uid, u})
	}
	w.mu.Unlock()
	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].u.since.Equal(pending[j].u.since) {
			return pending[i].u.since.Before(pending[j].u.since)
		}
		return pending[i].uid < pending[j].uid
	})
	if len(pending) > maxRetryPerPass {
		pending = pending[:maxRetryPerPass]
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		w.retryOne(ctx, p.uid, p.u)
	}
}

// retryOne retries the unsynced object uid, whose record was u when the pass started.
func (w *Watcher) retryOne(ctx context.Context, uid string, u unsyncedObject) {
	gvr, gvrErr := ParseGVR(u.entry.GVR)
	if gvrErr == nil {
		if s := w.reflectorStore(gvr); s != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
		}
	}
	// A watch event handled since the pass started has already sent this object, or
	// replaced the failure being retried with a newer one.
	if !w.stillUnsynced(uid, u) {
		return
	}
	if u.op == OpDeleted {
		w.sendTombstone(ctx, uid, u.entry, u.observedAt)
		return
	}
	if gvrErr != nil {
		log.WithError(gvrErr).WithField("uid", uid).Warn("objects: dropping unsynced object with an unparseable GVR")
		w.markSynced(uid)
		return
	}
	var res dynamic.ResourceInterface = w.dyn.Resource(gvr)
	if u.entry.Namespace != "" {
		res = w.dyn.Resource(gvr).Namespace(u.entry.Namespace)
	}
	obj, err := res.Get(ctx, u.entry.Name, metav1.GetOptions{})
	if w.afterRetryGet != nil {
		w.afterRetryGet(uid)
	}
	// A retry that finds the object gone, or its name taken by a new object, is when the
	// carrier observes the deletion.
	switch {
	case apierrors.IsNotFound(err):
		w.sendTombstone(ctx, uid, w.tombstoneEntry(uid, u.entry), w.now())
	case err != nil:
		log.WithError(err).WithFields(log.Fields{"gvr": u.entry.GVR, "uid": uid}).Warn("objects: fetching unsynced object")
	case string(obj.GetUID()) != uid:
		// The name now belongs to a new object, which its own watch sends.
		w.sendTombstone(ctx, uid, w.tombstoneEntry(uid, u.entry), w.now())
	default:
		if err := w.HandleObject(ctx, gvr, u.op, obj.Object); err != nil {
			log.WithError(err).WithFields(log.Fields{"gvr": u.entry.GVR, "uid": uid}).Warn("objects: retrying unsynced object")
		}
	}
}

// isListed reports whether gvr has a reflector whose first list succeeded.
func (w *Watcher) isListed(gvr schema.GroupVersionResource) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	return ok && st.cancel != nil && st.listed
}

// reflectorStore returns the reflector store of gvr, or nil when gvr has no reflector.
func (w *Watcher) reflectorStore(gvr schema.GroupVersionResource) *objectStore {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	if !ok || st.cancel == nil {
		return nil
	}
	return st.store
}

// stillUnsynced reports whether the unsynced record of uid is still u.
func (w *Watcher) stillUnsynced(uid string, u unsyncedObject) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.unsynced[uid]
	return ok && cur == u
}

// tombstoneEntry returns the stored identity of uid, or fallback when the store holds none.
func (w *Watcher) tombstoneEntry(uid string, fallback Entry) Entry {
	if e, ok := w.store.Get(uid); ok {
		return e
	}
	return fallback
}

// HandleObject redacts obj, hashes it, and sends its records unless the store already
// holds that hash for its uid. After a successful send it stores the new Entry, or, for
// OpDeleted, forgets the uid. A failed send leaves the store unchanged and marks the uid
// unsynced, so the next flush tick or the next event for the object sends again. obj
// itself is never modified.
func (w *Watcher) HandleObject(ctx context.Context, gvr schema.GroupVersionResource, op Op, obj map[string]any) error {
	return w.handle(ctx, gvr, op, obj, false)
}

// handle is HandleObject. With force set it sends obj even when the store already holds
// its hash.
func (w *Watcher) handle(ctx context.Context, gvr schema.GroupVersionResource, op Op, obj map[string]any, force bool) error {
	if IsOwnStoreObject(w.cfg.Namespace, gvr, obj) {
		// A store written by an older carrier can hold an entry for this object.
		if uid := objectUID(obj); uid != "" {
			w.store.Delete(uid)
			w.markSynced(uid)
		}
		return nil
	}
	red, hash := PrepareObject(gvr, w.kindOf(gvr), obj, w.key)
	meta, _ := red["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	if uid == "" {
		return fmt.Errorf("%s object has no metadata.uid", GVRString(gvr))
	}
	gvrString := GVRString(gvr)
	kind, _ := red["kind"].(string)
	namespace, _ := meta["namespace"].(string)
	name, _ := meta["name"].(string)

	if op != OpDeleted && !force {
		if old, ok := w.store.Get(uid); ok && old.Hash == hash {
			if old.GVR != gvrString {
				old.GVR = gvrString
				w.store.Put(uid, old)
			}
			// NRDB already holds this version.
			w.markSynced(uid)
			return nil
		}
	}

	now := w.now()
	recs, err := BuildRecords(red, gvr, op, w.cfg.ClusterName, now)
	if err != nil {
		return fmt.Errorf("building records for %s %s: %w", gvrString, uid, err)
	}
	if err := w.sink.Send(ctx, recs); err != nil {
		w.countFailedSend(ctx)
		identity := Entry{GVR: gvrString, Kind: kind, Namespace: namespace, Name: name}
		if op == OpDeleted {
			identity = w.tombstoneEntry(uid, identity)
		}
		u := unsyncedObject{entry: identity, op: op}
		if op == OpDeleted {
			u.observedAt = now
		}
		w.markUnsynced(ctx, uid, u)
		return fmt.Errorf("sending %s %s: %w", gvrString, uid, err)
	}
	w.markSynced(uid)
	if op == OpDeleted {
		w.store.Delete(uid)
		return nil
	}
	// A first-ever send stores a LastSent spread across ResendAfter, so the objects of one
	// startup come due for the resend spread out and not all at once.
	lastSent := now.Unix()
	if _, known := w.store.Get(uid); !known && w.cfg.ResendAfter > 0 {
		lastSent = firstSentAt(uid, now, w.cfg.ResendAfter)
	}
	w.store.Put(uid, Entry{GVR: gvrString, Kind: kind, Namespace: namespace, Name: name, Hash: hash, LastSent: lastSent})
	return nil
}

// PrepareObject returns obj as the watcher sends it, and its hash: apiVersion and kind
// filled in where obj lacks them (kind is the kind discovery reports for gvr, and may be
// empty), then redacted. Every object of core v1 secrets is redacted as a Secret, because
// a watch object can lack its own kind field. obj itself is never modified.
func PrepareObject(gvr schema.GroupVersionResource, kind string, obj map[string]any, hmacKey []byte) (redacted map[string]any, hash string) {
	typed := withTypeMeta(gvr, kind, obj)
	if gvr == secretsGVR {
		redacted = RedactSecret(typed, hmacKey)
	} else {
		redacted = Redact(typed, hmacKey)
	}
	return redacted, Hash(redacted)
}

// withTypeMeta returns obj with apiVersion set from gvr and kind set from discoveredKind
// where obj lacks them. It returns obj itself when both are set, and otherwise a shallow
// copy, so obj is never modified.
func withTypeMeta(gvr schema.GroupVersionResource, discoveredKind string, obj map[string]any) map[string]any {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	if apiVersion != "" && kind != "" {
		return obj
	}
	out := maps.Clone(obj)
	if apiVersion == "" {
		out["apiVersion"] = gvr.GroupVersion().String()
	}
	if kind == "" {
		kind = discoveredKind
		if kind == "" && gvr == secretsGVR {
			kind = "Secret"
		}
		if kind != "" {
			out["kind"] = kind
		}
	}
	return out
}

var (
	configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretsGVR    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// IsOwnStoreObject reports whether obj is one of the carrier's own hash-store objects in
// carrierNamespace: a shard ConfigMap (by its managed-by label), the pointer ConfigMap, or
// the HMAC Secret. The watcher sends and stores none of them, because every flush rewrites
// the shards and sending them would report the carrier's own writes. An empty
// carrierNamespace matches nothing.
func IsOwnStoreObject(carrierNamespace string, gvr schema.GroupVersionResource, obj map[string]any) bool {
	meta, _ := obj["metadata"].(map[string]any)
	namespace, _ := meta["namespace"].(string)
	if carrierNamespace == "" || namespace != carrierNamespace {
		return false
	}
	name, _ := meta["name"].(string)
	switch gvr {
	case configMapsGVR:
		labels, _ := meta["labels"].(map[string]any)
		managedBy, _ := labels[storeLabelKey].(string)
		return managedBy == storeLabelValue || name == pointerName
	case secretsGVR:
		return name == HMACSecretName
	}
	return false
}

// isOwnStoreName reports whether namespace/name in gvr names one of the carrier's own
// hash-store objects. It is the check for the paths that carry no object, so a shard is
// matched by its name prefix in place of its label.
func (w *Watcher) isOwnStoreName(gvr schema.GroupVersionResource, namespace, name string) bool {
	if w.cfg.Namespace == "" || namespace != w.cfg.Namespace {
		return false
	}
	switch gvr {
	case configMapsGVR:
		return name == pointerName || strings.HasPrefix(name, shardNamePrefix)
	case secretsGVR:
		return name == HMACSecretName
	}
	return false
}

// dropOwnStoreEntries removes from the store every entry of the carrier's own hash-store
// objects, which a store written by an older carrier can hold, and sends nothing for them.
func (w *Watcher) dropOwnStoreEntries() {
	entries := w.store.entriesWhere(func(e Entry) bool {
		gvr, err := ParseGVR(e.GVR)
		return err == nil && w.isOwnStoreName(gvr, e.Namespace, e.Name)
	})
	for uid := range entries {
		w.store.Delete(uid)
	}
	if len(entries) > 0 {
		log.WithField("count", len(entries)).Info("objects: dropped stored entries of the carrier's own hash-store objects")
	}
}

func objectUID(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	return uid
}

// sendTombstone sends the delete record of uid from its stored identity, with observedAt,
// when the carrier saw the deletion, as its timestamp, and once sent forgets uid. A failed
// tombstone marks uid unsynced, to be sent again as a tombstone with the same observedAt.
func (w *Watcher) sendTombstone(ctx context.Context, uid string, e Entry, observedAt time.Time) {
	rec := Tombstone(uid, e, w.cfg.ClusterName, observedAt)
	if err := w.sink.Send(ctx, []Record{rec}); err != nil {
		w.countFailedSend(ctx)
		w.markUnsynced(ctx, uid, unsyncedObject{entry: e, op: OpDeleted, observedAt: observedAt})
		log.WithError(err).WithFields(log.Fields{"gvr": e.GVR, "kind": e.Kind, "uid": uid}).Warn("objects: sending tombstone")
		return
	}
	w.markSynced(uid)
	w.store.Delete(uid)
}

// forgetCRDObjects tombstones every stored object of the custom resource group/plural, in
// any version. It runs only after the CRD's own delete was seen.
func (w *Watcher) forgetCRDObjects(ctx context.Context, group, plural string) {
	if group == "" || plural == "" {
		return
	}
	entries := w.store.entriesWhere(func(e Entry) bool {
		gvr, err := ParseGVR(e.GVR)
		return err == nil && gvr.Group == group && gvr.Resource == plural
	})
	now := w.now()
	for uid, e := range entries {
		w.sendTombstone(ctx, uid, e, now)
	}
}

// crdTarget returns the group and plural a CRD serves, from its spec, or from its name
// <plural>.<group> when the spec lacks them.
func crdTarget(obj map[string]any) (group, plural string) {
	spec, _ := obj["spec"].(map[string]any)
	names, _ := spec["names"].(map[string]any)
	group, _ = spec["group"].(string)
	plural, _ = names["plural"].(string)
	if group != "" && plural != "" {
		return group, plural
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return crdTargetFromName(name)
}

func crdTargetFromName(name string) (group, plural string) {
	plural, group, ok := strings.Cut(name, ".")
	if !ok {
		return "", ""
	}
	return group, plural
}

func isCRDResource(gvr schema.GroupVersionResource) bool {
	return gvr.Group == crdGroup && gvr.Resource == crdResource
}

// startHeartbeat sends a heartbeat from its own goroutine, bounded by heartbeatSendTimeout.
// While the previous heartbeat is still sending, it skips this one.
func (w *Watcher) startHeartbeat(ctx context.Context) {
	if !w.heartbeatBusy.CompareAndSwap(false, true) {
		log.Debug("objects: previous heartbeat still sending, skipping this one")
		return
	}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer w.heartbeatBusy.Store(false)
		hctx, cancel := context.WithTimeout(ctx, heartbeatSendTimeout)
		defer cancel()
		w.sendHeartbeat(hctx)
	}()
}

func (w *Watcher) sendHeartbeat(ctx context.Context) {
	if err := w.sink.Send(ctx, []Record{w.HeartbeatRecord(w.now())}); err != nil {
		w.countFailedSend(ctx)
		log.WithError(err).Warn("objects: sending heartbeat")
	}
}

// HeartbeatRecord builds the carrier heartbeat and sets the objects gauge from the same
// counts. carrier.kinds lists, as sorted
// "Kind=count," pairs cut by Split into carrier.kinds.pNNN parts, every watched kind whose
// list has succeeded, with the count of its objects in the store; a kind with no objects
// is listed with 0. carrier.failedKinds lists, sorted and comma-separated, the GVRs whose
// most recent list failed; capList drops whole GVRs from the end to keep it within one
// attribute. timestamp is Unix seconds. The other times are Unix milliseconds, 0 before
// they happen.
func (w *Watcher) HeartbeatRecord(now time.Time) Record {
	w.mu.Lock()
	kindOfGVR := map[string]string{}
	var failedGVRs []string
	for gvr, st := range w.gvrs {
		if st.listed {
			kindOfGVR[GVRString(gvr)] = st.kind
		}
		if st.listFailing {
			failedGVRs = append(failedGVRs, GVRString(gvr))
		}
	}
	start, full, failed, unsynced, cursor := w.startTime, w.lastFullList, w.failedSends, len(w.unsynced), w.resyncCursor
	w.mu.Unlock()
	sort.Strings(failedGVRs)
	failedList, _ := capList(failedGVRs)

	accountFlag := AccountFlagNotRequired
	if w.cfg.AccountFlag != nil {
		accountFlag = w.cfg.AccountFlag()
	}

	counts := map[string]int{}
	for _, kind := range kindOfGVR {
		counts[kind] = 0
	}
	entries := w.store.entriesWhere(func(e Entry) bool {
		_, ok := kindOfGVR[e.GVR]
		return ok
	})
	for _, e := range entries {
		counts[kindOfGVR[e.GVR]]++
	}
	setObjectsMetric(counts)
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var b strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&b, "%s=%d,", k, counts[k])
	}

	attrs := map[string]any{
		"type":              HeartbeatType,
		"timestamp":         now.Unix(),
		"k8s.cluster.name":  w.cfg.ClusterName,
		"carrier.startTime": unixMilli(start),
		"lastFullListTime":  unixMilli(full),
		"failedSends":       failed,
		"carrier.kindCount": len(kinds),
		// carrier.failedKindCount is the number of watched GVRs whose most recent list
		// failed; 0 means every watched kind is collected. carrier.failedKinds names
		// them, and can name fewer when the list was capped.
		"carrier.failedKindCount": len(failedGVRs),
		"carrier.failedKinds":     strings.Trim(failedList, ","),
		"carrier.accountFlag":     accountFlag,
		"carrier.resyncCursor":    fmt.Sprintf("%016x", cursor),
		// carrier.historyStart is when the hash store began recording, which survives
		// restarts.
		"carrier.historyStart": unixMilli(w.store.HistoryStart()),
		// carrier.unsyncedObjects is the number of objects whose last send failed and has
		// not succeeded since; 0 means NRDB holds the version the carrier last saw of
		// every object.
		"carrier.unsyncedObjects": unsynced,
	}
	for i, p := range Split(b.String()) {
		attrs[fmt.Sprintf("carrier.kinds.p%03d", i)] = p
	}
	return Record{
		Summary:    fmt.Sprintf("carrier heartbeat: %d kinds", len(kinds)),
		Category:   Category,
		Attributes: attrs,
	}
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// objectStore is the reflector's store for one kind. It keeps no objects: every call goes
// to the Watcher, which compares hashes with the hash store.
type objectStore struct {
	w   *Watcher
	ctx context.Context
	gvr schema.GroupVersionResource

	mu sync.Mutex
	// replaced is set after the kind's first list reached Replace.
	replaced bool
}

var _ cache.Store = (*objectStore)(nil)

func (w *Watcher) newObjectStore(ctx context.Context, gvr schema.GroupVersionResource) *objectStore {
	return &objectStore{w: w, ctx: ctx, gvr: gvr}
}

func (s *objectStore) handle(op Op, obj any) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.WithFields(log.Fields{"gvr": GVRString(s.gvr), "type": fmt.Sprintf("%T", obj)}).Warn("objects: unexpected object type")
		return
	}
	if err := s.w.HandleObject(s.ctx, s.gvr, op, u.Object); err != nil {
		log.WithError(err).WithFields(log.Fields{"gvr": GVRString(s.gvr), "uid": string(u.GetUID()), "op": string(op)}).Warn("objects: handling object")
	}
}

// Add handles a watch ADDED event, as OpInitial while the first list is still running.
func (s *objectStore) Add(obj any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := OpAdded
	if !s.replaced {
		op = OpInitial
	}
	s.handle(op, obj)
	if isCRDResource(s.gvr) {
		s.w.triggerRediscovery()
	}
	return nil
}

// Update handles a watch MODIFIED event.
func (s *objectStore) Update(obj any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle(OpUpdated, obj)
	// A CRD update can add a version or become Established, which changes discovery.
	if isCRDResource(s.gvr) {
		s.w.triggerRediscovery()
	}
	return nil
}

// Delete handles a watch DELETED event. A DeletedFinalStateUnknown without an object is
// tombstoned from the stored identity of its namespace/name.
func (s *objectStore) Delete(obj any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if d.Obj == nil {
			s.tombstoneKey(d.Key)
			return nil
		}
		obj = d.Obj
	}
	s.handle(OpDeleted, obj)
	if u, ok := obj.(*unstructured.Unstructured); ok && isCRDResource(s.gvr) {
		group, plural := crdTarget(u.Object)
		s.w.forgetCRDObjects(s.ctx, group, plural)
		s.w.triggerRediscovery()
	}
	return nil
}

func (s *objectStore) tombstoneKey(key string) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		log.WithError(err).WithField("gvr", GVRString(s.gvr)).Warn("objects: unparseable delete key")
		return
	}
	if s.w.isOwnStoreName(s.gvr, namespace, name) {
		return
	}
	gvrString := GVRString(s.gvr)
	entries := s.w.store.entriesWhere(func(e Entry) bool {
		return e.GVR == gvrString && e.Namespace == namespace && e.Name == name
	})
	now := s.w.now()
	for uid, e := range entries {
		s.w.sendTombstone(s.ctx, uid, e, now)
	}
}

// Replace handles a full list: the first one as OpInitial, later relists as OpAdded for
// unknown uids and OpUpdated for known ones, so unchanged objects send nothing. Then every
// stored uid of this kind that the list lacks gets a tombstone. A list that failed never
// reaches Replace, so a kind that cannot be listed sends no deletes.
func (s *objectStore) Replace(items []any, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The carrier's own hash-store objects are left out of listed; they are never stored,
	// so Missing never names them either.
	listed := make(map[string]struct{}, len(items))
	for _, item := range items {
		if u, ok := item.(*unstructured.Unstructured); ok && !IsOwnStoreObject(s.w.cfg.Namespace, s.gvr, u.Object) {
			listed[string(u.GetUID())] = struct{}{}
		}
	}
	first := !s.replaced
	for _, item := range items {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		op := OpInitial
		if !first {
			op = OpAdded
			if u, ok := item.(*unstructured.Unstructured); ok {
				if _, known := s.w.store.Get(string(u.GetUID())); known {
					op = OpUpdated
				}
			}
		}
		s.handle(op, item)
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	s.tombstoneMissing(items, listed)
	s.replaced = true
	s.w.markListed(s.gvr)
	return nil
}

// tombstoneMissing sends a tombstone for every stored uid of the kind that a full list of
// items lacks; listed holds the uids the list has. Every such object went while unobserved,
// so the list is when the carrier observes the deletion. The caller holds s.mu.
func (s *objectStore) tombstoneMissing(items []any, listed map[string]struct{}) {
	// A CRD deleted and recreated while the carrier was down is missing by uid but present
	// by name. The recreated CRD serves the same group and plural, so the kind's own list
	// tombstones its missing objects; forgetting them here would also tombstone the live
	// objects the kind's watch has already stored.
	listedNames := map[string]struct{}{}
	if isCRDResource(s.gvr) {
		for _, item := range items {
			if u, ok := item.(*unstructured.Unstructured); ok {
				listedNames[u.GetName()] = struct{}{}
			}
		}
	}
	gvrString := GVRString(s.gvr)
	reconciled := s.w.now()
	for _, uid := range s.w.store.Missing(gvrString, listed, true) {
		e, ok := s.w.store.Get(uid)
		if !ok {
			continue
		}
		s.w.sendTombstone(s.ctx, uid, e, reconciled)
		if isCRDResource(s.gvr) {
			if _, recreated := listedNames[e.Name]; recreated {
				continue
			}
			group, plural := crdTargetFromName(e.Name)
			s.w.forgetCRDObjects(s.ctx, group, plural)
		}
	}
}

// The reflector reads nothing back from its store.
func (s *objectStore) List() []any                                { return nil }
func (s *objectStore) ListKeys() []string                         { return nil }
func (s *objectStore) Get(any) (item any, exists bool, err error) { return nil, false, nil }
func (s *objectStore) GetByKey(string) (any, bool, error)         { return nil, false, nil }
func (s *objectStore) LastStoreSyncResourceVersion() string       { return "" }
func (s *objectStore) Bookmark(string)                            {}
func (s *objectStore) Resync() error                              { return nil }
