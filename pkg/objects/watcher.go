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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
)

const (
	DefaultDiscoveryInterval = 5 * time.Minute
	DefaultHeartbeatInterval = 60 * time.Second
	DefaultFlushInterval     = 60 * time.Second
	// flushBudgetBytes bounds the hash store bytes one flush tick writes. With
	// DefaultFlushInterval it caps the writes to etcd at about 1.4 GB a day, whatever the
	// number of objects; on a 200,000-object store (about 44 MB in 49 shards) every
	// changed shard is written within about 45 minutes. A shard written late only means
	// that a restart in between sends again the objects that changed in it, never that a
	// change is lost: the list after the restart compares every object with the stored
	// hash. Shutdown flushes everything.
	flushBudgetBytes      = 1_000_000
	DefaultResendInterval = time.Minute

	// discoveryRetry is how soon a discovery that failed outright is tried again.
	discoveryRetry = 10 * time.Second
	// shutdownFlushTimeout bounds the store flush after the run context is canceled.
	shutdownFlushTimeout = 30 * time.Second
	// heartbeatSendTimeout bounds one heartbeat send.
	heartbeatSendTimeout = 30 * time.Second
	// finalHeartbeatTimeout bounds the send of the last heartbeat of a run, so a forwarder
	// that does not answer delays the stop by at most this long.
	finalHeartbeatTimeout = 5 * time.Second

	// CarrierStateRunning and CarrierStateStopped are the heartbeat's carrier.state. Every
	// heartbeat of a running watcher says running. The last heartbeat of a run says
	// stopped; a run ends only when its context is canceled, because the account flag
	// turned off or the process is shutting down.
	CarrierStateRunning = "running"
	CarrierStateStopped = "stopped"

	crdGroup    = "apiextensions.k8s.io"
	crdResource = "customresourcedefinitions"
)

// Config configures a Watcher. A zero interval takes its default.
type Config struct {
	// AccountFlag reports the account collection flag for the heartbeat's
	// carrier.accountFlag. Nil reports unknown.
	AccountFlag func() string
	// Coverage, when set, records the resources this watcher collects, for the describe
	// path to skip. The watcher clears it when it stops.
	Coverage *Coverage

	ClusterName, Namespace                                              string
	DiscoveryInterval, HeartbeatInterval, FlushInterval, ResendInterval time.Duration
	// ResendAfter is the age of the last send after which an unchanged object is sent
	// again. Zero turns the resend off; it takes no default.
	ResendAfter time.Duration
	// MaxResendsPerScan caps the objects one resend scan sends while it drains a backlog.
	// Zero means no cap.
	MaxResendsPerScan int
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
	// cancel stops the kind's list-watch loop.
	cancel context.CancelFunc
	// store receives the kind's list pages and watch events. Its mu orders them, and a
	// retry of one of the kind's objects takes it too.
	store *objectStore
	kind  string
	// listed is set when the kind's first list reached the store.
	listed bool
	// resolved is set when the kind's first list succeeded or failed.
	resolved bool
	// listFailing is set while the kind's most recent list failed.
	listFailing bool
}

