package cache

import (
	"context"
	"slices"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

// DefaultIDListTTL は NewIDList に 0 以下を渡したときに使う保持期間です。
//
// メタデータ本体（DefaultTTL）より大幅に短いのは、新しく完成したジョブが一覧に
// 現れるまでの遅延を最小にするためです。削除時は Invalidate で明示的に破棄するので、
// 削除操作が TTL 分だけ画面に反映されない、という事態は起きません。
const DefaultIDListTTL = time.Minute

// IDList は、一覧走査で得たジョブ ID を短時間だけ保持するキャッシュです。
// ゼロ値は使えません。複数のゴルーチンから同時に呼び出せます。
//
// メタデータ本体のキャッシュ（TTL）と違い、これが無いと履歴画面を開くたびに
// プレフィックス配下全体の List が走ります。
//
// TTL とは違い、期限切れエントリを回収するゴルーチンは開始しません（したがって
// Close もありません）。保持するのは一覧の単位ごとに 1 エントリだけで、同じキーへの
// 書き込みで上書きされていくためです。逆に言えば、キーには一覧の単位（走査対象の
// プレフィックスなど）だけを使ってください。ジョブ ID のように増え続ける値をキーに
// すると、期限切れのエントリが回収されないまま溜まります。
type IDList struct {
	cache *ttlcache.Cache[string, []string]
}

// NewIDList はジョブ ID 一覧用のキャッシュを生成します。
func NewIDList(ttl time.Duration) *IDList {
	if ttl <= 0 {
		ttl = DefaultIDListTTL
	}

	return &IDList{
		cache: ttlcache.New[string, []string](
			ttlcache.WithTTL[string, []string](ttl),
			// 参照のたびに期限が延びると、新しいジョブがいつまでも一覧に現れません。
			ttlcache.WithDisableTouchOnHit[string, []string](),
		),
	}
}

// Load は key に対応するジョブ ID 一覧を返します。
// キャッシュに無い（あるいは期限切れの）ときだけ collect を呼び、結果を保持します。
//
// レシーバが nil のときは collect をそのまま呼びます。キャッシュを任意機能として
// 組み込めるようにするためのものです。
//
// 返すのは常に複製です。キャッシュ本体をそのまま渡すと、受け取った側での並べ替えが
// 他の並行リクエストへ波及します（paging.SelectIDs は引数を変更しませんが、
// 呼び出し側が自前で並べ替える場合に効いてきます）。
func (c *IDList) Load(ctx context.Context, key string, collect func(context.Context) ([]string, error)) ([]string, error) {
	if c == nil || c.cache == nil {
		return collect(ctx)
	}

	if cached := c.cache.Get(key); cached != nil {
		return slices.Clone(cached.Value()), nil
	}

	jobIDs, err := collect(ctx)
	if err != nil {
		return nil, err
	}

	c.cache.Set(key, jobIDs, ttlcache.DefaultTTL)

	return slices.Clone(jobIDs), nil
}

// Invalidate は一覧キャッシュを破棄し、ジョブの削除や追加を即座に反映させます。
func (c *IDList) Invalidate(key string) {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.Delete(key)
}

// Len は保持しているエントリ数を返します。主にテスト・監視のためのものです。
func (c *IDList) Len() int {
	if c == nil || c.cache == nil {
		return 0
	}
	return c.cache.Len()
}
