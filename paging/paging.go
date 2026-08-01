// Package paging は、ジョブ ID の一覧を新しい順に切り出し、画面表示に必要な
// ページメタデータを組み立てます。
//
// ページ番号は 1 始まりです。perPage が 0 以下のときはページングせず全件を返します。
package paging

import (
	"cmp"
	"slices"
)

// PageMeta は、一覧画面がページネーションを描画するために必要なメタデータです。
//
// JSON タグは ap-comp / ap-mv / ap-comic が返している既存のレスポンスと同じです。
// フロントエンドと M2M クライアントの双方が依存しているため、変更するときは
// 利用側の追随が要ります。
type PageMeta struct {
	Page       int  `json:"page"`
	PerPage    int  `json:"per_page"`
	Total      int  `json:"total"`
	TotalPages int  `json:"total_pages"`
	HasPrev    bool `json:"has_prev"`
	HasNext    bool `json:"has_next"`
	PrevPage   int  `json:"prev_page"`
	NextPage   int  `json:"next_page"`
	From       int  `json:"from"`
	To         int  `json:"to"`
}

// SortKeyFunc は、ジョブ ID から並べ替えに使う文字列を取り出します。
// 戻り値の降順が「新しい順」になるよう実装してください。
type SortKeyFunc func(jobID string) string

type options struct {
	sortKey SortKeyFunc
}

// Option は SelectIDs の挙動を変更します。
type Option func(*options)

// WithSortKey は、ジョブ ID そのものではなく、そこから取り出した文字列で並べ替えます。
//
// 既定では ID の降順がそのまま新しい順になることを前提にしています
// （go-utils/jobid が生成する時刻プレフィックス付き ID がこれを満たします）。
// ap-mv のように "video-recipe-" / "short-" / "mv-" と複数のプレフィックスが混在する
// 一覧では、ID の文字列比較がプレフィックス順になってしまうため、埋め込まれた
// タイムスタンプを取り出す関数をここへ渡してください。
//
// キーが空文字を返した ID は末尾に回ります（降順で空文字が最小になるため）。
// キーが同値のときは ID の降順で安定させます。
func WithSortKey(fn SortKeyFunc) Option {
	return func(o *options) { o.sortKey = fn }
}

// SelectIDs は jobIDs を新しい順に並べ替え、指定ページ分を切り出します。
//
// 引数のスライスは変更しません（並べ替えは複製に対して行います）。
// page が範囲外のときは最終ページへ丸めます。
func SelectIDs(jobIDs []string, page int, perPage int, opts ...Option) ([]string, PageMeta) {
	cfg := options{}
	for _, opt := range opts {
		opt(&cfg)
	}

	if page < 1 {
		page = 1
	}

	sorted := make([]string, len(jobIDs))
	copy(sorted, jobIDs)
	sortDesc(sorted, cfg.sortKey)

	total := len(sorted)
	totalPages := 0
	selectedIDs := sorted
	if perPage > 0 {
		totalPages = (total + perPage - 1) / perPage
		if totalPages > 0 && page > totalPages {
			page = totalPages
		}
		start := min((page-1)*perPage, total)
		end := min(start+perPage, total)
		selectedIDs = sorted[start:end]
	}

	meta := PageMeta{
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: totalPages,
		HasPrev:    page > 1,
		HasNext:    totalPages > 0 && page < totalPages,
		PrevPage:   page - 1,
		NextPage:   page + 1,
	}

	if total > 0 {
		if perPage > 0 {
			meta.From = (page-1)*perPage + 1
			meta.To = meta.From + len(selectedIDs) - 1
		} else {
			meta.TotalPages = 1
			meta.From = 1
			meta.To = len(selectedIDs)
		}
	}

	return selectedIDs, meta
}

// sortDesc は、ソートキーの降順（同値なら ID の降順）で並べ替えます。
func sortDesc(jobIDs []string, sortKey SortKeyFunc) {
	if sortKey == nil {
		slices.Sort(jobIDs)
		slices.Reverse(jobIDs)
		return
	}

	slices.SortFunc(jobIDs, func(a, b string) int {
		// キーの降順。同値なら ID の降順で安定させる。
		if c := cmp.Compare(sortKey(b), sortKey(a)); c != 0 {
			return c
		}
		return cmp.Compare(b, a)
	})
}

// AdjustItemCount は、実際に取得できた件数に合わせて From/To を補正します。
//
// 一覧は ID を並べてからメタデータ本体を読みにいくため、一部の読み込みに失敗すると
// 表示件数が SelectIDs の想定より少なくなります。「1〜10 件目を表示」と出しながら
// 8 件しか並ばない、というズレを防ぐために使います。
func AdjustItemCount(meta PageMeta, itemCount int) PageMeta {
	if meta.Total == 0 {
		return meta
	}
	if itemCount == 0 {
		meta.From = 0
		meta.To = 0
		return meta
	}
	if meta.PerPage > 0 {
		meta.To = meta.From + itemCount - 1
		return meta
	}
	meta.To = itemCount
	return meta
}
