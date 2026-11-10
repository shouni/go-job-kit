// Package paging cuts a list of job IDs into pages, newest first, and builds the
// page metadata a screen needs.
//
// Page numbers start at 1. A perPage of 0 or less disables paging and returns every item.
package paging

import (
	"cmp"
	"log/slog"
	"slices"

	"github.com/shouni/go-utils/paging"
)

// PageMeta is the metadata a list screen needs to render pagination.
//
// It is go-utils' paging.PageMeta. Returning the same type as the Firestore listings
// (gcp-kit/jobstatus) keeps edge values such as total_pages for an empty list and
// prev_page on page 1 from disagreeing for an M2M client that reads both through one type.
type PageMeta = paging.PageMeta

// SortKeyFunc extracts the string used for sorting from a job ID.
// Implement it so that descending order of the result is "newest first" (see SelectIDs for how to choose one).
type SortKeyFunc func(jobID string) string

type options struct {
	concurrency int
	logger      *slog.Logger
}

// Option changes how LoadPage behaves.
// SelectIDs does no loading, so no option affects it.
type Option func(*options)

// SelectIDs sorts jobIDs newest first and cuts out the requested page.
//
// sortKey is how the sort key is extracted, and the caller must always choose it.
// It is a parameter rather than an option because a default would let callers keep calling
// with the wrong default. A mistake here still returns a normal-looking list; only the order
// silently breaks.
//
//   - For IDs that embed a timestamp, pass go-utils' jobid.SortKey.
//     When IDs with different per-purpose prefixes share one list, or the ID format changed
//     along the way, comparing the ID strings orders them by prefix rather than by time (the
//     difference appears before the timestamp part). Every app this was ported from is in
//     that situation.
//   - nil sorts by the IDs themselves, descending. That order is correct only when the IDs
//     share a single prefix immediately followed by the timestamp.
//
// IDs whose key is the empty string go last (the empty string is the smallest in descending order).
// Ties on the key are broken by the ID, descending.
//
// The argument slice is not modified (sorting happens on a copy).
// An out-of-range page is clamped to the last page. Edge values of the metadata (TotalPages
// for an empty list, PrevPage on page 1 and so on) follow paging.New's rules.
func SelectIDs(jobIDs []string, page int, perPage int, sortKey SortKeyFunc) ([]string, PageMeta) {
	sorted := slices.Clone(jobIDs)
	sortDesc(sorted, sortKey)

	meta := paging.New(page, perPage, len(sorted))
	if perPage <= 0 {
		return sorted, meta
	}
	start := min(meta.Offset(), len(sorted))
	end := min(start+perPage, len(sorted))
	return sorted[start:end], meta
}

// sortDesc sorts by the sort key, descending (ties broken by the ID, descending).
func sortDesc(jobIDs []string, sortKey SortKeyFunc) {
	if sortKey == nil {
		slices.Sort(jobIDs)
		slices.Reverse(jobIDs)
		return
	}

	slices.SortFunc(jobIDs, func(a, b string) int {
		// Key descending; ties are broken by the ID, descending.
		if c := cmp.Compare(sortKey(b), sortKey(a)); c != 0 {
			return c
		}
		return cmp.Compare(b, a)
	})
}

// AdjustItemCount corrects From/To to match the number of items actually loaded.
//
// A listing orders the IDs before reading the metadata itself, so when some reads fail the
// page shows fewer items than SelectIDs expected. Use it to avoid saying "showing items
// 1-10" while only 8 are listed.
func AdjustItemCount(meta PageMeta, itemCount int) PageMeta {
	return meta.WithItemCount(itemCount)
}
