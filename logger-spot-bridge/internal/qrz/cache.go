// SPDX-License-Identifier: AGPL-3.0-or-later

package qrz

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Default cache tuning (config can override): callsign data moves at ham
// renewal/relocation speed, so positive entries live long; not-found entries
// are cached briefly so a mistyped call does not re-hit the API on every
// keystroke. The cap keeps the file bounded if the station works a huge
// variety of calls.
const (
	DefaultCacheDays       = 30 * 24 * time.Hour
	DefaultNegativeMinutes = 10 * time.Minute
	DefaultCacheMax        = 2000
)

// entry is one cached QRZ answer. NotFound entries carry a zero Record and
// expire on the (short) negative TTL.
type entry struct {
	Record   Record    `json:"record"`
	Fetched  time.Time `json:"fetched"`
	NotFound bool      `json:"not_found,omitempty"`
}

// Cache is a JSON-file-backed callsign→record store. Safe for concurrent
// use. The file is rewritten atomically (tmp + rename) on every write; a
// missing or corrupt file just means a cold cache.
type Cache struct {
	path   string
	ttl    time.Duration
	negTTL time.Duration
	max    int
	now    func() time.Time // injectable for tests

	mu      sync.Mutex
	entries map[string]entry
}

// OpenCache loads (or starts) the cache at path. Unreadable/corrupt files
// are tolerated as a cold start — the cache must never take the bridge down.
func OpenCache(path string, ttl, negTTL time.Duration, max int) *Cache {
	if ttl <= 0 {
		ttl = DefaultCacheDays
	}
	if negTTL <= 0 {
		negTTL = DefaultNegativeMinutes
	}
	if max <= 0 {
		max = DefaultCacheMax
	}
	c := &Cache{
		path:    path,
		ttl:     ttl,
		negTTL:  negTTL,
		max:     max,
		now:     time.Now,
		entries: map[string]entry{},
	}
	c.load()
	return c
}

func (c *Cache) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return // first run (or unreadable volume): cold cache
	}
	var entries map[string]entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return // corrupt: cold cache beats a refusal to start
	}
	c.entries = entries
}

// Get returns the cached answer for call. Positive hit: (record, true, nil).
// Negative hit: (zero, false, ErrNotFound). Miss or expired: (zero, false,
// nil). Expired entries are dropped lazily.
func (c *Cache) Get(call string) (Record, bool, error) {
	key := NormalizeCall(call)
	if key == "" {
		return Record{}, false, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return Record{}, false, nil
	}
	age := c.now().Sub(e.Fetched)
	ttl := c.ttl
	if e.NotFound {
		ttl = c.negTTL
	}
	if age > ttl || age < 0 {
		delete(c.entries, key)
		return Record{}, false, nil
	}
	if e.NotFound {
		return Record{}, false, ErrNotFound
	}
	return e.Record, true, nil
}

// Put caches a successful lookup and persists the file.
func (c *Cache) Put(rec Record) error {
	key := NormalizeCall(rec.Call)
	if key == "" {
		return fmt.Errorf("qrz cache: empty callsign")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{Record: rec, Fetched: c.now()}
	c.evictLocked()
	return c.saveLocked()
}

// PutNotFound caches a QRZ "not found" answer for the negative TTL.
func (c *Cache) PutNotFound(call string) error {
	key := NormalizeCall(call)
	if key == "" {
		return fmt.Errorf("qrz cache: empty callsign")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{Fetched: c.now(), NotFound: true}
	c.evictLocked()
	return c.saveLocked()
}

// evictLocked drops the oldest entries down to the cap.
func (c *Cache) evictLocked() {
	for len(c.entries) > c.max {
		oldestKey := ""
		var oldest time.Time
		for key, e := range c.entries {
			if oldestKey == "" || e.Fetched.Before(oldest) {
				oldestKey, oldest = key, e.Fetched
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

func (c *Cache) saveLocked() error {
	data, err := json.Marshal(c.entries)
	if err != nil {
		return fmt.Errorf("qrz cache: marshal: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("qrz cache: write: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("qrz cache: rename: %w", err)
	}
	return nil
}
