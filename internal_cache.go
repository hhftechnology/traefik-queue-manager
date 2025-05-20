// internal_cache.go
package traefik_queue_manager

import (
	"sync"
	"time"
)

// Item represents a cached item with expiration
type CacheItem struct {
	Object     interface{}
	Expiration int64 // Unix nano time when this item expires
}

// SimpleCache provides a thread-safe in-memory key/value store with expiration
type SimpleCache struct {
	items             map[string]CacheItem
	mu                sync.RWMutex
	defaultExpiration time.Duration
	cleanupInterval   time.Duration
	stopCleanup       chan bool
}

// Constants for expiration
const (
	NoExpiration      time.Duration = -1
	DefaultExpiration time.Duration = 0
)

// NewSimpleCache creates a new cache with the provided default expiration duration
// and cleanup interval. If the cleanup interval is <= 0, expired items are not
// automatically deleted from the cache.
func NewSimpleCache(defaultExpiration, cleanupInterval time.Duration) *SimpleCache {
	cache := &SimpleCache{
		items:             make(map[string]CacheItem),
		defaultExpiration: defaultExpiration,
		cleanupInterval:   cleanupInterval,
	}

	// Start the cleanup goroutine if a cleanup interval is specified
	if cleanupInterval > 0 {
		cache.stopCleanup = make(chan bool)
		go cache.startCleanupTimer()
	}

	return cache
}

// startCleanupTimer starts a timer that periodically cleans up expired items
func (c *SimpleCache) startCleanupTimer() {
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.DeleteExpired()
		case <-c.stopCleanup:
			return
		}
	}
}

// Set adds an item to the cache, replacing any existing item with the same key
func (c *SimpleCache) Set(k string, x interface{}, d time.Duration) {
	var exp int64

	if d == DefaultExpiration {
		d = c.defaultExpiration
	}

	if d > 0 {
		exp = time.Now().Add(d).UnixNano()
	}

	c.mu.Lock()
	c.items[k] = CacheItem{
		Object:     x,
		Expiration: exp,
	}
	c.mu.Unlock()
}

// Get retrieves an item from the cache and returns (item, found)
func (c *SimpleCache) Get(k string) (interface{}, bool) {
	c.mu.RLock()
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return nil, false
	}

	// Check if the item has expired
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return nil, false
		}
	}

	c.mu.RUnlock()
	return item.Object, true
}

// Delete removes an item from the cache
func (c *SimpleCache) Delete(k string) {
	c.mu.Lock()
	delete(c.items, k)
	c.mu.Unlock()
}

// DeleteExpired deletes all expired items from the cache
func (c *SimpleCache) DeleteExpired() {
	now := time.Now().UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range c.items {
		// Delete if the item has expired
		if v.Expiration > 0 && now > v.Expiration {
			delete(c.items, k)
		}
	}
}

// Stop ends the cleanup timer if it's running
func (c *SimpleCache) Stop() {
	if c.cleanupInterval > 0 {
		c.stopCleanup <- true
	}
}

// ItemCount returns the number of items in the cache (including expired items)
func (c *SimpleCache) ItemCount() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// Flush deletes all items from the cache
func (c *SimpleCache) Flush() {
	c.mu.Lock()
	c.items = make(map[string]CacheItem)
	c.mu.Unlock()
}