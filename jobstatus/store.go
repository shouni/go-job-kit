package jobstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-utils/jobid"
)

// ErrNotFound は、ジョブ状態がまだ記録されていないことを表します。
//
// 「状態が無い」は正常な状態（記録前の投入や、この機能より前に作られたジョブ）なので、
// 呼び出し側がストレージ障害と区別できるよう独立したエラーにしています。
// HTTP ハンドラーはこれを 404 に、それ以外を 500 にマップしてください。
var ErrNotFound = errors.New("job status not found")

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

// Store は、リモートストレージを裏付けとしたジョブ状態の読み書きを行います。
//
// 型引数 T にはアプリ固有の状態型（Status を埋め込んだ構造体）を指定します。
type Store[T any] struct {
	reader remoteio.Reader
	writer remoteio.OutputWriter
	locate Locator
	now    func() time.Time
}

// NewStore は Store を構築します。
//
// reader / writer は remoteio.InputReader / remoteio.OutputWriter をそのまま渡せます。
func NewStore[T any](reader remoteio.Reader, writer remoteio.OutputWriter, locate Locator) *Store[T] {
	return &Store[T]{
		reader: reader,
		writer: writer,
		locate: locate,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Save はジョブ状態を保存します。
//
// 状態ファイルは常に最新の 1 世代だけを保持し、上書きで更新します。
// status が Status を埋め込んでいれば、JobID と UpdatedAt はここで打刻されます
// （引数の値は変更しません）。
func (s *Store[T]) Save(ctx context.Context, jobID string, status T) error {
	uri, safeJobID, err := s.resolve(jobID)
	if err != nil {
		return err
	}
	if s.writer == nil {
		return errors.New("jobstatus: writer is not configured")
	}

	if stamper, ok := any(&status).(Stamper); ok {
		stamper.Stamp(safeJobID, s.now())
	}

	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("jobstatus: marshal (%s): %w", safeJobID, err)
	}

	if err := s.writer.Write(ctx, uri, bytes.NewReader(data),
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
func (s *Store[T]) Get(ctx context.Context, jobID string) (T, error) {
	var status T

	uri, safeJobID, err := s.resolve(jobID)
	if err != nil {
		return status, err
	}
	if s.reader == nil {
		return status, errors.New("jobstatus: reader is not configured")
	}

	rc, err := s.reader.Open(ctx, uri)
	if err != nil {
		// remoteio は「未存在」を型付きで返さないため、読めなかった時点で未記録とみなします。
		// 状態の欠落で処理を止めるより、記録が無いものとして先へ進めるほうが安全です。
		//
		// ただし原因は捨てずに包みます。「未存在」と「権限不足・ストレージ障害」がどちらも
		// ErrNotFound になる以上、後者を切り分ける手がかりがログにも残らないと、
		// 状態が出ない原因の調査が総当たりになるためです。
		return status, fmt.Errorf("%w: %s: %w", ErrNotFound, safeJobID, err)
	}
	defer func() { _ = rc.Close() }()

	if err := json.NewDecoder(rc).Decode(&status); err != nil {
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
	if s.writer == nil {
		return nil
	}
	return s.writer.Delete(ctx, uri)
}

// resolve はジョブ ID を正規化し、状態ファイルの URI を組み立てます。
//
// ジョブ ID は URL パスとストレージパスの双方に現れるため、正規化はセキュリティ境界を
// 兼ねます。呼び出し側が忘れられるようにしないため、Store が必ずここを通します。
func (s *Store[T]) resolve(jobID string) (uri string, safeJobID string, err error) {
	safeJobID, err = jobid.Sanitize(jobID)
	if err != nil {
		return "", "", fmt.Errorf("jobstatus: %w", err)
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
