package cache

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/sync/singleflight"
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
	// flight は、同じキーへの Load が同時にキャッシュミスしたときに
	// 走査を 1 回にまとめます（Load を参照）。
	flight singleflight.Group
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
// 同じ key への Load が同時にキャッシュミスした場合、走査は 1 回だけ実行され、
// 全員がその結果を共有します。一覧走査はプレフィックス配下全体の List なので、
// TTL が切れた瞬間に重なったリクエストの数だけ走らせないためです。
// その共有の中でも、各呼び出しは自分の ctx にだけ従います。
//
//   - 待っている間に自分の ctx が終われば、その呼び出しは ctx.Err() で抜けます。
//     走査そのものは続き、完了すればキャッシュを温めます。
//   - 走査を始めた呼び出しの ctx が先に終わった場合、待っていた呼び出しは
//     巻き込まれず、自分の ctx で走査をやり直します。
//
// 返すのは常に複製です。キャッシュ本体をそのまま渡すと、受け取った側での並べ替えが
// 他の並行リクエストへ波及します（paging.SelectIDs は引数を変更しませんが、
// 呼び出し側が自前で並べ替える場合に効いてきます）。
func (c *IDList) Load(ctx context.Context, key string, collect func(context.Context) ([]string, error)) ([]string, error) {
	if c == nil || c.cache == nil {
		return collect(ctx)
	}

	for retried := false; ; retried = true {
		if cached := c.cache.Get(key); cached != nil {
			return slices.Clone(cached.Value()), nil
		}

		// singleflight は同じキーの飛行中、最初の呼び出しの関数だけを実行します。
		// つまりこの closure の ctx は、走査を実際に始めた呼び出しのものです。
		ch := c.flight.DoChan(key, func() (any, error) {
			jobIDs, err := collect(ctx)
			if err != nil {
				return nil, err
			}
			c.cache.Set(key, jobIDs, ttlcache.DefaultTTL)
			return jobIDs, nil
		})

		select {
		case <-ctx.Done():
			// 走査は放棄しません。完了すればキャッシュに入り、次の呼び出しが使えます。
			return nil, ctx.Err()
		case res := <-ch:
			if res.Err == nil {
				return slices.Clone(res.Val.([]string)), nil
			}
			// 走査を始めた側のキャンセルに巻き込まれただけなら、自分がやり直します。
			// やり直しは 1 回まで。collect が自前の期限で ctx エラーを返し続ける場合に
			// ループしないためです（そのときは 2 回目のエラーをそのまま返します）。
			if !retried && ctx.Err() == nil && isContextError(res.Err) {
				continue
			}
			return nil, res.Err
		}
	}
}

// isContextError は、キャンセル・期限切れ由来のエラーかどうかを返します。
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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
