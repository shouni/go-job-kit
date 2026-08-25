package cache_test

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shouni/go-job-kit/cache"
)

func TestSetGetDelete(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	if _, ok := c.Get("job-1"); ok {
		t.Error("未設定のキーが取得できている")
	}

	c.Set("job-1", "value")
	got, ok := c.Get("job-1")
	if !ok || got != "value" {
		t.Errorf("Get() = %q, %v; want \"value\", true", got, ok)
	}

	c.Delete("job-1")
	if _, ok := c.Get("job-1"); ok {
		t.Error("削除後も取得できている")
	}
}

// ストレージパスと同じ正規化をキーにも通すこと。
// 揃っていないと、"dir/job-1" で書いた値を "job-1" で読めず、
// 更新したはずの履歴が古いまま表示されます。
func TestKeyIsNormalizedLikeStoragePath(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	c.Set("../../job-1", "value")

	got, ok := c.Get("job-1")
	if !ok || got != "value" {
		t.Errorf("Get(\"job-1\") = %q, %v; want \"value\", true", got, ok)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d, want 1（正規化されず別エントリになっている）", c.Len())
	}
}

// 正規化できない値でも、キャッシュとして使えないだけで落ちないこと。
func TestUnnormalizableKeyStillUsable(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	defer c.Close()

	c.Set("日本語", "value")
	if got, ok := c.Get("日本語"); !ok || got != "value" {
		t.Errorf("Get() = %q, %v; want \"value\", true", got, ok)
	}
}

// 期限切れの値を返さないこと。
func TestExpiredEntryIsNotReturned(t *testing.T) {
	t.Parallel()

	// バブル内の time.Sleep は仮想時間を進めるだけなので、保持期間の経過を
	// 実時間を待たずに再現できます。
	synctest.Test(t, func(t *testing.T) {
		c := cache.NewTTL[string](20 * time.Millisecond)
		defer c.Close()

		c.Set("job-1", "value")
		// Sleep だけでは回収ゴルーチンが走った保証がありません。
		// synctest.Sleep は時計を進めたうえで、ほかが落ち着くまで待ちます。
		synctest.Sleep(60 * time.Millisecond)

		if _, ok := c.Get("job-1"); ok {
			t.Error("期限切れの値が返っている")
		}
	})
}

// 期限切れエントリが回収されること。
// 回収を開始し忘れると、ジョブ ID をキーにする用途では増え続けます。
func TestExpiredEntryIsEvicted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c := cache.NewTTL[string](20 * time.Millisecond)
		defer c.Close()

		c.Set("job-1", "value")

		// 回収は NewTTL が起動したゴルーチンが行います。Sleep で保持期間を過ぎさせ、
		// Wait でその回収が終わるまで待ちます。「いつかは回収されるはず」と
		// 期限を切ってポーリングする必要はありません。
		synctest.Sleep(30 * time.Millisecond)

		if got := c.Len(); got != 0 {
			t.Errorf("Len() = %d, want 0（期限切れエントリが回収されていない）", got)
		}
	})
}

func TestNonPositiveTTLFallsBackToDefault(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](0)
	defer c.Close()

	c.Set("job-1", "value")
	if _, ok := c.Get("job-1"); !ok {
		t.Error("既定の TTL が適用されていない")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)
	c.Close()
	c.Close()
}

// 複数のゴルーチンから同時に Close しても安全なこと。
// 「停止済みか確認してから停止する」実装だと、判定と実行の間に割り込まれて
// 二重停止になります。逐次の二重 Close ではこれを検出できません。
func TestConcurrentCloseIsSafe(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](time.Minute)

	const goroutines = 16
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start // 同時に踏ませる
			c.Close()
		}()
	}

	close(start)
	wg.Wait()
}
