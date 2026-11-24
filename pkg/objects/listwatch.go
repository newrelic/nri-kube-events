package objects

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	// listPageSize is the most objects one page of a list holds. A kind is processed page
	// by page, so at most listConcurrency pages sit in memory at once, whatever the size
	// of the cluster.
	listPageSize = 100
	// secretListPageSize is the page size for core Secrets, whose objects can each be up
	// to 1 MiB (Helm keeps a release of up to that size per revision).
	secretListPageSize = 10
	// listConcurrency is the most kinds listed at once.
	listConcurrency = 2
	// listRetryInitial and listRetryMax bound the wait before a failed list or watch is
	// tried again; the wait doubles after each failure in a row.
	listRetryInitial = time.Second
	listRetryMax     = 2 * time.Minute
	// watchTimeoutMin is the shortest server-side timeout of one watch request. Each
	// request picks a timeout between it and twice it, so watches of all kinds do not
	// expire together.
	watchTimeoutMin = 5 * time.Minute
)

// errRelist reports that a watch can no longer resume from its resource version.
var errRelist = errors.New("watch expired; relisting")

// runKind lists gvr page by page, then watches it from the list's resource version,
// until ctx is canceled. A watch that expires leads to a new list; a list or watch that
// fails is tried again with backoff. A failed list is reported with listFailed, so the
// kind appears in carrier.failedKinds until a list succeeds.
func (w *Watcher) runKind(ctx context.Context, gvr schema.GroupVersionResource, s *objectStore) {
	wait := listRetryInitial
	for ctx.Err() == nil {
		rv, err := w.listKind(ctx, gvr, s)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.listFailed(gvr, err)
			sleepCtx(ctx, jittered(wait))
			wait = min(backoffFactor*wait, listRetryMax)
			continue
		}
		wait = listRetryInitial
		w.watchKind(ctx, gvr, s, rv)
	}
}

// listKind lists gvr in pages and hands each page to s, holding s.mu for the whole list so
// that a retry or resend of one of the kind's objects cannot interleave with it. It waits
// for one of listConcurrency slots first. It returns the resource version to watch from.
func (w *Watcher) listKind(ctx context.Context, gvr schema.GroupVersionResource, s *objectStore) (string, error) {
	select {
	case w.listSlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-w.listSlots }()

	s.mu.Lock()
	defer s.mu.Unlock()
	limit := int64(listPageSize)
	if gvr == secretsGVR {
		limit = secretListPageSize
	}
	l := s.beginList()
	opts := metav1.ListOptions{Limit: limit}
	for {
		list, err := w.dyn.Resource(gvr).List(ctx, opts)
		if err != nil {
			if apierrors.IsResourceExpired(err) && opts.Continue != "" {
				// The continue token expired mid-list; start the list over.
				l = s.beginList()
				opts.Continue = ""
				continue
			}
			return "", err
		}
		for i := range list.Items {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			s.listItem(l, &list.Items[i])
		}
		if list.GetContinue() == "" {
			s.finishList(l)
			return list.GetResourceVersion(), nil
		}
		opts.Continue = list.GetContinue()
	}
}

// watchKind watches gvr from rv and hands every event to s, until ctx is canceled or the
// watch can no longer resume, which ends in a new list.
func (w *Watcher) watchKind(ctx context.Context, gvr schema.GroupVersionResource, s *objectStore, rv string) {
	wait := listRetryInitial
	for ctx.Err() == nil {
		timeout := int64((watchTimeoutMin + time.Duration(rand.Int64N(int64(watchTimeoutMin)))) / time.Second) //nolint:gosec // jitter, not security
		wi, err := w.dyn.Resource(gvr).Watch(ctx, metav1.ListOptions{
			ResourceVersion:     rv,
			AllowWatchBookmarks: true,
			TimeoutSeconds:      &timeout,
		})
		if err != nil {
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || ctx.Err() != nil {
				return
			}
			w.warner.warn(log.WithError(err).WithField("gvr", GVRString(gvr)), "objects: watch failed; retrying")
			sleepCtx(ctx, jittered(wait))
			wait = min(backoffFactor*wait, listRetryMax)
			continue
		}
		wait = listRetryInitial
		next, err := s.consume(ctx, wi, rv)
		wi.Stop()
		if errors.Is(err, errRelist) {
			return
		}
		rv = next
	}
}

// consume hands the events of wi to s until wi ends, and returns the last resource version
// seen. It returns errRelist when the watch reports that rv is too old.
func (s *objectStore) consume(ctx context.Context, wi watch.Interface, rv string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return rv, ctx.Err()
		case ev, ok := <-wi.ResultChan():
			if !ok {
				return rv, nil
			}
			if ev.Type == watch.Error {
				err := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return rv, errRelist
				}
				return rv, err
			}
			u, isObject := ev.Object.(*unstructured.Unstructured)
			if !isObject {
				continue
			}
			if v := u.GetResourceVersion(); v != "" {
				rv = v
			}
			switch ev.Type {
			case watch.Added:
				_ = s.Add(u)
			case watch.Modified:
				_ = s.Update(u)
			case watch.Deleted:
				_ = s.Delete(u)
			case watch.Bookmark, watch.Error:
			}
		}
	}
}

// jittered returns d scaled by a random factor between 0.5 and 1.
func jittered(d time.Duration) time.Duration {
	half := d / backoffFactor
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
}

// sleepCtx waits for d or until ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