// Watcher discovers every listable and watchable kind, lists and watches each, and
// sends a record whenever an object's hash changes, a tombstone when an object goes, and a
// heartbeat on a timer. It keeps no objects: a list is processed page by page, and the
// Store keeps one Entry per uid.
type Watcher struct {
	dyn   dynamic.Interface
	disc  discovery.DiscoveryInterface
	sink  RecordSink
	store *Store
	now   func() time.Time

	// rediscover asks Run for a discovery pass. It holds at most one pending request.
	rediscover chan struct{}
	// listSlots holds one token per kind being listed, at most listConcurrency.
	listSlots chan struct{}
	// afterRetryGet, when set, is called by a retry after it fetched an object and before
	// it sends it. Tests use it to deliver a watch event at that point.
	afterRetryGet func(uid string)
	// pending, guarded by mu, holds the latest queued version of every uid with a send in
	// the queue.
	pending map[string]pendingVersion
	// sender delivers the queued sends while Run runs; sendMu guards it.
	sender *sender
	// lastHeartbeat, guarded by mu, is the last heartbeat built; the forwarder probe resends it.
	lastHeartbeat *Record
	warner        *rateLimitedWarner

	// mu guards gvrs, unsynced, startTime, lastFullList, failedSends, resyncCursor,
	// catchUp and stopped.
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
	// getTimeout bounds the GET of one resend or retry; NewWatcher sets objectGetTimeout.
	getTimeout time.Duration
	// finalTimeout bounds sendStoppedHeartbeat; NewWatcher sets finalHeartbeatTimeout.
	finalTimeout time.Duration

	mu     sync.Mutex
	sendMu sync.Mutex
	// sendSeq, guarded by mu, numbers the queued sends.
	sendSeq     uint64
	failedSends int64
	// resyncCursor is where the next resend scan starts; the heartbeat reports it.
	resyncCursor uint64
	// catchUp is the catch-up part of the resend budget while a backlog drains; see
	// scanBudget.
	catchUp int

	// reflectors counts running list-watch loops, so shutdown can wait for them.
	reflectors sync.WaitGroup
	// background counts the heartbeat, retry and resend goroutines, so shutdown can wait for
	// them.
	background sync.WaitGroup
	// heartbeatBusy, retryBusy and resendBusy are set while a heartbeat send, a retry pass
	// or a resend scan runs.
	heartbeatBusy, retryBusy, resendBusy atomic.Bool
	// stopped, guarded by mu, is set when shutdown begins. A list that completes after it
	// no longer marks its resource covered.
	stopped bool
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
		cfg:          cfg.withDefaults(),
		dyn:          dyn,
		disc:         disc,
		store:        store,
		sink:         sink,
		key:          hmacKey,
		now:          time.Now,
		getTimeout:   objectGetTimeout,
		finalTimeout: finalHeartbeatTimeout,
		rediscover:   make(chan struct{}, 1),
		listSlots:    make(chan struct{}, listConcurrency),
		gvrs:         map[schema.GroupVersionResource]*gvrState{},
		unsynced:     map[string]unsyncedObject{},
		pending:      map[string]pendingVersion{},
		warner:       newWarner(),
	}
}

// Run starts a watch for every discovered kind, and runs discovery, heartbeat and flush on
// their timers until ctx is canceled. It then stops the watches and flushes the store once
// more, and returns that flush's error. Run never loads the store: main loads it once, with
// a startup deadline, and exits if that fails. Heartbeats and retries of failed sends run
// in their own goroutines, as do the resend scans of unchanged objects while ResendAfter is
// set, so the discovery and flush ticks never wait on the forwarder.
func (w *Watcher) Run(ctx context.Context) error {
	w.dropOwnStoreEntries()
	w.mu.Lock()
	w.startTime = w.now()
	w.mu.Unlock()
	w.startSender()

	// The first heartbeat goes out before discovery, so a reader sees the new run at once.
	w.startHeartbeat(ctx)
	w.syncWatches(ctx)

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
			if err := w.store.FlushBudget(ctx, flushBudgetBytes); err != nil {
				log.WithError(err).Warn("objects: flushing hash store")
			}
		case <-resendC:
			w.startResend(ctx)
		}
	}
}

// shutdown clears the coverage, stops every watch, waits for the list-watch loops and the
// background sends to return, sends the stopped heartbeat, and flushes the store. The
// coverage is cleared first, so the describe path resumes while the watches still run, and
// every change is sent by at least one of the two. The stopped heartbeat goes after every
// running one of this run has returned, so it is the run's last heartbeat.
func (w *Watcher) shutdown() error {
	w.mu.Lock()
	w.stopped = true
	w.cfg.Coverage.clear()
	for _, st := range w.gvrs {
		st.cancel()
	}
	w.mu.Unlock()
	w.reflectors.Wait()
	w.background.Wait()
	w.stopSender()
	w.sendStoppedHeartbeat()

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

// discover returns every resource with the list and watch verbs, in the version its group
// prefers, mapped to its kind. Subresources and both events resources are left out. When
// some groups fail discovery, it returns the others and names the failed groups.
func (w *Watcher) discover() (found map[schema.GroupVersionResource]string, failedGroups map[string]struct{}, err error) {
	disc := w.disc
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
		st.cancel()
		if !st.resolved {
			removedPending = true
		}
		delete(w.gvrs, gvr)
		w.cfg.Coverage.set(gvr.GroupResource(), false)
	}
	if removedPending && len(w.gvrs) > 0 && w.allResolvedLocked() {
		w.lastFullList = w.now()
	}
}

