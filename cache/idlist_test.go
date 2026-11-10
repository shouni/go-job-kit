package cache_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shouni/go-job-kit/cache"
)

const testPrefix = "gs://bucket/jobs/"

// A listing scan is a List over the whole bucket, so later calls return without scanning.
func TestIDListCachesCollectResult(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	var calls int
	collect := func(context.Context) ([]string, error) {
		calls++
		return []string{"job-b", "job-a"}, nil
	}

	for range 3 {
		got, err := list.Load(context.Background(), testPrefix, collect)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !slices.Equal(got, []string{"job-b", "job-a"}) {
			t.Fatalf("Load() = %v", got)
		}
	}

	if calls != 1 {
		t.Errorf("collect calls = %d, want 1", calls)
	}
}

// Handing out the cached slice would let a sort on the receiving side leak into other concurrent requests.
func TestIDListReturnsCopy(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	collect := func(context.Context) ([]string, error) {
		return []string{"job-b", "job-a"}, nil
	}

	first, err := list.Load(context.Background(), testPrefix, collect)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	slices.Sort(first)

	second, err := list.Load(context.Background(), testPrefix, collect)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(second, []string{"job-b", "job-a"}) {
		t.Errorf("second = %v; the first call's sort leaked into the cache", second)
	}
}

// Prevents a delete or addition from staying invisible on screen for the length of the TTL.
func TestIDListInvalidate(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	var calls int
	collect := func(context.Context) ([]string, error) {
		calls++
		return []string{"job-a"}, nil
	}

	if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	list.Invalidate(testPrefix)
	if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if calls != 2 {
		t.Errorf("collect calls = %d, want 2 (rescan after discarding)", calls)
	}
}

// If Invalidate comes in during a scan, that scan's result (the pre-delete list) is not
// cached. Otherwise a deleted job would stay in the list for the TTL.
func TestIDListInvalidateDuringScan(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		list := cache.NewIDList(time.Minute)
		release := make(chan struct{})
		var calls atomic.Int32

		stale := func(context.Context) ([]string, error) {
			calls.Add(1)
			<-release
			return []string{"job-a", "job-deleted"}, nil
		}
		fresh := func(context.Context) ([]string, error) {
			calls.Add(1)
			return []string{"job-a"}, nil
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := list.Load(context.Background(), testPrefix, stale); err != nil {
				t.Errorf("Load() error = %v", err)
			}
		}()
		synctest.Wait() // wait for the scan to start

		// A delete came in during the scan. Later Loads do not join the in-flight scan.
		list.Invalidate(testPrefix)
		got, err := list.Load(context.Background(), testPrefix, fresh)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if want := []string{"job-a"}; !slices.Equal(got, want) {
			t.Errorf("Load() right after Invalidate = %v, want %v", got, want)
		}

		close(release)
		<-done

		// When the old scan finishes, it does not overwrite the new list.
		got, err = list.Load(context.Background(), testPrefix, fresh)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if want := []string{"job-a"}; !slices.Equal(got, want) {
			t.Errorf("Load() after the old scan completed = %v, want %v", got, want)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("collect calls = %d, want 2", got)
		}
	})
}

// A failed scan is not cached; the next call retries.
func TestIDListDoesNotCacheFailure(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	wantErr := errors.New("list failed")
	var calls int
	collect := func(context.Context) ([]string, error) {
		calls++
		return nil, wantErr
	}

	for range 2 {
		if _, err := list.Load(context.Background(), testPrefix, collect); !errors.Is(err, wantErr) {
			t.Fatalf("Load() error = %v, want %v", err, wantErr)
		}
	}
	if calls != 2 {
		t.Errorf("collect calls = %d, want 2", calls)
	}
}

// If every read extended the expiry, new jobs would never appear in the list.
func TestIDListExpires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		list := cache.NewIDList(30 * time.Millisecond)
		var calls int
		collect := func(context.Context) ([]string, error) {
			calls++
			return []string{"job-a"}, nil
		}

		if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		synctest.Sleep(60 * time.Millisecond)
		if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		if calls != 2 {
			t.Errorf("collect calls = %d, want 2 (rescan after expiry)", calls)
		}
	})
}

// The cache can be wired in as an optional feature (scanning still works with nil).
func TestIDListNilReceiver(t *testing.T) {
	t.Parallel()

	var list *cache.IDList
	got, err := list.Load(context.Background(), testPrefix, func(context.Context) ([]string, error) {
		return []string{"job-a"}, nil
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(got, []string{"job-a"}) {
		t.Errorf("Load() = %v", got)
	}

	list.Invalidate(testPrefix)
	if list.Len() != 0 {
		t.Errorf("Len() = %d, want 0", list.Len())
	}
}

// The history screen is opened by several requests at once.
func TestIDListConcurrentAccess(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	collect := func(context.Context) ([]string, error) {
		return []string{"job-b", "job-a"}, nil
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			got, err := list.Load(context.Background(), testPrefix, collect)
			if err != nil {
				t.Errorf("Load() error = %v", err)
				return
			}
			slices.Sort(got) // a write on the receiving side must not leak to others
			if i%10 == 0 {
				list.Invalidate(testPrefix)
			}
		})
	}
	wg.Wait()
}

