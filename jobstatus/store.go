package jobstatus

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-utils/jobid"
)

// Store が返す 3 つの分類です。いずれも原因を包んだまま返すので、errors.Is の
// 判定はそのままに、ログには元の失敗理由が残ります。
//
// HTTP ハンドラーは次のようにマップしてください。分類ごとに呼び出し側が取るべき
// 判断が違うため、まとめると必ずどちらかを取り違えます。
//
//	ErrInvalidJobID → 400  入力が不正。再試行しても直らない
//	ErrNotFound     → 404  未記録。処理を先へ進めてよい
//	ErrUnavailable  → 503  読めなかっただけ。あとで読めるかもしれない
//	その他          → 500  壊れた JSON など
var (
	// ErrNotFound は、ジョブ状態がまだ記録されていないことを表します。
	// 記録前の投入や、この機能より前に作られたジョブでも起こる正常な状態です。
	ErrNotFound = errors.New("job status not found")

	// ErrUnavailable は、ジョブ状態が「存在するはずなのに読めなかった」ことを
	// 表します。
	//
	// ErrNotFound と分けているのは、両者で取るべき判断が正反対だからです。記録が
	// 無いなら処理を進めてよい一方、読めなかっただけの場合に「無い」とみなすと、
	// 完了済みのジョブを未完了と誤認して生成をまるごとやり直します
	// （Recorder.AlreadySucceeded を参照）。
	//
	// 切り分けは remoteio が返す remoteio.ErrNotExist で行うため、追加のストレージ
	// 往復は要りません。
	ErrUnavailable = errors.New("job status unavailable")

	// ErrInvalidJobID は、渡されたジョブ ID が正規化を通らなかったことを表します。
	//
	// これが独立していないと、ハンドラーは URL に紛れ込んだ不正な ID をストレージ
	// 障害と同じ 5xx で返すことになり、再試行しても直らないリクエストを再試行
	// させます。原因は jobid のエラーとして残るので、errors.Is で jobid.ErrEmpty /
	// jobid.ErrTooLong / jobid.ErrInvalidFormat まで辿れます。
	ErrInvalidJobID = errors.New("invalid job id")
)

// contentType はジョブ状態 JSON の Content-Type です。
const contentType = "application/json; charset=utf-8"

// statusFile はジョブ状態を記録するオブジェクト名です。
const statusFile = "status.json"

// Locator は、正規化済みのジョブ ID から状態ファイルの URI を組み立てます。
//
// 渡されるジョブ ID は Store 側で jobid.Sanitize を通した後の値です。
// Locator の中で改めて検証する必要はありません。
type Locator func(jobID string) (string, error)

// UnderJobDir は、baseURI/{jobID}/status.json を指す Locator を返します。
//
// 成果物と同じジョブディレクトリ配下に置くため、履歴削除（プレフィックスの一括削除）で
// 状態ファイルも自動的に片付きます。
func UnderJobDir(baseURI string) Locator {
	base := strings.TrimRight(strings.TrimSpace(baseURI), "/")
	return func(jobID string) (string, error) {
		if base == "" {
			return "", errors.New("jobstatus: base URI is empty")
		}
		return base + "/" + jobID + "/" + statusFile, nil
	}
}

// Storage は Store が必要とする操作だけを表します。
// remoteio.Store がそのまま満たします。
//
// remoteio 側の複合インターフェースを直接受け取らないのは、このパッケージが
// 5 つのアプリから参照されるハブで、必要以上の操作を要求すると
// 呼び出し側のテストが使いもしない実装まで用意することになるためです。
type Storage interface {
	remoteio.Reader
	remoteio.Writer

	// Delete は状態ファイルを削除します。不在はエラーになりません。
	Delete(ctx context.Context, name string) error
}

// Store は、リモートストレージを裏付けとしたジョブ状態の読み書きを行います。
//
// 型引数 T にはアプリ固有の状態型（Status を埋め込んだ構造体）を指定します。
type Store[T any] struct {
	storage Storage
	locate  Locator
	now     func() time.Time
}

// mustNotBePointer は、型引数にポインタ型を渡す誤用を構築時に弾きます。
//
// Status の埋め込みは値型 T のメソッド集合を通して見つけます（Save は any(&status) を
// Stamper へアサートします）。T が *X だとアサート対象が **X になり、Stamper も Carrier も
// IsTerminal も満たしません。その結果、打刻されず・引き継がれず・再実行ガードが常に
// false になった状態ファイルが、エラーもログも無しに書かれます。
//
// Store[*X] は Go として自然に書けてしまい、しかも壊れ方が静かなので、データを
// 書く前に落とします。埋め込みの無い型（T = X で Stamper を満たさない）は doc の
// とおり引き続き許容します。そちらは打刻も引き継ぎも行われないだけで、誤って
// 静かに壊れるわけではないためです。
func mustNotBePointer[T any](fn string) {
	if t := reflect.TypeFor[T](); t.Kind() == reflect.Pointer {
		panic(fmt.Sprintf(
			"jobstatus: %s[%s]: 型引数にポインタ型を渡さないでください。"+
				"打刻・引き継ぎ・再実行ガードが無効になります（値型で instantiate してください）", fn, t))
	}
}