// startLocked starts the list-watch loop of gvr (runKind). The caller holds w.mu.
func (w *Watcher) startLocked(ctx context.Context, gvr schema.GroupVersionResource, kind string) {
	wctx, cancel := context.WithCancel(ctx)
	store := w.newObjectStore(wctx, gvr)
	w.gvrs[gvr] = &gvrState{kind: kind, cancel: cancel, store: store}
	log.WithFields(log.Fields{"gvr": GVRString(gvr), "kind": kind}).Debug("objects: starting watch")
	w.reflectors.Add(1)
	go func() {
		defer w.reflectors.Done()
		w.runKind(wctx, gvr, store)
	}()
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
	w.cfg.Coverage.set(gvr.GroupResource(), false)
	if !st.resolved && err != nil {
		log.WithError(err).WithField("gvr", GVRString(gvr)).Warn("objects: list failed; the kind sends no deletes until a list succeeds")
	}
	w.resolveLocked(st)
}

// markListed marks gvr as listed: its kind joins the heartbeat's kinds, and, until the
// watcher stops, its resource is covered. finishList calls it after every object of the list
// was sent or marked unsynced.
func (w *Watcher) markListed(gvr schema.GroupVersionResource) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	if !ok {
		return
	}
	st.listed = true
	st.listFailing = false
	if !w.stopped {
		w.cfg.Coverage.set(gvr.GroupResource(), true)
	}
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
// the store lock of the object's kind across its GET and until its send is queued, the same
// lock that orders the kind's watch events. The queue delivers in order, so a watch event
// that lands after the GET is sent after the retry, and cannot be overwritten: a newer
// version by the older one the retry fetched, or a delete by a live version that puts the
// uid back in the store.
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
		if s := w.kindStore(gvr); s != nil {
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
	getCtx, cancel := context.WithTimeout(ctx, w.getTimeout)
	// A quorum read, not one from the watch cache (resourceVersion "0"): a cached read can
	// return a version older than one already sent, and this send would then give it a
	// newer timestamp. The reads are few, at most Config.MaxResendsPerScan or maxRetryPerPass a pass.
	obj, err := res.Get(getCtx, u.entry.Name, metav1.GetOptions{})
	cancel()
	if w.afterRetryGet != nil {
		w.afterRetryGet(uid)
	}
	// A retry that finds the object gone, or its name taken by a new object, is when the
	// carrier observes the deletion.
	switch {
	case apierrors.IsNotFound(err):
		w.sendTombstone(ctx, uid, w.tombstoneEntry(uid, u.entry), w.now())
	case err != nil:
		w.warner.warn(log.WithError(err).WithFields(log.Fields{"gvr": u.entry.GVR, "uid": uid}), "objects: fetching unsynced object")
	case string(obj.GetUID()) != uid:
		// The name now belongs to a new object, which its own watch sends.
		w.sendTombstone(ctx, uid, w.tombstoneEntry(uid, u.entry), w.now())
	default:
		if err := w.HandleObject(ctx, gvr, u.op, obj.Object); err != nil {
			w.warner.warn(log.WithError(err).WithFields(log.Fields{"gvr": u.entry.GVR, "uid": uid}), "objects: retrying unsynced object")
		}
	}
}

