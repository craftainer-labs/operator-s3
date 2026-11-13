package controller

import (
	"sync"
	"time"
)

// StoreCache keeps one Store per object store between reconciles, so the
// connections to the store are reused (keep-alive) instead of opened for every
// reconcile. An entry is valid for one version, the stamp: the store object
// (its UID and generation) and everything its connection is built from (credentials, CA,
// proxy), so it is replaced as soon as any of them changes. It is dropped when
// the store is gone (Forget) or has not been used for a while. The zero value
// is not usable; a nil *StoreCache caches nothing.
type StoreCache struct {
	mu      sync.Mutex
	idle    time.Duration
	now     func() time.Time
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	version string
	store   Store
	used    time.Time
}

// NewStoreCache returns a cache that drops an entry idle for longer than idle.
func NewStoreCache(idle time.Duration) *StoreCache {
	return &StoreCache{idle: idle, now: time.Now, entries: map[string]*cacheEntry{}}
}

// Get returns the Store cached for key if it was built for version, else builds
// one, replaces the entry and closes the idle connections of the old one. A
// failed build caches nothing.
func (c *StoreCache) Get(key, version string, build func() (Store, error)) (Store, error) {
	if c == nil {
		return build()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.used) > c.idle {
			c.drop(k)
		}
	}
	if e, ok := c.entries[key]; ok && e.version == version {
		e.used = now
		return e.store, nil
	}
	c.drop(key)
	s, err := build()
	if err != nil {
		return nil, err
	}
	c.entries[key] = &cacheEntry{version: version, store: s, used: now}
	return s, nil
}

// Forget drops the entry of a store that no longer exists.
func (c *StoreCache) Forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(key)
}

// Len is the number of cached stores.
func (c *StoreCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// drop removes an entry and closes its idle connections. The caller holds mu.
func (c *StoreCache) drop(key string) {
	e, ok := c.entries[key]
	if !ok {
		return
	}
	delete(c.entries, key)
	if cl, ok := e.store.(interface{ CloseIdleConnections() }); ok {
		cl.CloseIdleConnections()
	}
}
