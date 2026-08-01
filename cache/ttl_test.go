package cache_test

import (
	"testing"
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

	c := cache.NewTTL[string](20 * time.Millisecond)
	defer c.Close()

	c.Set("job-1", "value")
	time.Sleep(60 * time.Millisecond)

	if _, ok := c.Get("job-1"); ok {
		t.Error("期限切れの値が返っている")
	}
}

// 期限切れエントリが回収されること。
// 回収を開始し忘れると、ジョブ ID をキーにする用途では増え続けます。
func TestExpiredEntryIsEvicted(t *testing.T) {
	t.Parallel()

	c := cache.NewTTL[string](20 * time.Millisecond)
	defer c.Close()

	c.Set("job-1", "value")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Len() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Len() = %d, want 0（期限切れエントリが回収されていない）", c.Len())
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
