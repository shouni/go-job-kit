// Package paging は、ジョブ ID の一覧を新しい順に切り出し、画面表示に必要な
// ページメタデータを組み立てます。
//
// ページ番号は 1 始まりです。perPage が 0 以下のときはページングせず全件を返します。
package paging

import (
	"cmp"
	"log/slog"
	"slices"
)

// PageMeta は、一覧画面がページネーションを描画するために必要なメタデータです。
//
// JSON タグは移植元のサービスが返している既存のレスポンスと同じ形です。
// 画面と M2M クライアントの双方が依存しているため、変更するときは利用側の追随が要ります。
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
// 戻り値の降順が「新しい順」になるよう実装してください（選び方は SelectIDs を参照）。
type SortKeyFunc func(jobID string) string

type options struct {
	concurrency int
	logger      *slog.Logger
}

// Option は LoadPage の挙動を変更します。
// SelectIDs は読み込みを行わないため、いずれのオプションも影響しません。
type Option func(*options)

// SelectIDs は jobIDs を新しい順に並べ替え、指定ページ分を切り出します。
//
// sortKey は並べ替えに使うキーの取り出し方で、呼び出し側が必ず選びます。
// オプションではなく引数なのは、既定を置くと間違った既定のまま呼べてしまうからです。
// ここで誤っても一覧は正常に返り、順序だけが静かに崩れます。
//
//   - 時刻を埋め込んだ ID なら go-utils の jobid.SortKey を渡してください。
//     用途ごとに異なるプレフィックスを付けた ID が同じ一覧に混在する場合や、
//     採番の形式が途中で変わった場合、ID の文字列比較は時刻ではなくプレフィックスの
//     順になります（時刻部分より前に差が出るため）。移植元のアプリはいずれも
//     この状態にあります。
//   - nil を渡すと ID そのものの降順で並べます。ID が単一のプレフィックスを共有し、
//     その直後に時刻が来る場合にだけ正しい並びになります。
//
// キーが空文字を返した ID は末尾に回ります（降順で空文字が最小になるため）。
// キーが同値のときは ID の降順で安定させます。
//
// 引数のスライスは変更しません（並べ替えは複製に対して行います）。
// page が範囲外のときは最終ページへ丸めます。
func SelectIDs(jobIDs []string, page int, perPage int, sortKey SortKeyFunc) ([]string, PageMeta) {
	if page < 1 {
		page = 1
	}

	sorted := make([]string, len(jobIDs))
	copy(sorted, jobIDs)
	sortDesc(sorted, sortKey)

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
