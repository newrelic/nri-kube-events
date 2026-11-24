package objects

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Methods only the tests call.

// track records gvr as watched, with a store and a no-op cancel, without starting its
// list-watch loop. Tests drive the store directly.
func (w *Watcher) track(gvr schema.GroupVersionResource, kind string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.gvrs[gvr]; !ok {
		w.gvrs[gvr] = &gvrState{kind: kind, cancel: func() {}, store: w.newObjectStore(context.Background(), gvr)}
	}
}

// watching reports whether gvr is watched.
func (w *Watcher) watching(gvr schema.GroupVersionResource) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.gvrs[gvr]
	return ok
}

// isListed reports whether gvr is watched and its first list succeeded.
func (w *Watcher) isListed(gvr schema.GroupVersionResource) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.gvrs[gvr]
	return ok && st.listed
}

// payload is the SDK v3 JSON for one batch of recs.
func (s *InfraSink) payload(recs []Record) ([]byte, error) {
	return s.encode(s.decorate(recs))
}

// Replace handles items as one complete list of the kind, as listKind does page by page.
func (s *objectStore) Replace(items []any, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.beginList()
	for _, item := range items {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if u, ok := item.(*unstructured.Unstructured); ok {
			s.listItem(l, u)
		}
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	s.finishList(l)
	return nil
}

// ShardNames returns every name a shard ConfigMap can have.
func ShardNames() []string {
	gens := []int{1, 2} // one generation of each slot
	out := make([]string, 0, len(gens)*MaxShards)
	for _, gen := range gens {
		for i := 0; i < MaxShards; i++ {
			out = append(out, shardName(gen, i))
		}
	}
	return out
}
