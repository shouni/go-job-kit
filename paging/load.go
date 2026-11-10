package paging

import (
	"context"
	"log/slog"
	"sync"
)

// DefaultConcurrency is the default concurrency for loading one page of metadata.
const DefaultConcurrency = 10

// WithConcurrency changes the concurrency for loading one page of metadata.
// A value of 0 or less uses DefaultConcurrency. Only LoadPage honours it.
func WithConcurrency(n int) Option {
	return func(o *options) { o.concurrency = n }
}

// WithLogger replaces the logger that records IDs that failed to load.
// The default is slog.Default(). Only LoadPage honours it.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// LoadPage cuts out a page and then loads the metadata for its IDs concurrently.
//
// A listing has two stages: "order the job IDs" and "read the metadata one by one". The
// second stage, even for one page, is dozens of storage reads, so it cannot run serially,
// yet running it concurrently breaks the order. This puts that assembly in one place.
//
//	items, meta, err := paging.LoadPage(ctx, jobIDs, page, perPage, jobid.SortKey, repo.loadHistory)
//
// See SelectIDs for how to choose sortKey (sorting is delegated to it).
//
// It behaves as follows.
//
//   - The order is exactly the order SelectIDs returned (newest first).
//   - An error returned by load is logged as a warning and that ID is dropped from the list,
//     so that one corrupt record or a racing delete does not lose the whole page.
//   - Cancellation or expiry of ctx is returned as an error. If every read failed and a
//     "page with 0 items" were returned, the caller could not tell it from a genuinely empty list.
//   - PageMeta is already corrected to the number of items actually loaded (as AdjustItemCount does).
//
// To keep an ID in the list with a substitute item when load fails, return a fallback value
// from inside load (the message and the shape of the substitute differ per service, so this
// package does not hold them).
//
//	load := func(ctx context.Context, jobID string) (History, error) {
//	    h, err := repo.load(ctx, jobID)
//	    if err != nil {
//	        return History{JobID: jobID, Title: jobID}, nil // keep it in the list
//	    }
//	    return h, nil
//	}
func LoadPage[T any](
	ctx context.Context,
	jobIDs []string,
	page int,
	perPage int,
	sortKey SortKeyFunc,
	load func(ctx context.Context, jobID string) (T, error),
	opts ...Option,
) ([]T, PageMeta, error) {
	cfg := options{concurrency: DefaultConcurrency, logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.concurrency <= 0 {
		cfg.concurrency = DefaultConcurrency
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	selectedIDs, meta := SelectIDs(jobIDs, page, perPage, sortKey)

	// Writing by index keeps the IDs' order while reading concurrently without a lock.
	// Items that could not be read stay nil and are removed afterwards.
	loaded := make([]*T, len(selectedIDs))
	sem := make(chan struct{}, cfg.concurrency)
	var wg sync.WaitGroup

loop:
	for i, jobID := range selectedIDs {
		if ctx.Err() != nil {
			break
		}

		// The slot is acquired outside the goroutine. Waiting inside it would cap concurrent
		// reads correctly but still start a goroutine for every item on the page at once.
		// Cancellation is watched while waiting for a slot too. Otherwise a cancellation that
		// arrives while every slot is taken keeps waiting and starts one more read in the freed slot.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Go(func() {
			defer func() { <-sem }()

			item, err := load(ctx, jobID)
			if err != nil {
				cfg.logger.WarnContext(ctx, "failed to load metadata for job list",
					"job_id", jobID,
					"error", err,
				)
				return
			}
			loaded[i] = &item
		})
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, PageMeta{}, err
	}

	items := make([]T, 0, len(loaded))
	for _, item := range loaded {
		if item != nil {
			items = append(items, *item)
		}
	}

	return items, AdjustItemCount(meta, len(items)), nil
}
