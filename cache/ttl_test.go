package cache_test

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shouni/go-job-kit/cache"
)

func TestSetGetDelete(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	if _, ok := c.Get("job-1"); ok {
		t.Error("a key that was never set was returned")
	}

	c.Set("job-1", "value")
	got, ok := c.Get("job-1")
	if !ok || got != "value" {
		t.Errorf("Get() = %q, %v; want \"value\", true", got, ok)
	}

	c.Delete("job-1")
	if _, ok := c.Get("job-1"); ok {
		t.Error("value still returned after Delete")
	}
}

// Keys go through the same normalization as storage paths.
// Otherwise a value written as "dir/job-1" cannot be read as "job-1",
// and history that was just updated is shown stale.
func TestKeyIsNormalizedLikeStoragePath(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	c.Set("../../job-1", "value")

	got, ok := c.Get("job-1")
	if !ok || got != "value" {
		t.Errorf("Get(\"job-1\") = %q, %v; want \"value\", true", got, ok)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (not normalized, so it became a separate entry)", c.Len())
	}
}

// A value that cannot be normalized still works; the cache just does not help.
func TestUnnormalizableKeyStillUsable(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	c.Set("日本語", "value")
	if got, ok := c.Get("日本語"); !ok || got != "value" {
		t.Errorf("Get() = %q, %v; want \"value\", true", got, ok)
	}
}

// Expired values are not returned.
func TestExpiredEntryIsNotReturned(t *testing.T) {
	t.Parallel()

	// time.Sleep inside the bubble only advances virtual time, so the retention period
	// can elapse without waiting in real time.
	synctest.Test(t, func(t *testing.T) {
		c := cache.NewTTL[string](20 * time.Millisecond)
		defer c.Close()

		c.Set("job-1", "value")
		// Sleep alone does not guarantee the janitor goroutine ran.
		// synctest.Sleep advances the clock and then waits for everything else to settle.
		synctest.Sleep(60 * time.Millisecond)

		if _, ok := c.Get("job-1"); ok {
			t.Error("expired value was returned")
		}
	})
}

// Expired entries are reclaimed.
// If reclaiming were never started, a cache keyed by job ID would grow without bound.
func TestExpiredEntryIsEvicted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c := cache.NewTTL[string](20 * time.Millisecond)
		defer c.Close()

		c.Set("job-1", "value")

		// The goroutine NewTTL started does the reclaiming. Sleep moves past the retention
		// period, and Wait waits for the reclaiming to finish. There is no need to poll with a
		// deadline for "it should be reclaimed eventually".
		synctest.Sleep(30 * time.Millisecond)

		if got := c.Len(); got != 0 {
			t.Errorf("Len() = %d, want 0 (expired entry was not reclaimed)", got)
		}
	})
}

func TestNonPositiveTTLFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](0)
	defer c.Close()

	c.Set("job-1", "value")
	if _, ok := c.Get("job-1"); !ok {
		t.Error("the default TTL was not applied")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	c.Close()
	c.Close()
}

// Closing concurrently from several goroutines is safe.
// A "check it has stopped, then stop" implementation can be interrupted between the check and
// the action and stop twice. Sequential double Close cannot detect that.
func TestConcurrentCloseIsSafe(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)

	const goroutines = 16
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start // hit it at the same time
			c.Close()
		}()
	}

	close(start)
	wg.Wait()
}