// Concurrent cache misses collapse into one scan. A listing scan is a List over the whole
// prefix, so it must not run once per request that piles up the moment the TTL expires.
func TestIDListConcurrentMissRunsCollectOnce(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		list := cache.NewIDList(time.Minute)
		release := make(chan struct{})
		var calls atomic.Int32
		collect := func(context.Context) ([]string, error) {
			calls.Add(1)
			<-release
			return []string{"job-b", "job-a"}, nil
		}

		const waiters = 20
		results := make([][]string, waiters)
		errs := make([]error, waiters)
		var wg sync.WaitGroup
		for i := range waiters {
			wg.Go(func() {
				results[i], errs[i] = list.Load(context.Background(), testPrefix, collect)
			})
		}

		// Wait for everyone to join the scan, then release them all.
		//
		// Releasing as soon as "the scan has started" can leave calls that have not entered Load
		// yet, and those start a second scan (this did fail now and then).
		// Wait returns once every other goroutine in the bubble is durably blocked, so it
		// guarantees "everyone has joined" without estimating a wait time.
		synctest.Wait()
		close(release)
		wg.Wait()

		for i := range waiters {
			if errs[i] != nil {
				t.Fatalf("Load()[%d] error = %v", i, errs[i])
			}
			if !slices.Equal(results[i], []string{"job-b", "job-a"}) {
				t.Fatalf("Load()[%d] = %v", i, results[i])
			}
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("collect calls = %d, want 1 (concurrent misses collapse into one scan)", got)
		}
	})
}

// A waiting call follows only its own ctx. The scan itself is not abandoned and warms
// the cache when it completes.
func TestIDListWaiterHonorsOwnContext(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	collect := func(context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []string{"job-a"}, nil
	}

	// The call that starts the scan stays blocked.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
			t.Errorf("scanning Load() error = %v", err)
		}
	}()
	<-entered

	// The waiter leaves as soon as its own ctx ends (it is not made to wait for the scan).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := list.Load(ctx, testPrefix, collect); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v, want context.Canceled", err)
	}

	close(release)
	<-leaderDone

	// After it left, the scan still completed and can be read from the cache.
	got, err := list.Load(context.Background(), testPrefix, collect)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(got, []string{"job-a"}) {
		t.Errorf("Load() = %v", got)
	}
	if callCount := calls.Load(); callCount != 1 {
		t.Errorf("collect calls = %d, want 1 (the scan is not abandoned)", callCount)
	}
}

// When the call that started the scan is cancelled, the waiting calls are not dragged
// along; they redo the scan with their own ctx.
func TestIDListRetriesAfterLeaderCancel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		list := cache.NewIDList(time.Minute)
		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		entered := make(chan struct{})
		var calls atomic.Int32
		collect := func(ctx context.Context) ([]string, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-ctx.Done() // the first scan blocks until cancelled, then fails
				return nil, ctx.Err()
			}
			return []string{"job-a"}, nil
		}

		leaderErr := make(chan error, 1)
		go func() {
			_, err := list.Load(leaderCtx, testPrefix, collect)
			leaderErr <- err
		}()
		<-entered

		waiterDone := make(chan struct{})
		var waiterIDs []string
		var waiterErr error
		go func() {
			defer close(waiterDone)
			waiterIDs, waiterErr = list.Load(context.Background(), testPrefix, collect)
		}()

		// Wait for the waiter to join the scan, then cancel. Wait returns once every other
		// goroutine in the bubble is durably blocked, so "it should have joined" needs no wait-time
		// estimate, and cancelling before it joins cannot happen.
		synctest.Wait()
		cancelLeader()

		<-waiterDone
		if waiterErr != nil {
			t.Fatalf("waiter's Load() error = %v, want nil (not dragged into another call's cancellation)", waiterErr)
		}
		if !slices.Equal(waiterIDs, []string{"job-a"}) {
			t.Errorf("waiter's Load() = %v", waiterIDs)
		}
		if err := <-leaderErr; !errors.Is(err, context.Canceled) {
			t.Errorf("scanning Load() error = %v, want context.Canceled", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("collect calls = %d, want 2 (failed scan + retry)", got)
		}
	})
}

// The default TTL is used (0 or less is rounded to DefaultIDListTTL).
func TestIDListDefaultTTL(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(0)
	if _, err := list.Load(context.Background(), testPrefix, func(context.Context) ([]string, error) {
		return []string{"job-a"}, nil
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if list.Len() != 1 {
		t.Errorf("Len() = %d, want 1", list.Len())
	}
}
