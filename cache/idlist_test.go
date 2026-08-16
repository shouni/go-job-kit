package cache_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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

// 同時のキャッシュミスでは走査を 1 回にまとめること。一覧走査はプレフィックス配下
// 全体の List なので、TTL が切れた瞬間に重なったリクエストの数だけ走らせない。
func TestIDListConcurrentMissRunsCollectOnce(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	collect := func(context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []string{"job-b", "job-a"}, nil
	}

	const waiters = 20
	results := make([][]string, waiters)
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := range waiters {
		wg.Go(func() {
			results[i], errs[i] = list.Load(context.Background(), testPrefix, collect)
		})
	}

	<-entered // 走査が始まるのを待ってから、全員をまとめて解放する
	close(release)
	wg.Wait()

	for i := range waiters {
		if errs[i] != nil {
			t.Fatalf("Load()[%d] error = %v", i, errs[i])
		}
		if !slices.Equal(results[i], []string{"job-b", "job-a"}) {
			t.Fatalf("Load()[%d] = %v", i, results[i])
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("collect の呼び出し回数 = %d, want 1（同時ミスは 1 回の走査にまとめる）", got)
	}
}

// 待っている呼び出しは自分の ctx にだけ従うこと。走査そのものは放棄せず、
// 完了すればキャッシュを温める。
func TestIDListWaiterHonorsOwnContext(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	collect := func(context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []string{"job-a"}, nil
	}

	// 走査を始める側は塞がったまま。
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, err := list.Load(context.Background(), testPrefix, collect); err != nil {
			t.Errorf("走査側の Load() error = %v", err)
		}
	}()
	<-entered

	// 待ち側は自分の ctx が終わった時点で抜ける（走査の完了を待たされない）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := list.Load(ctx, testPrefix, collect); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v, want context.Canceled", err)
	}

	close(release)
	<-leaderDone

	// 抜けたあとも走査は完了しており、キャッシュから読めること。
	got, err := list.Load(context.Background(), testPrefix, collect)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(got, []string{"job-a"}) {
		t.Errorf("Load() = %v", got)
	}
	if callCount := calls.Load(); callCount != 1 {
		t.Errorf("collect の呼び出し回数 = %d, want 1（走査は放棄されない）", callCount)
	}
}

// 走査を始めた呼び出しがキャンセルされても、待っていた呼び出しは巻き込まれず、
// 自分の ctx で走査をやり直すこと。
func TestIDListRetriesAfterLeaderCancel(t *testing.T) {
	t.Parallel()

	list := cache.NewIDList(time.Minute)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	entered := make(chan struct{})
	var calls atomic.Int32
	collect := func(ctx context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done() // 最初の走査はキャンセルされるまで塞がり、そのまま失敗する
			return nil, ctx.Err()
		}
		return []string{"job-a"}, nil
	}

	leaderErr := make(chan error, 1)
	go func() {
		_, err := list.Load(leaderCtx, testPrefix, collect)
		leaderErr <- err
	}()
	<-entered

	waiterDone := make(chan struct{})
	var waiterIDs []string
	var waiterErr error
	go func() {
		defer close(waiterDone)
		waiterIDs, waiterErr = list.Load(context.Background(), testPrefix, collect)
	}()

	// 待ち側が走査へ合流したかどうかは外から観測できないため、少し待ってから
	// キャンセルする。合流前だった場合も、待ち側が自分で走査を始めるだけで、
	// 検証する振る舞い（巻き込まれずに結果を得る・走査は計 2 回）は変わらない。
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	<-waiterDone
	if waiterErr != nil {
		t.Fatalf("待ち側の Load() error = %v, want nil（他人のキャンセルに巻き込まれない）", waiterErr)
	}
	if !slices.Equal(waiterIDs, []string{"job-a"}) {
		t.Errorf("待ち側の Load() = %v", waiterIDs)
	}
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("走査側の Load() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("collect の呼び出し回数 = %d, want 2（失敗した走査 + やり直し）", got)
	}
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
