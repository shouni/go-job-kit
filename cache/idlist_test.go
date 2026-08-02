package cache_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shouni/go-job-kit/cache"
)

const testPrefix = "gs://bucket/jobs/"

// 一覧走査はバケット全体の List になるため、2 回目以降は走査せずに返すこと。
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
		t.Errorf("collect の呼び出し回数 = %d, want 1", calls)
	}
}

// キャッシュ本体を渡すと、受け取った側の並べ替えが他の並行リクエストへ波及する。
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
		t.Errorf("2 回目 = %v。1 回目の並べ替えがキャッシュへ波及している", second)
	}
}

// 削除・追加が TTL 分だけ画面に反映されない、という事態を防げること。
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
		t.Errorf("collect の呼び出し回数 = %d, want 2（破棄後は再走査）", calls)
	}
}

// 走査に失敗した結果はキャッシュせず、次の呼び出しでやり直すこと。
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
		t.Errorf("collect の呼び出し回数 = %d, want 2", calls)
	}
}

// 参照のたびに期限が延びると、新しいジョブがいつまでも一覧に現れない。
func TestIDListExpires(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(30 * time.Millisecond)
	var calls int
	collect := func(context.Context) ([]string, error) {
		calls++
		return []string{"job-a"}, nil
	}

	if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if calls != 2 {
		t.Errorf("collect の呼び出し回数 = %d, want 2（期限切れ後は再走査）", calls)
	}
}

// キャッシュを任意機能として組み込めること（nil でも走査は成立する）。
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

// 履歴画面は複数リクエストから同時に開かれる。
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
			slices.Sort(got) // 受け取った側での書き換えが他へ波及しないこと
			if i%10 == 0 {
				list.Invalidate(testPrefix)
			}
		})
	}
	wg.Wait()
}

// 既定の TTL が使われること（0 以下は DefaultIDListTTL へ丸める）。
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