// kindStore returns the store of gvr's list-watch loop, or nil when gvr is not watched.
func (w *Watcher) kindStore(gvr schema.GroupVersionResource) *objectStore {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, ok := w.gvrs[gvr]; ok {
		return st.store
	}
	return nil
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
	red, hash := prepareObject(gvr, w.kindOf(gvr), obj, w.key)
	meta, _ := red["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	if uid == "" {
		return fmt.Errorf("%s object has no metadata.uid", GVRString(gvr))
	}
	gvrString := GVRString(gvr)
	kind, _ := red["kind"].(string)
	namespace, _ := meta["namespace"].(string)
	name, _ := meta["name"].(string)

	if op != OpDeleted && !force && w.alreadySent(uid, hash, gvrString) {
		return nil
	}

	now := w.now()
	recs, err := BuildRecords(red, gvr, op, w.cfg.ClusterName, now)
	if err != nil {
		return fmt.Errorf("building records for %s %s: %w", gvrString, uid, err)
	}
	item := pendingSend{uid: uid, op: op, recs: recs, size: queuedSize(recs)}
	if op == OpDeleted {
		item.entry = w.tombstoneEntry(uid, Entry{GVR: gvrString, Kind: kind, Namespace: namespace, Name: name})
		item.observedAt = now
	} else {
		// A first-ever send stores a LastSent spread across ResendAfter, so the objects of
		// one startup come due for the resend spread out and not all at once.
		lastSent := now.Unix()
		if _, known := w.store.Get(uid); !known && w.cfg.ResendAfter > 0 {
			lastSent = firstSentAt(uid, now, w.cfg.ResendAfter)
		}
		item.entry = Entry{GVR: gvrString, Kind: kind, Namespace: namespace, Name: name, Hash: hash, LastSent: lastSent}
	}
	if err := w.submit(ctx, item); err != nil {
		return fmt.Errorf("sending %s %s: %w", gvrString, uid, err)
	}
	return nil
}

// alreadySent reports whether the version of uid with hash needs no send: the queue holds
// it, or, with nothing queued for uid, the store does, which means NRDB holds it. A stored
// entry under another GVR string takes gvrString.
func (w *Watcher) alreadySent(uid, hash, gvrString string) bool {
	if p, queued := w.pendingOf(uid); queued {
		return !p.deleted && p.hash == hash
	}
	old, ok := w.store.Get(uid)
	if !ok || old.Hash != hash {
		return false
	}
	if old.GVR != gvrString {
		old.GVR = gvrString
		w.store.Put(uid, old)
	}
	w.markSynced(uid)
	return true
}

// queuedSize is the size recs take in the send queue and in a forwarder POST.
func queuedSize(recs []Record) int {
	n := 0
	for _, r := range recs {
		n += recordSize(r) + decorationBytes
	}
	return n
}

