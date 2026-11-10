package cache

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/sync/singleflight"
)

// DefaultIDListTTL is the retention period used when NewIDList is given 0 or less.
//
// It is much shorter than the metadata's (DefaultTTL) to minimize the delay before a newly
// finished job appears in the list. Deletions discard the entry explicitly via Invalidate, so a
// delete never stays invisible on screen for the length of the TTL.
const DefaultIDListTTL = time.Minute

// IDList is a cache that briefly holds the job IDs found by a listing scan.
// The zero value is not usable. It is safe for concurrent use by multiple goroutines.
//
// Unlike the metadata cache (TTL), without this every visit to the history screen runs a
// List over the whole prefix.
//
// Unlike TTL, it starts no goroutine to reclaim expired entries (and therefore has no Close).
// It holds just one entry per listing unit, which is overwritten by writes to the same key.
// Conversely, use only the listing unit (such as the scanned prefix) as the key. Keying by an
// ever-growing value such as a job ID would pile up expired entries that are never reclaimed.
type IDList struct {
	cache *ttlcache.Cache[string, []string]
	// flight collapses scans into one when Loads for the same key miss the
	// cache concurrently (see Load).
	flight singleflight.Group

	// mu serializes updates to generation and the cache writes that depend on it.
	mu sync.Mutex
	// generation advances on every Invalidate. A scan remembers the value when it starts
	// and does not put its result in the cache if the value changed by the time it ends,
	// because a delete that came in during the scan means the result is the pre-delete list.
	//
	// It is not kept per key: being caught by another key's Invalidate costs only one scan's
	// worth of caching, and in exchange there is no set of keys to manage.
	generation uint64
}

// NewIDList creates a cache for job ID lists.
func NewIDList(ttl time.Duration) *IDList {
	if ttl <= 0 {
		ttl = DefaultIDListTTL
	}

	return &IDList{
		cache: ttlcache.New[string, []string](
			ttlcache.WithTTL[string, []string](ttl),
			// If every read extended the expiry, new jobs would never appear in the list.
			ttlcache.WithDisableTouchOnHit[string, []string](),
		),
	}
}

// Load returns the job ID list for key.
// It calls collect only when the list is not cached (or has expired), and keeps the result.
//
// With a nil receiver it calls collect directly, so the cache can be wired in as an
// optional feature.
//
// When Loads for the same key miss the cache concurrently, the scan runs only once and
// everyone shares its result. A listing scan is a List over the whole prefix, and this keeps
// requests that pile up the moment the TTL expires from each running one.
// Within that sharing, each call follows only its own ctx.
//
//   - If its own ctx ends while waiting, that call returns with ctx.Err().
//     The scan itself continues and warms the cache when it completes.
//   - If the ctx of the call that started the scan ends first, the waiting calls are
//     not dragged along; they redo the scan with their own ctx.
//
// It always returns a copy. Handing out the cached slice itself would let a sort on the
// receiving side leak into other concurrent requests (paging.SelectIDs does not modify its
// argument, but it matters when a caller sorts on its own).
func (c *IDList) Load(ctx context.Context, key string, collect func(context.Context) ([]string, error)) ([]string, error) {
	if c == nil || c.cache == nil {
		return collect(ctx)
	}

	for retried := false; ; retried = true {
		if cached := c.cache.Get(key); cached != nil {
			return slices.Clone(cached.Value()), nil
		}

		// While a key is in flight, singleflight runs only the first call's function.
		// So the ctx in this closure belongs to the call that actually started the scan.
		ch := c.flight.DoChan(key, func() (any, error) {
			started := c.currentGeneration()
			jobIDs, err := collect(ctx)
			if err != nil {
				return nil, err
			}
			c.store(key, jobIDs, started)
			return jobIDs, nil
		})

		select {
		case <-ctx.Done():
			// The scan is not abandoned. When it completes it goes into the cache for the next call.
			return nil, ctx.Err()
		case res := <-ch:
			if res.Err == nil {
				return slices.Clone(res.Val.([]string)), nil
			}
			// If this was only caught in the cancellation of the call that started the scan, redo it.
			// At most one retry, so that a collect that keeps returning ctx errors from its own
			// deadline does not loop (the second error is then returned as is).
			if !retried && ctx.Err() == nil && isContextError(res.Err) {
				continue
			}
			return nil, res.Err
		}
	}
}

// isContextError reports whether err comes from cancellation or an expired deadline.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// currentGeneration returns the current generation.
func (c *IDList) currentGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// store keeps the result only when no Invalidate came in since the scan started.
func (c *IDList) store(key string, jobIDs []string, started uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != started {
		return
	}
	c.cache.Set(key, jobIDs, ttlcache.DefaultTTL)
}

// Invalidate discards the cached list so that a job deletion or addition shows immediately.
//
// If it is called during a scan, that scan's result does not go into the cache. Later Loads
// do not join the in-flight scan; they scan anew. Calls already waiting on it receive the
// in-flight scan's result (which may be the list from before Invalidate).
func (c *IDList) Invalidate(key string) {
	if c == nil || c.cache == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.flight.Forget(key)
	c.cache.Delete(key)
}

// Len returns the number of entries held. It is mainly for tests and monitoring.
func (c *IDList) Len() int {
	if c == nil || c.cache == nil {
		return 0
	}
	return c.cache.Len()
}
