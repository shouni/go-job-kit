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
	"time"

	"github.com/shouni/go-job-kit/paging"
)

func discardLogger() paging.Option {
	return paging.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 並行に読み込んでも、SelectIDs が返した新しい順がそのまま保たれること。
func TestLoadPageKeepsSelectionOrder(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-05", "job-03", "job-04", "job-02"}
	items, meta, err := paging.LoadPage(context.Background(), jobIDs, 1, 3,
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

// ソートキーは SelectIDs と同じオプションで渡せること。
func TestLoadPageWithSortKey(t *testing.T) {
	t.Parallel()

	// {用途}-{時刻} 形式。ID の文字列比較では用途プレフィックス順になってしまう。
	jobIDs := []string{"mv-300", "recipe-100", "short-200"}
	items, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 3,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil },
		paging.WithSortKey(func(jobID string) string {
			_, ts, _ := strings.Cut(jobID, "-")
			return ts
		}))
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	want := []string{"mv-300", "short-200", "recipe-100"}
	if !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
}

// 1 件の破損や競合する削除で、ページ全体を失わせないこと。
// 表示件数もその分だけ補正されること。
func TestLoadPageSkipsFailedItems(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-02", "job-03", "job-04"}
	items, meta, err := paging.LoadPage(context.Background(), jobIDs, 1, 4,
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
		t.Errorf("Total = %d, want 4（一覧の総数は読み込み結果で変わらない）", meta.Total)
	}
	if meta.To != 3 {
		t.Errorf("To = %d, want 3（実際に並んだ件数へ補正）", meta.To)
	}
}

// 読み込みが軒並み失敗した結果の「0 件」を、正常な空一覧と取り違えないこと。
func TestLoadPageReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := paging.LoadPage(ctx, []string{"job-01", "job-02"}, 1, 2,
		func(ctx context.Context, jobID string) (string, error) {
			return jobID, ctx.Err()
		}, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadPage() error = %v, want context.Canceled", err)
	}
}

// 一部の読み込み失敗を一覧へ残したい場合は、load 側で代替値を返せること。
func TestLoadPageWithFallbackInsideLoad(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), []string{"job-01", "job-02"}, 1, 2,
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

// 同時実行数の上限を超えないこと（ストレージへの同時読み取りを抑えるため）。
func TestLoadPageLimitsConcurrency(t *testing.T) {
	t.Parallel()

	jobIDs := make([]string, 50)
	for i := range jobIDs {
		jobIDs[i] = fmt.Sprintf("job-%02d", i)
	}

	var inFlight, peak atomic.Int64
	_, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 0,
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
		t.Errorf("同時実行数の最大 = %d, want <= 4", got)
	}
}

// 全ての取得枠が塞がっている間にキャンセルされたら、空いた枠で次の読み込みを
// 始めないこと（枠待ちは ctx を見る）。
func TestLoadPageStopsAcquiringAfterCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	// ctx を無視して塞がる読み込み。1 件目が枠を占有し続ける。
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
		_, _, err = paging.LoadPage(ctx, []string{"job-01", "job-02"}, 1, 10, load,
			paging.WithConcurrency(1), discardLogger())
	}()

	<-started // 1 件目が枠を取ってから
	cancel()
	// キャンセル後、枠待ちの唯一の出口は ctx.Done なので、release の前に
	// 2 件目の取得待ちは抜けている。ここで枠を空けても 2 件目は始まらないこと。
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadPage() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("load の呼び出し回数 = %d, want 1（キャンセル後に次を読み始めない）", got)
	}
}

// perPage が 0 以下ならページングせず全件を読み込むこと（SelectIDs と同じ）。
func TestLoadPageWithoutPaging(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), []string{"job-01", "job-02"}, 1, 0,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil })
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if len(items) != 2 {
		t.Errorf("items = %v, want 2 件", items)
	}
	if meta.TotalPages != 1 || meta.To != 2 {
		t.Errorf("meta = %+v", meta)
	}
}

func TestLoadPageWithNoJobs(t *testing.T) {
	t.Parallel()

	items, meta, err := paging.LoadPage(context.Background(), nil, 1, 10,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil })
	if err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if len(items) != 0 {
		t.Errorf("items = %v, want 空", items)
	}
	if meta.Total != 0 || meta.From != 0 || meta.To != 0 {
		t.Errorf("meta = %+v", meta)
	}
}

// 引数のスライスは変更しないこと（SelectIDs と同じ約束）。
func TestLoadPageDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	jobIDs := []string{"job-01", "job-03", "job-02"}
	original := slices.Clone(jobIDs)

	if _, _, err := paging.LoadPage(context.Background(), jobIDs, 1, 2,
		func(_ context.Context, jobID string) (string, error) { return jobID, nil }); err != nil {
		t.Fatalf("LoadPage() error = %v", err)
	}

	if !slices.Equal(jobIDs, original) {
		t.Errorf("引数が変更されている: %v, want %v", jobIDs, original)
	}
}