// prepareObject returns obj as the watcher sends it, and its hash: apiVersion and kind
// filled in where obj lacks them (kind is the kind discovery reports for gvr, and may be
// empty), then redacted. Every object of core v1 secrets is redacted as a Secret, because
// a watch object can lack its own kind field. obj itself is never modified.
func prepareObject(gvr schema.GroupVersionResource, kind string, obj map[string]any, hmacKey []byte) (redacted map[string]any, hash string) {
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

// sendTombstone queues the delete record of uid from its stored identity, with observedAt,
// when the carrier saw the deletion, as its timestamp. Once sent, uid is forgotten. A failed
// tombstone marks uid unsynced, to be sent again as a tombstone with the same observedAt.
func (w *Watcher) sendTombstone(ctx context.Context, uid string, e Entry, observedAt time.Time) {
	recs := []Record{Tombstone(uid, e, w.cfg.ClusterName, observedAt)}
	item := pendingSend{uid: uid, op: OpDeleted, recs: recs, size: queuedSize(recs), entry: e, observedAt: observedAt}
	// deliver logs a failed send and marks uid unsynced.
	_ = w.submit(ctx, item)
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

// sendStoppedHeartbeat sends the last heartbeat of a run, with carrier.state set to
// CarrierStateStopped and carrier.stoppedAt to now. It runs within its own finalHeartbeatTimeout, never within the
// run's context, which is already canceled. A failed send is logged and counted in
// failedSends and nri_kube_events_objects_send_failures_total, and returns; it never
// blocks the stop past the timeout.
func (w *Watcher) sendStoppedHeartbeat() {
	state := CarrierStateStopped
	ctx, cancel := context.WithTimeout(context.Background(), w.finalTimeout)
	defer cancel()
	now := w.now()
	if err := w.sink.Send(ctx, []Record{w.heartbeatRecord(now, state, now)}); err != nil {
		w.countFailedSend(ctx)
		log.WithError(err).WithField("state", state).Warn("objects: sending the final heartbeat")
		return
	}
	log.WithField("state", state).Info("objects: sent the final heartbeat")
}

// HeartbeatRecord builds the carrier heartbeat and sets the objects gauge from the same
// counts. carrier.kinds lists, as sorted "Kind.group=count," pairs cut by Split into
// carrier.kinds.pNNN parts, every watched kind whose list has succeeded, with the count of
// its objects in the store; a kind with no objects is listed with 0. A core kind has no
// group suffix, so Pod is "Pod" and Cluster-API's Cluster is "Cluster.cluster.x-k8s.io"
// (see qualifiedKind); two groups that serve the same Kind name stay apart.
// carrier.failedKinds lists, sorted and comma-separated, the GVRs whose most recent list
// failed; capList drops whole GVRs from the end to keep it within one attribute. A kind
// whose first list is still running is in neither, and a reader should treat it as not
// collected yet. carrier.state is CarrierStateRunning and carrier.stoppedAt is 0.
// timestamp is Unix seconds. The other times are Unix milliseconds, 0 before they happen.
func (w *Watcher) HeartbeatRecord(now time.Time) Record {
	return w.heartbeatRecord(now, CarrierStateRunning, time.Time{})
}

// heartbeatRecord is HeartbeatRecord with carrier.state set to state and carrier.stoppedAt
// to stoppedAt, which is zero for a running heartbeat.
func (w *Watcher) heartbeatRecord(now time.Time, state string, stoppedAt time.Time) Record {
	w.mu.Lock()
	kindOfGVR := map[string]string{}
	var failedGVRs []string
	for gvr, st := range w.gvrs {
		if st.listed {
			kindOfGVR[GVRString(gvr)] = qualifiedKind(gvr, st.kind)
		}
		if st.listFailing {
			failedGVRs = append(failedGVRs, GVRString(gvr))
		}
	}
	start, full, failed, unsynced, cursor := w.startTime, w.lastFullList, w.failedSends, len(w.unsynced), w.resyncCursor
	w.mu.Unlock()
	sort.Strings(failedGVRs)
	failedList, _ := capList(failedGVRs)

	accountFlag := accountflag.Unknown.String()
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
		// carrier.resendAfter is the resend interval of unchanged objects in whole seconds;
		// 0 means the resend is off. A reader uses it to judge whether a read window is
		// long enough to hold a record of every object.
		"carrier.resendAfter": int64(w.cfg.ResendAfter / time.Second),
		// carrier.unsyncedObjects is the number of objects whose last send failed and has
		// not succeeded since; 0 means NRDB holds the version the carrier last saw of
		// every object.
		"carrier.unsyncedObjects": unsynced,
		// carrier.state is running on every heartbeat of a running watcher, and stopped or
		// failed on the last one; carrier.stoppedAt is when that last one was built.
		"carrier.state":     state,
		"carrier.stoppedAt": unixMilli(stoppedAt),
	}
	for i, p := range Split(b.String()) {
		attrs[fmt.Sprintf("carrier.kinds.p%03d", i)] = p
	}
	rec := Record{
		Summary:    fmt.Sprintf("carrier heartbeat: %d kinds", len(kinds)),
		Category:   Category,
		Attributes: attrs,
	}
	w.mu.Lock()
	w.lastHeartbeat = &rec
	w.mu.Unlock()
	return rec
}

// probeRecord returns the last heartbeat built, unchanged, timestamp included, so a probe
// costs no scan of the store and a reader that takes the newest heartbeat is not misled.
// Before the first heartbeat it builds one.
func (w *Watcher) probeRecord() Record {
	w.mu.Lock()
	last := w.lastHeartbeat
	w.mu.Unlock()
	if last != nil {
		return *last
	}
	return w.HeartbeatRecord(w.now())
}

// qualifiedKind returns kind followed by "." and gvr's group, or kind alone for the core
// group. The heartbeat's carrier.kinds and the objects gauge's kind label use it.
func qualifiedKind(gvr schema.GroupVersionResource, kind string) string {
	if gvr.Group == "" {
		return kind
	}
	return kind + "." + gvr.Group
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// objectStore receives the list pages and watch events of one kind. It keeps no objects: every call goes
// to the Watcher, which compares hashes with the hash store.
type objectStore struct {
	w   *Watcher
	ctx context.Context
	gvr schema.GroupVersionResource

	mu sync.Mutex
	// replaced is set after the kind's first list finished (finishList).
	replaced bool
}

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
		s.w.warner.warn(log.WithError(err).WithFields(log.Fields{"gvr": GVRString(s.gvr), "uid": string(u.GetUID()), "op": string(op)}), "objects: handling object")
	}
}

