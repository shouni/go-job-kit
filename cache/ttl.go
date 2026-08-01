// Package cache は、ジョブ ID をキーとした TTL 付きインメモリキャッシュを提供します。
//
// 履歴一覧はジョブ ID を並べてから 1 件ずつメタデータを読みにいくため、ページを
// めくるたびに同じオブジェクトを読み直します。それを避けるための薄いキャッシュです。
package cache

import (
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/shouni/go-utils/jobid"
)

// DefaultTTL は NewTTL に 0 以下を渡したときに使う保持期間です。
const DefaultTTL = 10 * time.Minute

// TTL は、ジョブ ID をキーとする TTL 付きキャッシュです。ゼロ値は使えません。
// Close を含め、複数のゴルーチンから同時に呼び出せます。
type TTL[T any] struct {
	cache *ttlcache.Cache[string, T]
	// closeOnce は、回収の停止をちょうど 1 回だけ行わせます。
	closeOnce sync.Once
}

// NewTTL はキャッシュを生成し、期限切れエントリを回収するゴルーチンを開始します。
//
// 回収を開始しないと、期限切れのエントリは同じキーが上書きされるまでメモリ上に
// 残り続けます。ジョブ ID をキーにする用途では新しいジョブのぶんだけ増え続けるため、
// 開始忘れがそのまま滞留につながります。呼び出し側の手順にせず生成時に開始し、
// 停止は Close に集約しています。
//
// 使い終わったら Close を呼んでください。
func NewTTL[T any](ttl time.Duration) *TTL[T] {
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	c := &TTL[T]{
		cache: ttlcache.New[string, T](
			ttlcache.WithTTL[string, T](ttl),
			// 参照のたびに期限が延びると、更新されない古い値がいつまでも残ります。
			// 保持期間は「最後に書いてから」で数えます。
			ttlcache.WithDisableTouchOnHit[string, T](),
		),
	}

	go c.cache.Start()

	return c
}

// Get はキャッシュされた値を返します。未キャッシュ・期限切れのときは ok が false です。
func (c *TTL[T]) Get(jobID string) (value T, ok bool) {
	item := c.cache.Get(key(jobID))
	if item == nil {
		var zero T
		return zero, false
	}
	return item.Value(), true
}

// Set は値をキャッシュへ保存します。
func (c *TTL[T]) Set(jobID string, value T) {
	c.cache.Set(key(jobID), value, ttlcache.DefaultTTL)
}

// Delete はキャッシュから値を削除します。
func (c *TTL[T]) Delete(jobID string) {
	c.cache.Delete(key(jobID))
}

// Len は保持しているエントリ数を返します。主にテスト・監視のためのものです。
func (c *TTL[T]) Len() int {
	return c.cache.Len()
}

// Close は回収用のゴルーチンを停止し、その終了まで待ちます。
//
// 何度呼んでも、複数のゴルーチンから同時に呼んでも安全です。停止処理を
// sync.Once に通しているのは、「まだ停止していないか確認してから停止する」形にすると
// 判定と実行の間に別のゴルーチンが割り込めるためです。
func (c *TTL[T]) Close() {
	c.closeOnce.Do(c.cache.Stop)
}

// key は、キャッシュキーに使うジョブ ID を正規化します。
//
// 同じジョブを指す "abc" と "dir/abc" が別エントリになると、更新したはずの値が
// 読まれないキャッシュミスになります。ストレージパスと同じ正規化を通して揃えます。
// 正規化できない値はキーとしてそのまま扱います（キャッシュの都合で呼び出しを
// 失敗させるより、キャッシュが効かないだけに留めるほうが安全なため）。
func key(jobID string) string {
	safe, err := jobid.Sanitize(jobID)
	if err != nil {
		return jobID
	}
	return safe
}
