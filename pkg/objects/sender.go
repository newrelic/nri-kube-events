package objects

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

const (
	// sendQueueBytes bounds the records waiting in the send queue, by their marshaled
	// size. A watch event that finds the queue full waits; it never sends from the watch.
	sendQueueBytes = 32 << 20
	// sendQueueItems bounds the object versions waiting in the send queue.
	sendQueueItems = 4096
	// sendTimeout bounds the send of one group, retries included. A group that fails is
	// marked unsynced and retried on the flush tick, so a slow forwarder holds up the
	// queue by at most this long per group.
	sendTimeout = 60 * time.Second
	// drainTimeout bounds the delivery of what is left in the queue when the watcher stops.
	drainTimeout = 10 * time.Second
	// decorationBytes estimates the bytes the sink adds to every record (decorate), so a
	// group stays within one forwarder POST.
	decorationBytes = 192
	// groupBytes is the largest summed size of one group, under MaxPostBytes.
	groupBytes = MaxPostBytes - envelopeReserve - 64<<10
	// rejectsAllWaitInitial and rejectsAllWaitMax bound the sender's wait after the
	// forwarder rejected a group and a heartbeat; the wait doubles while it keeps doing so.
	rejectsAllWaitInitial = 5 * time.Second
	rejectsAllWaitMax     = 5 * time.Minute
	// warnsPerSecond and warnBurst rate-limit the per-object warning logs, so a forwarder
	// outage on a large cluster does not flood the log.
	warnsPerSecond = 1
	warnBurst      = 20
)

// pendingSend is one object version or tombstone that waits in the send queue.
type pendingSend struct {
	observedAt time.Time
	// entry is stored after a successful send of an object version. For a tombstone it is
	// the identity a retried tombstone needs.
	entry Entry
	uid   string
	op    Op
	recs  []Record
	size  int
	seq   uint64
}

// pendingVersion is the latest version of an object that waits in the send queue.
type pendingVersion struct {
	hash    string
	seq     uint64
	deleted bool
}

// sender delivers the queued sends in order, grouping consecutive object versions into one
// forwarder POST. Records are built and queued under the kind's store lock, which orders
// them with the kind's watch events; the network send happens here, outside every lock.
type sender struct {
	queue chan pendingSend
	// stopping is closed when stopSender begins.
	stopping chan struct{}
	bytes    *semaphore.Weighted
	done     chan struct{}
	// cancel ends the delivery of the queued sends, after drainTimeout once the queue is
	// closed.
	cancel context.CancelFunc
	ctx    context.Context
}

// rateLimitedWarner logs warnings at most warnsPerSecond, and reports how many it dropped.
type rateLimitedWarner struct {
	limiter    *rate.Limiter
	suppressed atomic.Int64
}

func newWarner() *rateLimitedWarner {
	return &rateLimitedWarner{limiter: rate.NewLimiter(warnsPerSecond, warnBurst)}
}

func (r *rateLimitedWarner) warn(entry *log.Entry, msg string) {
	if !r.limiter.Allow() {
		r.suppressed.Add(1)
		return
	}
	if n := r.suppressed.Swap(0); n > 0 {
		entry = entry.WithField("suppressedWarnings", n)
	}
	entry.Warn(msg)
}

// startSender starts the goroutine that delivers queued sends. Until it runs, submit
// delivers each send at once in the caller's goroutine.
func (w *Watcher) startSender() {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sender{
		queue:    make(chan pendingSend, sendQueueItems),
		stopping: make(chan struct{}),
		bytes:    semaphore.NewWeighted(sendQueueBytes),
		done:     make(chan struct{}),
		cancel:   cancel,
		ctx:      ctx,
	}
	w.sendMu.Lock()
	w.sender = s
	w.sendMu.Unlock()
	go func() {
		defer close(s.done)
		w.runSender(s)
	}()
}

