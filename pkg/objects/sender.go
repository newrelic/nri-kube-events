package objects

import (
	"context"
	"errors"
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
	bytes *semaphore.Weighted
	done  chan struct{}
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
		queue:  make(chan pendingSend, sendQueueItems),
		bytes:  semaphore.NewWeighted(sendQueueBytes),
		done:   make(chan struct{}),
		cancel: cancel,
		ctx:    ctx,
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
	for {
		var first pendingSend
		if next != nil {
			first, next = *next, nil
		} else {
			item, ok := <-s.queue
			if !ok {
				return
			}
			first = item
		}
		group := []pendingSend{first}
		records, size := len(first.recs), first.size
	fill:
		for records < MaxEventsPerPost {
			select {
			case item, ok := <-s.queue:
				if !ok {
					break fill
				}
				if records+len(item.recs) > MaxEventsPerPost || size+item.size > groupBytes {
					next = &item
					break fill
				}
				group = append(group, item)
				records += len(item.recs)
				size += item.size
			default:
				break fill
			}
		}
		_ = w.deliver(s.ctx, group)
		for _, item := range group {
			s.bytes.Release(min(int64(item.size), sendQueueBytes))
		}
	}
}

// deliver sends the records of group in one call and applies the result to each send. A
// group the forwarder rejects for its content is split in halves and each half sent again,
// so one object that is always rejected costs about log2(len(group)) extra sends, and the
// others in its group are delivered. That one object is dropped: counted in
// records_rejected_total, logged, and stored as sent, so it is not sent again until it
// changes. Any other failure marks every send of the group unsynced, for the retry pass. It
// returns the error of a group of one.
func (w *Watcher) deliver(ctx context.Context, group []pendingSend) error {
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
	case errors.Is(err, ErrRejected) && len(group) > 1:
		mid := len(group) / 2
		first := w.deliver(ctx, group[:mid])
		return errors.Join(first, w.deliver(ctx, group[mid:]))
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
	if !canceled(ctx) {
		w.warner.warn(log.WithError(err).WithField("objects", len(group)), "objects: sending to the forwarder failed; the objects are retried on the flush tick")
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
