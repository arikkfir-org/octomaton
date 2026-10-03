package http

import (
	"container/list"
	"sync"
	"time"
)

// Dedupe remembers recently seen delivery IDs (bounded LRU with a TTL) so that
// redelivered webhooks are processed once per replica, unless processing failed (Remove).
type Dedupe struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*list.Element
	order   *list.List // front = newest
	now     func() time.Time
}

type dedupeEntry struct {
	id   string
	seen time.Time
}

// NewDedupe creates a cache holding at most max IDs for ttl each.
func NewDedupe(max int, ttl time.Duration) *Dedupe {
	return &Dedupe{ttl: ttl, max: max, entries: map[string]*list.Element{}, order: list.New(), now: time.Now}
}

// Add records id and reports whether it was not seen within the TTL.
func (d *Dedupe) Add(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.evictExpired(now)
	if el, ok := d.entries[id]; ok {
		el.Value.(*dedupeEntry).seen = now
		d.order.MoveToFront(el)
		return false
	}
	d.entries[id] = d.order.PushFront(&dedupeEntry{id: id, seen: now})
	for d.order.Len() > d.max {
		d.remove(d.order.Back())
	}
	return true
}

// Remove forgets id, so that a redelivery is accepted (used when a delivery could not be queued, or
// its handling failed).
func (d *Dedupe) Remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.entries[id]; ok {
		d.remove(el)
	}
}

func (d *Dedupe) evictExpired(now time.Time) {
	for el := d.order.Back(); el != nil; el = d.order.Back() {
		if now.Sub(el.Value.(*dedupeEntry).seen) < d.ttl {
			return
		}
		d.remove(el)
	}
}

func (d *Dedupe) remove(el *list.Element) {
	d.order.Remove(el)
	delete(d.entries, el.Value.(*dedupeEntry).id)
}
