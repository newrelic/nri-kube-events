package objects

import (
	"context"
	"hash/fnv"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// resendSpread is the number of resend scans in one resendAfter at the default interval of
// one minute and resendAfter of 24 hours. One scan resends at most 1/resendSpread of the
// store, and a first send's LastSent falls in one of resendSpread slots of resendAfter.
const resendSpread = 1440

// resendGetTimeout bounds the GET of one resend. The resend holds its kind's reflector store
// lock across the GET, so a GET that hangs would stall the kind's watch and every later scan.
const resendGetTimeout = 30 * time.Second

// uidOrder is the position of uid in the resend order: the FNV-1a 64 hash of the uid.
func uidOrder(uid string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))
	return h.Sum64()
}

// firstSentAt is the LastSent stored for the first-ever send of uid at now. It lies up to
// resendAfter before now, by uidOrder, so the objects of one startup come due for their first
// resend spread across a resendAfter, and not all at once.
func firstSentAt(uid string, now time.Time, resendAfter time.Duration) int64 {
	return now.Unix() - int64(resendAfter.Seconds()*float64(uidOrder(uid)%resendSpread)/resendSpread)
}

// catchUpScans is the number of resend scans over which a backlog of overdue objects is
// drained: about an hour at the default resend interval of one minute.
const catchUpScans = 60

// resendBudget is the steady part of one resend scan's budget for a store of entries: enough
// that every object is resent once per resendSpread scans.
func resendBudget(entries int) int {
	return ceilDiv(entries, resendSpread)
}

func ceilDiv(n, d int) int {
	return (n + d - 1) / d
}

// scanBudget returns the most objects this scan sends when due objects are overdue in a store
// of entries, and updates the catch-up rate. The steady part, resendBudget(entries), only
// keeps pace with the objects that come due each scan. When more are due, as after an outage
// of the carrier, the forwarder or the account flag, the scan adds a catch-up part of
// ceil(due/catchUpScans), the largest seen since the backlog started. Holding the rate at its
// largest value drains the backlog in about catchUpScans scans; a rate recomputed from the
// shrinking backlog would only halve it every 41 scans. The rate resets once a scan finds no
// more due objects than the steady part. MaxResendsPerScan caps the result. The caller holds
// w.mu.
func (w *Watcher) scanBudget(entries, due int) int {
	steady := resendBudget(entries)
	if due <= steady {
		w.catchUp = 0
		return due
	}
	w.catchUp = max(w.catchUp, ceilDiv(due, catchUpScans))
	budget := steady + w.catchUp
	if w.cfg.MaxResendsPerScan > 0 {
		budget = min(budget, w.cfg.MaxResendsPerScan)
	}
	return min(budget, due)
}

// isDue reports whether e was last sent at least resendAfter before now. With the resend off
// nothing is due.
func (w *Watcher) isDue(e Entry, now time.Time) bool {
	after := int64(w.cfg.ResendAfter / time.Second)
	return w.cfg.ResendAfter > 0 && now.Unix()-e.LastSent >= after
}