// Add handles a watch ADDED event. A watch starts only after the kind's first list, so its
// events are never part of the initial state.
func (s *objectStore) Add(obj any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle(OpAdded, obj)
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

// Delete handles a watch DELETED event.
func (s *objectStore) Delete(obj any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle(OpDeleted, obj)
	if u, ok := obj.(*unstructured.Unstructured); ok && isCRDResource(s.gvr) {
		group, plural := crdTarget(u.Object)
		s.w.forgetCRDObjects(s.ctx, group, plural)
		s.w.triggerRediscovery()
	}
	return nil
}

// listing is the state of one list of a kind while its pages arrive.
type listing struct {
	// listed holds the uids of the list; the carrier's own hash-store objects are left out,
	// since they are never stored and Missing never names them.
	listed map[string]struct{}
	// names holds the names of the listed objects, kept only for the CRD kind.
	names map[string]struct{}
	first bool
}

// beginList starts a list of the kind. The caller holds s.mu until finishList.
func (s *objectStore) beginList() *listing {
	return &listing{listed: map[string]struct{}{}, names: map[string]struct{}{}, first: !s.replaced}
}

// listItem handles one listed object: as OpInitial in the kind's first list, and in a
// later list as OpAdded for an unknown uid and OpUpdated for a known one, so an unchanged
// object sends nothing. The caller holds s.mu.
func (s *objectStore) listItem(l *listing, u *unstructured.Unstructured) {
	uid := string(u.GetUID())
	if !IsOwnStoreObject(s.w.cfg.Namespace, s.gvr, u.Object) {
		l.listed[uid] = struct{}{}
	}
	if isCRDResource(s.gvr) {
		l.names[u.GetName()] = struct{}{}
	}
	op := OpInitial
	if !l.first {
		op = OpAdded
		if _, known := s.w.store.Get(uid); known {
			op = OpUpdated
		}
	}
	s.handle(op, u)
}

// finishList ends a list that delivered every page: every stored uid of the kind that the
// list lacks gets a tombstone, and the kind counts as listed. A list that failed never gets
// here, so a kind that cannot be listed sends no deletes. The caller holds s.mu.
func (s *objectStore) finishList(l *listing) {
	s.tombstoneMissing(l)
	s.replaced = true
	s.w.markListed(s.gvr)
}

// tombstoneMissing sends a tombstone for every stored uid of the kind that the list l
// lacks. Every such object went while unobserved, so the list is when the carrier observes
// the deletion. The caller holds s.mu.
func (s *objectStore) tombstoneMissing(l *listing) {
	// A CRD deleted and recreated while the carrier was down is missing by uid but present
	// by name. The recreated CRD serves the same group and plural, so the kind's own list
	// tombstones its missing objects; forgetting them here would also tombstone the live
	// objects the kind's watch has already stored.
	gvrString := GVRString(s.gvr)
	reconciled := s.w.now()
	for _, uid := range s.w.store.Missing(gvrString, l.listed) {
		e, ok := s.w.store.Get(uid)
		if !ok {
			continue
		}
		s.w.sendTombstone(s.ctx, uid, e, reconciled)
		if isCRDResource(s.gvr) {
			if _, recreated := l.names[e.Name]; recreated {
				continue
			}
			group, plural := crdTargetFromName(e.Name)
			s.w.forgetCRDObjects(s.ctx, group, plural)
		}
	}
}
