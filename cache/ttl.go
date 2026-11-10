// Package cache provides in-memory caches with TTL for history listings.
// TTL caches metadata keyed by job ID, and IDList caches the result of a listing scan.
//
// A history listing orders the job IDs and then reads the metadata one by one, so every page
// turn rereads the same objects. These are thin caches to avoid that.
package cache

import (
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/shouni/go-utils/jobid"
)

// DefaultTTL is the retention period used when NewTTL is given 0 or less.
const DefaultTTL = 10 * time.Minute

// TTL is a cache with TTL keyed by job ID. The zero value is not usable.
// All methods, Close included, are safe for concurrent use by multiple goroutines.
type TTL[T any] struct {
	cache *ttlcache.Cache[string, T]
	// closeOnce makes the janitor stop exactly once.
	closeOnce sync.Once
}

// NewTTL creates a cache and starts the goroutine that reclaims expired entries.
//
// Without that goroutine, expired entries stay in memory until the same key is overwritten.
// Keyed by job ID, the cache grows by every new job, so forgetting to start it turns directly
// into a build-up. It is started at creation rather than left as a step for the caller, and
// stopping is gathered into Close.
//
// Call Close when done with it.
func NewTTL[T any](ttl time.Duration) *TTL[T] {
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	c := &TTL[T]{
		cache: ttlcache.New[string, T](
			ttlcache.WithTTL[string, T](ttl),
			// If every read extended the expiry, a stale value that is never updated would stay forever.
			// Retention counts from the last write.
			ttlcache.WithDisableTouchOnHit[string, T](),
		),
	}

	go c.cache.Start()

	return c
}

// Get returns the cached value. ok is false when it is not cached or has expired.
func (c *TTL[T]) Get(jobID string) (value T, ok bool) {
	item := c.cache.Get(key(jobID))
	if item == nil {
		var zero T
		return zero, false
	}
	return item.Value(), true
}

// Set stores value in the cache.
func (c *TTL[T]) Set(jobID string, value T) {
	c.cache.Set(key(jobID), value, ttlcache.DefaultTTL)
}

// Delete removes the value from the cache.
func (c *TTL[T]) Delete(jobID string) {
	c.cache.Delete(key(jobID))
}

// Len returns the number of entries held. It is mainly for tests and monitoring.
func (c *TTL[T]) Len() int {
	return c.cache.Len()
}

// Close stops the janitor goroutine and waits for it to finish.
//
// It is safe to call any number of times, concurrently from several goroutines. Stopping
// goes through sync.Once because a "check it has not stopped yet, then stop" shape would let
// another goroutine slip in between the check and the action.
func (c *TTL[T]) Close() {
	c.closeOnce.Do(c.cache.Stop)
}

// key normalizes the job ID used as a cache key.
//
// If "abc" and "dir/abc", which name the same job, became separate entries, a value that was
// just updated would not be read: a cache miss. They are aligned by the same normalization as
// storage paths. A value that cannot be normalized is used as the key as is (leaving the
// cache ineffective is safer than failing the call for the cache's sake).
func key(jobID string) string {
	safe, err := jobid.Sanitize(jobID)
	if err != nil {
		return jobID
	}
	return safe
}