// NewStore は Store を構築します。
//
// storage には remoteio.Store をそのまま渡せます。以前は読み込みと書き込みを
// 別々の引数で受けていましたが、remoteio 側で 1 つの窓口に畳まれたため
// ここも 1 つにしています。
//
// 型引数にポインタ型を渡した場合は panic します（mustNotBePointer を参照）。
func NewStore[T any](storage Storage, locate Locator) *Store[T] {
	mustNotBePointer[T]("NewStore")

	return &Store[T]{
		storage: storage,
		locate:  locate,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Save はジョブ状態を保存します。
//
// 状態ファイルは常に最新の 1 世代だけを保持し、上書きで更新します。
// status が Status を埋め込んでいれば、JobID と UpdatedAt はここで打刻されます
// （引数の値は変更しません）。
//
// 書き込みには Cache-Control: no-store を付けます。状態は頻繁に変わるため、CDN や
// ブラウザにキャッシュさせると古い状態を返し続けることになるためです。
//
// エンコードは Get と揃えて encoding/json/v2 を使います。v1 と違い &・<・> を
// \u00XX へ逃がさないため、題目や失敗理由を含む status.json がそのまま読めます
// （JSON としては同値で、フィールドの並びも埋め込みの展開も v1 と一致します）。
func (s *Store[T]) Save(ctx context.Context, jobID string, status T) error {
	uri, safeJobID, err := s.resolve(jobID)
	if err != nil {
		return err
	}
	if s.storage == nil {
		return errors.New("jobstatus: storage is not configured")
	}

	if stamper, ok := any(&status).(Stamper); ok {
		stamper.Stamp(safeJobID, s.now())
	}

	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("jobstatus: marshal (%s): %w", safeJobID, err)
	}

	if err := s.storage.Write(ctx, uri, bytes.NewReader(data),
		remoteio.WithContentType(contentType),
		// 状態は頻繁に変わるため、CDN・ブラウザにキャッシュさせない。
		remoteio.WithCacheControl("no-store"),
	); err != nil {
		return fmt.Errorf("jobstatus: write (%s): %w", uri, err)
	}
	return nil
}

// Get はジョブ状態を取得します。未記録の場合は ErrNotFound を返します。
//
// 壊れた JSON は未記録ではなくデコード失敗として返します。未記録と同じ扱いにすると、
// 破損に気づかないまま再生成が走り続けるためです。
//
// デコードは encoding/json/v2 の UnmarshalRead で行います。JSON 値のあとに残った
// バイト列と、重複したキーを拒否するためです。従来の Decoder はどちらも黙って
// 受け取っていました。とくに重複キーは後勝ちで、途中で切れた書き込みに次の書き込みが
// 続いた status.json が {"state":"succeeded",...,"state":"running"} の形になると、
// running として読めてしまいます。これは再実行ガードが防いでいるはずの巻き戻しが、
// 記録ではなく読み取りの側から入ってくる経路です。
func (s *Store[T]) Get(ctx context.Context, jobID string) (T, error) {
	var status T

	uri, safeJobID, err := s.resolve(jobID)
	if err != nil {
		return status, err
	}
	if s.storage == nil {
		return status, errors.New("jobstatus: storage is not configured")
	}

	rc, err := s.storage.Open(ctx, uri)
	if err != nil {
		// remoteio は未存在を remoteio.ErrNotExist に包んで返します（GCS は
		// storage.ErrObjectNotExist、S3 は NoSuchKey、ローカルは os.Open がそのまま）。
		// これで「記録が無い」と「あるのに読めない」を追加の往復なしに切り分けられます。
		if errors.Is(err, remoteio.ErrNotExist) {
			return status, fmt.Errorf("%w: %s: %w", ErrNotFound, safeJobID, err)
		}
		// 権限不足・ストレージ障害・ネットワーク断。「無い」と誤認すると、完了済みの
		// ジョブを未完了とみなして生成をやり直すため、別のエラーとして返します。
		return status, fmt.Errorf("%w: %s: %w", ErrUnavailable, safeJobID, err)
	}
	defer func() { _ = rc.Close() }()

	if err := json.UnmarshalRead(rc, &status); err != nil {
		var zero T
		return zero, fmt.Errorf("jobstatus: decode (%s): %w", safeJobID, err)
	}

	if stamper, ok := any(&status).(Stamper); ok {
		stamper.EnsureJobID(safeJobID)
	}
	return status, nil
}

// Delete はジョブ状態を削除します。履歴削除に追随させるために使います。
//
// writer が未設定のときは何もしません（記録できていない以上、消すものも無いため）。
func (s *Store[T]) Delete(ctx context.Context, jobID string) error {
	uri, _, err := s.resolve(jobID)
	if err != nil {
		return err
	}
	if s.storage == nil {
		return nil
	}
	if err := s.storage.Delete(ctx, uri); err != nil {
		return fmt.Errorf("jobstatus: delete (%s): %w", uri, err)
	}
	return nil
}

// resolve はジョブ ID を正規化し、状態ファイルの URI を組み立てます。
//
// ジョブ ID は URL パスとストレージパスの双方に現れるため、正規化はセキュリティ境界を
// 兼ねます。呼び出し側が忘れられるようにしないため、Store が必ずここを通します。
func (s *Store[T]) resolve(jobID string) (uri string, safeJobID string, err error) {
	safeJobID, err = jobid.Sanitize(jobID)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrInvalidJobID, err)
	}
	if s.locate == nil {
		return "", "", errors.New("jobstatus: locator is not configured")
	}
	uri, err = s.locate(safeJobID)
	if err != nil {
		return "", "", fmt.Errorf("jobstatus: locate (%s): %w", safeJobID, err)
	}
	return uri, safeJobID, nil
}
