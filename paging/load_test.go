package paging_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/shouni/go-job-kit/paging"
)

func discardLogger() paging.Option {
	return paging.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Concurrent loading keeps the newest-first order SelectIDs returned.
func TestLoadPageKeepsSelectionOrder(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-05", "job-03", "job-04", "job-02"}
	items, meta, err := paging.LoadPage(context.Background(), jobIDs, 1, 3, nil,
		func(_ context.Context, jobID string) (string, error) {
			return jobID, nil
		})
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	want := []string{"job-05", "job-04", "job-03"}
	if !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
	if meta.Total != 5 || meta.From != 1 || meta.To != 3 {
		t.Errorf("meta = %+v", meta)
	}
}

// The caller must always choose the sort key as a parameter (no shape that can be called with a default).
func TestLoadPageUsesSortKey(t *testing.T) {
	t.Parallel()

	// {purpose}-{time} format. Comparing ID strings would order them by purpose prefix.
	jobIDs := []string{"mv-300", "recipe-100", "short-200"}
	items, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 3,
		func(jobID string) string {
			_, ts, _ := strings.Cut(jobID, "-")
			return ts
		},
		func(_ context.Context, jobID string) (string, error) { return jobID, nil })
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	want := []string{"mv-300", "short-200", "recipe-100"}
	if !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
}

// One corrupt record or a racing delete does not lose the whole page,
// and the displayed count is corrected by that much.
func TestLoadPageSkipsFailedItems(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-02", "job-03", "job-04"}
	items, meta, err := paging.LoadPage(context.Background(), jobIDs, 1, 4, nil,
		func(_ context.Context, jobID string) (string, error) {
			if jobID == "job-03" {
				return "", errors.New("broken metadata")
			}
			return jobID, nil
		}, discardLogger())
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	want := []string{"job-04", "job-02", "job-01"}
	if !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
	if meta.Total != 4 {
		t.Errorf("Total = %d, want 4 (the list's total does not change with load results)", meta.Total)
	}
	if meta.To != 3 {
		t.Errorf("To = %d, want 3 (corrected to the items actually listed)", meta.To)
	}
}

// "0 items" caused by every read failing is not mistaken for a genuinely empty list.
func TestLoadPageReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := paging.LoadPage(ctx, []string{"job-01", "job-02"}, 1, 2, nil,
		func(ctx context.Context, jobID string) (string, error) {
			return jobID, ctx.Err()
		}, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadPage() error = %v, want context.Canceled", err)
	}
}

// To keep some failed loads in the list, load can return a substitute value.
func TestLoadPageWithFallbackInsideLoad(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), []string{"job-01", "job-02"}, 1, 2, nil,
		func(_ context.Context, jobID string) (string, error) {
			if jobID == "job-01" {
				return "placeholder:" + jobID, nil
			}
			return jobID, nil
		})
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	want := []string{"job-02", "placeholder:job-01"}
	if !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
	if meta.To != 2 {
		t.Errorf("To = %d, want 2", meta.To)
	}
}

// The concurrency limit is never exceeded (to bound concurrent reads from storage).
func TestLoadPageLimitsConcurrency(t *testing.T) {
	t.Parallel()

	jobIDs := make([]string, 50)
	for i := range jobIDs {
		jobIDs[i] = fmt.Sprintf("job-%02d", i)
	}

	var inFlight, peak atomic.Int64
	_, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 0, nil,
		func(_ context.Context, jobID string) (string, error) {
			current := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				highest := peak.Load()
				if current <= highest || peak.CompareAndSwap(highest, current) {
					break
				}
			}
			return jobID, nil
		}, paging.WithConcurrency(4))
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if got := peak.Load(); got > 4 {
		t.Errorf("peak concurrency = %d, want <= 4", got)
	}
}

// A cancellation while every slot is taken does not start the next read in a freed
// slot (waiting for a slot watches ctx).
func TestLoadPageStopsAcquiringAfterCancel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		// A load that blocks and ignores ctx. The first item keeps holding the slot.
		load := func(context.Context, string) (string, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return "", errors.New("gave up")
		}

		done := make(chan struct{})
		var err error
		go func() {
			defer close(done)
			_, _, err = paging.LoadPage(ctx, []string{"job-01", "job-02"}, 1, 10, nil, load,
				paging.WithConcurrency(1), discardLogger())
		}()

		<-started // once the first item holds the slot
		cancel()
		// After the cancel, the only way out of waiting for a slot is ctx.Done, so the second
		// item has stopped waiting before release. Wait for that to finish, then free the slot
		// and check that the second item does not start (no reliance on timing estimates).
		synctest.Wait()
		close(release)
		<-done

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LoadPage() error = %v, want context.Canceled", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("load calls = %d, want 1 (no new read starts after cancel)", got)
		}
	})
}

// A perPage of 0 or less loads every item without paging (as SelectIDs does).
func TestLoadPageWithoutPaging(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), []string{"job-01", "job-02"}, 1, 0, nil,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil })
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if len(items) != 2 {
		t.Errorf("items = %v, want 2 items", items)
	}
	if meta.TotalPages != 1 || meta.To != 2 {
		t.Errorf("meta = %+v", meta)
	}
}

func TestLoadPageWithNoJobs(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), nil, 1, 10, nil,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil })
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if len(items) != 0 {
		t.Errorf("items = %v, want empty", items)
	}
	if meta.Total != 0 || meta.From != 0 || meta.To != 0 {
		t.Errorf("meta = %+v", meta)
	}
}

// The argument slice is not modified (the same promise as SelectIDs).
func TestLoadPageDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-03", "job-02"}
	original := slices.Clone(jobIDs)

	if _, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 2, nil,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil }); err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if !slices.Equal(jobIDs, original) {
		t.Errorf("argument was modified: %v, want %v", jobIDs, original)
	}
}