// stopSender closes the queue, lets the sender deliver what is left within drainTimeout,
// and waits for it. The caller has stopped every goroutine that submits.
func (w *Watcher) stopSender() {
	w.sendMu.Lock()
	s := w.sender
	w.sender = nil
	w.sendMu.Unlock()
	if s == nil {
		return
	}
	close(s.stopping)
	close(s.queue)
	timer := time.AfterFunc(drainTimeout, s.cancel)
	<-s.done
	timer.Stop()
	s.cancel()
}

// submit queues item. It records item as the object's pending version first, so a watch
// event with the same content that arrives before the send does not queue it again.
// Without a running sender it delivers item at once and returns the send's error. With
// one, it waits for room in the queue, and returns ctx's error if ctx ends first.
func (w *Watcher) submit(ctx context.Context, item pendingSend) error {
	w.mu.Lock()
	w.sendSeq++
	item.seq = w.sendSeq
	w.pending[item.uid] = pendingVersion{hash: item.entry.Hash, seq: item.seq, deleted: item.op == OpDeleted}
	w.mu.Unlock()

	w.sendMu.Lock()
	s := w.sender
	w.sendMu.Unlock()
	if s == nil {
		return w.deliver(ctx, []pendingSend{item})
	}
	if err := s.bytes.Acquire(ctx, min(int64(item.size), sendQueueBytes)); err != nil {
		w.clearPending(item)
		return err
	}
	select {
	case s.queue <- item:
		return nil
	case <-ctx.Done():
		s.bytes.Release(min(int64(item.size), sendQueueBytes))
		w.clearPending(item)
		return ctx.Err()
	}
}

// runSender delivers the queue in order until it is closed and empty. It groups the sends
// that are already queued, up to MaxEventsPerPost records and groupBytes, so one POST
// carries many objects.
func (w *Watcher) runSender(s *sender) {
	var next *pendingSend
	rejectWait := rejectsAllWaitInitial
	for {
		group, ok := s.nextGroup(&next)
		if !ok {
			return
		}
		err := w.deliver(s.ctx, group)
		for _, item := range group {
			s.bytes.Release(min(int64(item.size), sendQueueBytes))
		}
		// A forwarder that rejects everything is not asked again at once.
		if errors.Is(err, errForwarderRejectsAll) {
			s.wait(jittered(rejectWait))
			rejectWait = min(backoffFactor*rejectWait, rejectsAllWaitMax)
		} else if err == nil {
			rejectWait = rejectsAllWaitInitial
		}
	}
}

// nextGroup returns the next group: *next, or the next queued send, followed by the sends
// already queued, up to MaxEventsPerPost records and groupBytes. A send that does not fit
// is left in *next. It returns false once the queue is closed and empty.
func (s *sender) nextGroup(next **pendingSend) ([]pendingSend, bool) {
	var first pendingSend
	if *next != nil {
		first, *next = **next, nil
	} else {
		item, ok := <-s.queue
		if !ok {
			return nil, false
		}
		first = item
	}
	group := []pendingSend{first}
	records, size := len(first.recs), first.size
	for records < MaxEventsPerPost {
		select {
		case item, ok := <-s.queue:
			if !ok {
				return group, true
			}
			if records+len(item.recs) > MaxEventsPerPost || size+item.size > groupBytes {
				*next = &item
				return group, true
			}
			group = append(group, item)
			records += len(item.recs)
			size += item.size
		default:
			return group, true
		}
	}
	return group, true
}

// wait waits for d, or less when the sender is stopping, so a stop drains the queue left at
// once.
func (s *sender) wait(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.stopping:
	case <-s.ctx.Done():
	}
}

// errForwarderRejectsAll reports that the forwarder rejected a known-good heartbeat too,
// so a rejection says nothing about the objects: the endpoint or the forwarder is wrong.
var errForwarderRejectsAll = errors.New("the forwarder rejects every request")

