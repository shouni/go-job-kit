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
	selected, meta := SelectIDs(ids, 1, 2)

	// 降順（新しい順）にソートされた上で先頭2件
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
	selected, meta := SelectIDs(ids, 99, 2)

	if meta.Page != 2 {
		t.Errorf("Page = %d, want clamped to 2 (last page)", meta.Page)
	}
	if len(selected) != 1 {
		t.Errorf("selected = %v, want 1 item on last page", selected)
	}
}

// page に 0 や負値が来ても 1 ページ目として扱うこと。
// ページ番号はクエリ文字列から来るため、呼び出し側での検証に依存しません。
func TestSelectIDsClampsNonPositivePage(t *testing.T) {
	t.Parallel()

	for _, page := range []int{0, -1} {
		_, meta := SelectIDs([]string{"c1", "c2", "c3"}, page, 2)
		if meta.Page != 1 {
			t.Errorf("SelectIDs(page=%d) Page = %d, want 1", page, meta.Page)
		}
	}
}

func TestSelectIDsWithoutPaging(t *testing.T) {
	t.Parallel()

	selected, meta := SelectIDs([]string{"c1", "c2"}, 1, 0)

	if len(selected) != 2 {
		t.Errorf("selected = %v, want all items when perPage <= 0", selected)
	}
	if meta.TotalPages != 1 || meta.From != 1 || meta.To != 2 {
		t.Errorf("meta = %+v, unexpected for unpaginated result", meta)
	}
}

func TestSelectIDsEmpty(t *testing.T) {
	t.Parallel()

	selected, meta := SelectIDs(nil, 1, 10)

	if len(selected) != 0 {
		t.Errorf("selected = %v, want empty", selected)
	}
	// 0 件のときに "1〜0 件目" と表示されないよう From/To は 0 のままにする。
	if meta.Total != 0 || meta.From != 0 || meta.To != 0 || meta.HasNext || meta.HasPrev {
		t.Errorf("meta = %+v, unexpected for empty input", meta)
	}
}

// 呼び出し側のスライスを並べ替えないこと。
// 一覧のキャッシュをそのまま渡す利用側があるため、破壊すると
// キャッシュの中身が呼び出しごとに書き換わります。
func TestSelectIDsDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	ids := []string{"c1", "c3", "c2"}
	original := slices.Clone(ids)

	SelectIDs(ids, 1, 2)

	if !slices.Equal(ids, original) {
		t.Errorf("入力が変更された: %v, want %v", ids, original)
	}
}

// WithSortKey は、ID の文字列比較がプレフィックス順になってしまう一覧のためのもの。
// ap-mv は "video-recipe-" / "short-" / "mv-" 等を混在させるため、これが無いと
// 古いジョブが先頭に来ます。
func TestSelectIDsWithSortKey(t *testing.T) {
	t.Parallel()

	ids := []string{
		"video-recipe-20260706-194856-aaa",
		"mv-20260711-010101-bbb",
		"short-20260710-155126-ccc",
		"no-timestamp-job",
	}

	selected, meta := SelectIDs(ids, 1, 10, WithSortKey(embeddedTimestamp))

	want := []string{
		"mv-20260711-010101-bbb",
		"short-20260710-155126-ccc",
		"video-recipe-20260706-194856-aaa",
		"no-timestamp-job", // キーが空の ID は末尾へ
	}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
	if meta.Total != 4 {
		t.Errorf("meta.Total = %d, want 4", meta.Total)
	}
}

// キーが同値のときは ID の降順で安定させること。
func TestSelectIDsWithSortKeyBreaksTiesByID(t *testing.T) {
	t.Parallel()

	ids := []string{"mv-20260711-010101-aaa", "mv-20260711-010101-ccc", "mv-20260711-010101-bbb"}

	selected, _ := SelectIDs(ids, 1, 10, WithSortKey(embeddedTimestamp))

	want := []string{"mv-20260711-010101-ccc", "mv-20260711-010101-bbb", "mv-20260711-010101-aaa"}
	if !slices.Equal(selected, want) {
		t.Errorf("selected = %v, want %v", selected, want)
	}
}

func TestAdjustItemCountHandlesPartialFailures(t *testing.T) {
	t.Parallel()

	meta := PageMeta{Page: 1, PerPage: 2, Total: 5, From: 1, To: 2}

	adjusted := AdjustItemCount(meta, 1) // 2件中1件だけ読み込めた想定
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

// embeddedTimestamp は ap-mv の historyCreatedAtRaw 相当のソートキーです。
// この抽出規則自体はライブラリに入れていません（利用側が 1 つのみのため）。
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