// dueForResend returns the uids this resend scan sends: the due entries of kinds that are
// watched and listed, leaving out the carrier's own store objects and the uids whose last send
// failed, in uidOrder. A failed send is the retry pass's to repeat: a failed tombstone keeps
// its observed deletion time there, which a resend would replace with a later one. The scan
// starts at the first order above resyncCursor, wraps past the highest order, and takes at
// most scanBudget uids. It moves resyncCursor to the order of the last uid taken.
func (w *Watcher) dueForResend(now time.Time) []string {
	if w.cfg.ResendAfter <= 0 {
		return nil
	}
	w.mu.Lock()
	listed := map[string]bool{}
	for gvr, st := range w.gvrs {
		if st.cancel != nil && st.listed {
			listed[GVRString(gvr)] = true
		}
	}
	unsynced := make(map[string]struct{}, len(w.unsynced))
	for uid := range w.unsynced {
		unsynced[uid] = struct{}{}
	}
	w.mu.Unlock()

	entries := w.store.entriesWhere(func(e Entry) bool {
		if !listed[e.GVR] || !w.isDue(e, now) {
			return false
		}
		gvr, err := ParseGVR(e.GVR)
		return err == nil && !w.isOwnStoreName(gvr, e.Namespace, e.Name)
	})
	type ordered struct {
		uid   string
		order uint64
	}
	due := make([]ordered, 0, len(entries))
	for uid := range entries {
		if _, ok := unsynced[uid]; ok {
			continue
		}
		due = append(due, ordered{uid: uid, order: uidOrder(uid)})
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].order != due[j].order {
			return due[i].order < due[j].order
		}
		return due[i].uid < due[j].uid
	})
	entriesLen := w.store.Len()
	w.mu.Lock()
	defer w.mu.Unlock()
	budget := w.scanBudget(entriesLen, len(due))
	if budget == 0 {
		return nil
	}
	start := sort.Search(len(due), func(i int) bool { return due[i].order > w.resyncCursor })
	out := make([]string, 0, budget)
	for i := 0; i < budget; i++ {
		out = append(out, due[(start+i)%len(due)].uid)
	}
	w.resyncCursor = due[(start+budget-1)%len(due)].order
	return out
}

// startResend runs a resend scan in its own goroutine, unless the previous one still runs.
func (w *Watcher) startResend(ctx context.Context) {
	if !w.resendBusy.CompareAndSwap(false, true) {
		log.Debug("objects: previous resend scan still running, skipping this one")
		return
	}
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		defer w.resendBusy.Store(false)
		for _, uid := range w.dueForResend(w.now()) {
			if ctx.Err() != nil {
				return
			}
			w.resendOne(ctx, uid)
		}
	}()
}

// resendOne sends uid again as OpResync from the object as the apiserver serves it now. When
// the apiserver no longer serves it, or serves a new object of the same name, it sends a
// tombstone for uid. Like retryOne, it holds the reflector store lock of the object's kind
// across its GET and its send, so a watch event that lands in between cannot be overwritten
// by the older version the resend fetched. A watch event handled since the scan picked uid
// has already sent the object, and then resendOne sends nothing. After a failed GET or send
// the stored entry is unchanged, so the object stays due for the next scan.
func (w *Watcher) resendOne(ctx context.Context, uid string) {
	e, ok := w.store.Get(uid)
	if !ok {
		return
	}
	gvr, err := ParseGVR(e.GVR)
	if err != nil {
		return
	}
	s := w.reflectorStore(gvr)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok = w.store.Get(uid); !ok || !w.isDue(e, w.now()) || w.isUnsynced(uid) {
		return
	}
	var res dynamic.ResourceInterface = w.dyn.Resource(gvr)
	if e.Namespace != "" {
		res = w.dyn.Resource(gvr).Namespace(e.Namespace)
	}
	getCtx, cancel := context.WithTimeout(ctx, w.resendGetTimeout)
	obj, err := res.Get(getCtx, e.Name, metav1.GetOptions{})
	cancel()
	// A resend that finds the object gone, or its name taken by a new object, is when the
	// carrier observes the deletion.
	switch {
	case apierrors.IsNotFound(err):
		w.sendTombstone(ctx, uid, e, w.now())
	case err != nil:
		log.WithError(err).WithFields(log.Fields{"gvr": e.GVR, "uid": uid}).Warn("objects: fetching object to resend")
	case string(obj.GetUID()) != uid:
		// The name now belongs to a new object, which its own watch sends.
		w.sendTombstone(ctx, uid, e, w.now())
	default:
		if err := w.handle(ctx, gvr, OpResync, obj.Object, true); err != nil {
			log.WithError(err).WithFields(log.Fields{"gvr": e.GVR, "uid": uid}).Warn("objects: resending object")
			return
		}
		metricResends.Inc()
	}
}

// isUnsynced reports whether the last send of uid failed and has not succeeded since.
func (w *Watcher) isUnsynced(uid string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.unsynced[uid]
	return ok
}