// deliver sends the records of group in one call and applies the result to each send.
//
// When the forwarder rejects the group for its content, deliver first sends a heartbeat,
// whose content is known to be valid. If that is rejected too, the forwarder rejects
// everything (a wrong endpoint path, a forwarder without /v1/data, an auth change): every
// send of the group is marked unsynced, nothing is stored as sent, forwarder_rejects_all
// counts it, and deliver returns errForwarderRejectsAll so the sender backs off. If the
// heartbeat is accepted, the group is split in halves and each half sent again, so one
// object that is always rejected costs about log2(len(group)) extra sends, and the others
// in its group are delivered. That one object is dropped: counted in records_rejected_total,
// logged, and stored as sent, so it is not sent again until it changes.
//
// Any other failure marks every send of the group unsynced, for the retry pass. deliver
// returns the error of a group of one.
func (w *Watcher) deliver(ctx context.Context, group []pendingSend) error {
	return w.deliverGroup(ctx, group, false)
}

// deliverGroup is deliver; probed is set once the forwarder accepted the heartbeat probe.
func (w *Watcher) deliverGroup(ctx context.Context, group []pendingSend, probed bool) error {
	var recs []Record
	for _, item := range group {
		recs = append(recs, item.recs...)
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	err := w.sink.Send(sendCtx, recs)
	cancel()
	switch {
	case err == nil:
		for _, item := range group {
			w.delivered(item)
		}
		return nil
	case errors.Is(err, ErrRejected) && !probed:
		if probeErr := w.probeForwarder(ctx); probeErr != nil {
			err = probeErr
			break
		}
		return w.deliverGroup(ctx, group, true)
	case errors.Is(err, ErrRejected) && len(group) > 1:
		mid := len(group) / 2
		first := w.deliverGroup(ctx, group[:mid], true)
		return errors.Join(first, w.deliverGroup(ctx, group[mid:], true))
	case errors.Is(err, ErrRejected):
		item := group[0]
		metricRecordsRejected.Add(float64(len(item.recs)))
		w.warner.warn(log.WithError(err).WithFields(log.Fields{"gvr": item.entry.GVR, "uid": item.uid, "op": string(item.op)}),
			"objects: the forwarder rejected the object; dropping this version")
		w.delivered(item)
		return err
	}
	for _, item := range group {
		w.countFailedSend(ctx)
		w.markUnsynced(ctx, item.uid, unsyncedObject{entry: item.entry, op: item.op, observedAt: item.observedAt})
		w.clearPending(item)
	}
	if canceled(ctx) {
		return err
	}
	if errors.Is(err, errForwarderRejectsAll) {
		metricForwarderRejectsAll.Inc()
		w.warner.warn(log.WithError(err).WithFields(log.Fields{"objects": len(group), "endpoint": "agentEndpoint"}),
			"objects: the forwarder rejects every request, a heartbeat included; check the forwarder sidecar and objectState.agentEndpoint. The objects are retried")
		return err
	}
	w.warner.warn(log.WithError(err).WithField("objects", len(group)), "objects: sending to the forwarder failed; the objects are retried on the flush tick")
	return err
}

// probeForwarder sends one running heartbeat. It returns nil when the forwarder accepts it,
// errForwarderRejectsAll when the forwarder rejects it, and the send's error otherwise.
func (w *Watcher) probeForwarder(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, heartbeatSendTimeout)
	defer cancel()
	err := w.sink.Send(probeCtx, []Record{w.HeartbeatRecord(w.now())})
	if errors.Is(err, ErrRejected) {
		return fmt.Errorf("%w: %w", errForwarderRejectsAll, err)
	}
	return err
}

// delivered records that NRDB holds item: it stores the version, or forgets a tombstoned uid.
func (w *Watcher) delivered(item pendingSend) {
	w.markSynced(item.uid)
	if item.op == OpDeleted {
		w.store.Delete(item.uid)
	} else {
		w.store.Put(item.uid, item.entry)
	}
	w.clearPending(item)
}

// clearPending forgets item as its object's pending version, unless a later version of the
// object has been queued since.
func (w *Watcher) clearPending(item pendingSend) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p, ok := w.pending[item.uid]; ok && p.seq == item.seq {
		delete(w.pending, item.uid)
	}
}

// pendingOf returns the latest queued version of uid.
func (w *Watcher) pendingOf(uid string) (pendingVersion, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.pending[uid]
	return p, ok
}
