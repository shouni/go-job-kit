package paging

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSelectIDsPaginates(t *testing.T) {
	t.Parallel()

	ids := []string{"c1", "c3", "c2", "c5", "c4"}
	selected, meta := SelectIDs(ids, 1, 2, nil)

	// Sorted descending (newest first), then the first two.
	if want := []string{"c5", "c4"}; !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if meta.Total != 5 || meta.TotalPages != 3 || !meta.HasNext || meta.HasPrev {
		t.Errorf("meta = %+v, unexpected", meta)
	}
	if meta.From != 1 || meta.To != 2 {
		t.Errorf("From/To = %d/%d, want 1/2", meta.From, meta.To)
	}
}

func TestSelectIDsClampsOutOfRangePage(t *testing.T) {
	t.Parallel()

	ids := []string{"c1", "c2", "c3"}
	selected, meta := SelectIDs(ids, 99, 2, nil)

	if meta.Page != 2 {
		t.Errorf("Page = %d, want clamped to 2 (last page)", meta.Page)
	}
	if len(selected) != 1 {
		t.Errorf("selected = %v, want 1 item on last page", selected)
	}
}

// A page of 0 or a negative value is treated as page 1.
// The page number comes from the query string, so this must not rely on the caller validating it.
func TestSelectIDsClampsNonPositivePage(t *testing.T) {
	t.Parallel()

	for _, page := range []int{0, -1} {
		_, meta := SelectIDs([]string{"c1", "c2", "c3"}, page, 2, nil)
		if meta.Page != 1 {
			t.Errorf("SelectIDs(page=%d) Page = %d, want 1", page, meta.Page)
		}
	}
}

func TestSelectIDsWithoutPaging(t *testing.T) {
	t.Parallel()

	selected, meta := SelectIDs([]string{"c1", "c2"}, 1, 0, nil)

	if len(selected) != 2 {
		t.Errorf("selected = %v, want all items when perPage <= 0", selected)
	}
	if meta.TotalPages != 1 || meta.From != 1 || meta.To != 2 {
		t.Errorf("meta = %+v, unexpected for unpaginated result", meta)
	}
}

func TestSelectIDsEmpty(t *testing.T) {
	t.Parallel()

	selected, meta := SelectIDs(nil, 1, 10, nil)

	if len(selected) != 0 {
		t.Errorf("selected = %v, want empty", selected)
	}
	// With 0 items From/To stay 0, so the page never says "items 1-0".
	if meta.Total != 0 || meta.From != 0 || meta.To != 0 || meta.HasNext || meta.HasPrev {
		t.Errorf("meta = %+v, unexpected for empty input", meta)
	}
}

// Edge values match the Firestore listing (gcp-kit/jobstatus).
// An M2M client reads both through one type, so if TotalPages for an empty list or
// PrevPage on page 1 disagreed, responses under the same contract would differ per service.
func TestSelectIDsEdgeValuesMatchFirestoreListing(t *testing.T) {
	t.Parallel()

	_, empty := SelectIDs(nil, 5, 10, nil)
	if empty.Page != 1 || empty.TotalPages != 1 || empty.HasPrev || empty.PrevPage != 1 || empty.NextPage != 1 {
		t.Errorf("page=5 on an empty list: meta = %+v, want Page 1 / TotalPages 1 / PrevPage 1 / NextPage 1", empty)
	}

	_, first := SelectIDs([]string{"c", "b", "a"}, 1, 2, nil)
	if first.PrevPage != 1 || first.NextPage != 2 {
		t.Errorf("page 1: PrevPage %d NextPage %d, want 1 2", first.PrevPage, first.NextPage)
	}

	_, last := SelectIDs([]string{"c", "b", "a"}, 2, 2, nil)
	if last.NextPage != 2 || last.HasNext {
		t.Errorf("last page: NextPage %d HasNext %v, want 2 false", last.NextPage, last.HasNext)
	}
}

// The caller's slice is not reordered.
// Some callers pass the cached listing as is, so mutating it would rewrite the
// cache's contents on every call.
func TestSelectIDsDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	ids := []string{"c1", "c3", "c2"}
	original := slices.Clone(ids)

	SelectIDs(ids, 1, 2, nil)

	if !slices.Equal(ids, original) {
		t.Errorf("input was modified: %v, want %v", ids, original)
	}
}

// sortKey is for listings where comparing ID strings orders them by prefix.
// When different per-purpose prefixes are mixed, old jobs come first without it.
func TestSelectIDsUsesSortKey(t *testing.T) {
	t.Parallel()

	ids := []string{
		"video-recipe-20260706-194856-aaa",
		"mv-20260711-010101-bbb",
		"short-20260710-155126-ccc",
		"no-timestamp-job",
	}

	selected, meta := SelectIDs(ids, 1, 10, embeddedTimestamp)

	want := []string{
		"mv-20260711-010101-bbb",
		"short-20260710-155126-ccc",
		"video-recipe-20260706-194856-aaa",
		"no-timestamp-job", // IDs with an empty key go last
	}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if meta.Total != 4 {
		t.Errorf("meta.Total = %d, want 4", meta.Total)
	}
}

// Ties on the key are broken by the ID, descending.
func TestSelectIDsBreaksTiesByID(t *testing.T) {
	t.Parallel()

	ids := []string{"mv-20260711-010101-aaa", "mv-20260711-010101-ccc", "mv-20260711-010101-bbb"}

	selected, _ := SelectIDs(ids, 1, 10, embeddedTimestamp)

	want := []string{"mv-20260711-010101-ccc", "mv-20260711-010101-bbb", "mv-20260711-010101-aaa"}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
}

func TestAdjustItemCountHandlesPartialFailures(t *testing.T) {
	t.Parallel()

	meta := PageMeta{Page: 1, PerPage: 2, Total: 5, From: 1, To: 2}

	adjusted := AdjustItemCount(meta, 1) // assume only 1 of 2 items loaded
	if adjusted.To != 1 {
		t.Errorf("To = %d, want 1 (adjusted for partial load)", adjusted.To)
	}

	zero := AdjustItemCount(meta, 0)
	if zero.From != 0 || zero.To != 0 {
		t.Errorf("From/To = %d/%d, want 0/0 when itemCount is 0", zero.From, zero.To)
	}
}

func TestAdjustItemCountWithoutPaging(t *testing.T) {
	t.Parallel()

	meta := PageMeta{Page: 1, PerPage: 0, Total: 3, From: 1, To: 3}

	adjusted := AdjustItemCount(meta, 2)
	if adjusted.To != 2 {
		t.Errorf("To = %d, want 2", adjusted.To)
	}
}

// embeddedTimestamp is an example sort key that extracts a timestamp embedded mid-ID.
// The extraction rule depends on the caller's ID format, so it is not part of the library.
func embeddedTimestamp(jobID string) string {
	const layout = "20060102150405"

	parts := strings.Split(jobID, "-")
	for i := 0; i+1 < len(parts); i++ {
		raw := parts[i] + parts[i+1]
		if len(raw) != len(layout) {
			continue
		}
		if _, err := time.ParseInLocation(layout, raw, time.UTC); err != nil {
			continue
		}
		return raw
	}
	return ""
}
