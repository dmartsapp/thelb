package kube

import (
	"context"
	"sync"
	"time"
)

// Cache is a resource kept in memory: listed, then watched, listed again when
// the watch can no longer go on. Readers take what it holds now.
type Cache[T any] struct {
	c    *Client
	path string
	key  func(*T) string

	mu      sync.Mutex
	items   map[string]*T
	synced  bool
	err     string    // why it is not watching, if it is not
	at      time.Time // when it last heard from the API server
	changed chan struct{}
	refused map[string]string // objects left out, not yet reported: key -> why
}

// NewCache is a cache of the objects at path, keyed by key.
func NewCache[T any](c *Client, path string, key func(*T) string) *Cache[T] {
	return &Cache[T]{c: c, path: path, key: key, items: map[string]*T{}, changed: make(chan struct{})}
}

// Backoff between failed lists and watches. Variables, for tests.
var (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// Run keeps the cache until ctx ends.
func (k *Cache[T]) Run(ctx context.Context) {
	wait := backoffMin
	for ctx.Err() == nil {
		items, rv, err := List[T](ctx, k.c, k.path)
		if err != nil {
			k.fail(err)
			if !sleep(ctx, wait) {
				return
			}
			wait = min(2*wait, backoffMax)
			continue
		}
		k.replace(items)
		wait = backoffMin
		// Watch, and watch again from where the last one ended, until the API
		// server says the resource version is too old: then list again.
		for ctx.Err() == nil {
			began := time.Now()
			rv, err = Watch(ctx, k.c, k.path, rv, func(ch Change[T]) error {
				k.apply(ch)
				return nil
			})
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// 410 Gone is routine - the resource version is too old to
				// watch from - and no failure: list again, at once.
				if !IsGone(err) {
					k.fail(err)
					if !sleep(ctx, wait) {
						return
					}
					wait = min(2*wait, backoffMax)
				}
				break
			}
			if time.Since(began) < time.Second && !sleep(ctx, wait) {
				return // a watch the server closes at once: do not spin
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (k *Cache[T]) replace(items []T) {
	m := make(map[string]*T, len(items))
	k.mu.Lock()
	for i := range items {
		if k.refuseLocked(&items[i]) {
			continue
		}
		m[k.key(&items[i])] = &items[i]
	}
	k.items, k.synced, k.err, k.at = m, true, "", time.Now()
	k.signalLocked()
	k.mu.Unlock()
}

func (k *Cache[T]) apply(ch Change[T]) {
	obj := ch.Object
	key := k.key(&obj)
	k.mu.Lock()
	if ch.Type == "DELETED" || k.refuseLocked(&obj) {
		delete(k.items, key)
	} else {
		k.items[key] = &obj
	}
	k.err, k.at = "", time.Now()
	k.signalLocked()
	k.mu.Unlock()
}

// refuseLocked is whether obj is left out - it says what is wrong with it - and
// records why, for Refused. k.mu is held.
func (k *Cache[T]) refuseLocked(obj *T) bool {
	c, ok := any(obj).(Checked)
	if !ok {
		return false
	}
	why := c.Problem()
	if why == "" {
		return false
	}
	if k.refused == nil {
		k.refused = map[string]string{}
	}
	if len(k.refused) < 100 { // reported in batches: a flood is not logged whole
		k.refused[k.key(obj)] = why
	}
	return true
}

// Refused is the objects left out since it was last asked - by key, with what
// is wrong with each - for the director to say once.
func (k *Cache[T]) Refused() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	r := k.refused
	k.refused = nil
	return r
}

func (k *Cache[T]) fail(err error) {
	k.mu.Lock()
	changed := k.err != err.Error()
	k.err = err.Error()
	if changed {
		k.signalLocked()
	}
	k.mu.Unlock()
}

func (k *Cache[T]) signalLocked() {
	close(k.changed)
	k.changed = make(chan struct{})
}

// Changed is closed at the next change.
func (k *Cache[T]) Changed() <-chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.changed
}

// Synced is whether the cache has listed once: until then, it knows nothing.
func (k *Cache[T]) Synced() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.synced
}

// Err is why the cache is not up to date now, or "".
func (k *Cache[T]) Err() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Items is what the cache holds: the objects themselves, which callers must
// not change.
func (k *Cache[T]) Items() map[string]*T {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]*T, len(k.items))
	for key, v := range k.items {
		out[key] = v
	}
	return out
}

// Get is the object with that key, or nil.
func (k *Cache[T]) Get(key string) *T {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.items[key]
}
