package objects

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Coverage records the resources a running Watcher collects. A resource is covered from the
// moment its first list has been sent, until its list fails, it leaves discovery, or the
// Watcher stops. The describe path reads it to skip the descriptions of covered resources.
// A nil *Coverage covers nothing and ignores writes.
type Coverage struct {
	covered map[schema.GroupResource]struct{}
	// changed receives after a change. Changes may collapse into one receive.
	changed chan struct{}
	mu      sync.Mutex
}

// NewCoverage returns a Coverage that covers nothing.
func NewCoverage() *Coverage {
	return &Coverage{covered: map[schema.GroupResource]struct{}{}, changed: make(chan struct{}, 1)}
}

// Covers reports whether the running Watcher collects gr.
func (c *Coverage) Covers(gr schema.GroupResource) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.covered[gr]
	return ok
}

// Changed receives after Covers changed for some resource. Changes may collapse into one
// receive, so a reader checks every resource it cares about after each receive.
func (c *Coverage) Changed() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.changed
}

func (c *Coverage) set(gr schema.GroupResource, on bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	_, was := c.covered[gr]
	if on {
		c.covered[gr] = struct{}{}
	} else {
		delete(c.covered, gr)
	}
	c.mu.Unlock()
	if was != on {
		c.notify()
	}
}

func (c *Coverage) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	n := len(c.covered)
	c.covered = map[schema.GroupResource]struct{}{}
	c.mu.Unlock()
	if n > 0 {
		c.notify()
	}
}

func (c *Coverage) notify() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}
