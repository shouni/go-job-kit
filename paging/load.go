package paging

import (
	"context"
	"log/slog"
	"sync"
)

// DefaultConcurrency は、1 ページ分のメタデータを読み込むときの既定の同時実行数です。
const DefaultConcurrency = 10

// WithConcurrency は、1 ページ分のメタデータを読み込む同時実行数を変更します。
// 0 以下を渡したときは DefaultConcurrency を使います。LoadPage でのみ効きます。
func WithConcurrency(n int) Option {
	return func(o *options) { o.concurrency = n }
}

// WithLogger は、読み込みに失敗した ID を書き出すロガーを差し替えます。
// 既定は slog.Default() です。LoadPage でのみ効きます。
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// LoadPage は、ページを切り出してから、その ID 分のメタデータを並行に読み込みます。
//
// 一覧は「ジョブ ID を並べる」と「1 件ずつメタデータを読む」の 2 段構えになります。
// 後段は 1 ページぶんとはいえ数十回のストレージ読み取りになるため直列にはできず、
// かといって並行にすると順序が崩れます。その組み立てを 1 か所に集めたものです。
//
//	items, meta, err := paging.LoadPage(ctx, jobIDs, page, perPage, jobid.SortKey, repo.loadHistory)
//
// sortKey の選び方は SelectIDs を参照してください（並べ替えはそちらへ委ねます）。
//
// 振る舞いは次のとおりです。
//
//   - 並び順は SelectIDs が返した順（新しい順）をそのまま保ちます。
//   - load が返したエラーは警告ログに残し、その ID を一覧から取り除きます。
//     1 件の破損や競合する削除で、ページ全体を失わせないためです。
//   - ctx のキャンセル・期限切れはエラーとして返します。読み込みが軒並み失敗した結果
//     「0 件のページ」を返してしまうと、呼び出し側が正常な空一覧と区別できません。
//   - PageMeta は実際に読み込めた件数へ補正済みです（AdjustItemCount 相当）。
//
// load 失敗時に ID を落とさず代替要素を並べたい場合は、load の中でフォールバック値を
// 返してください（メッセージや代替値の形はサービスごとに違うため、ここでは持ちません）。
//
//	load := func(ctx context.Context, jobID string) (History, error) {
//	    h, err := repo.load(ctx, jobID)
//	    if err != nil {
//	        return History{JobID: jobID, Title: jobID}, nil // 一覧には残す
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

	// 添字へ書き込むことで、ロックを持たずに並行して読みつつ ID の並び順をそのまま
	// 維持できます。読めなかった要素は nil のまま残し、後段で取り除きます。
	loaded := make([]*T, len(selectedIDs))
	sem := make(chan struct{}, cfg.concurrency)
	var wg sync.WaitGroup

loop:
	for i, jobID := range selectedIDs {
		if ctx.Err() != nil {
			break
		}

		// 取得枠は goroutine の外で確保します。中で待たせると、同時に読むのは
		// 上限どおりでも goroutine だけはページ全件分が一度に立ちます。
		// 枠待ちの間もキャンセルを見ます。見ないと、全ての枠が塞がっている間に
		// キャンセルされても待ち続け、空いた枠でもう 1 件読み始めてしまいます。
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
