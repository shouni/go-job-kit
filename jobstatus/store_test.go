package jobstatus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shouni/go-remote-io/remoteio"
	"github.com/shouni/go-remote-io/remoteio/memio"

	"github.com/shouni/go-job-kit/jobstatus"
)

const testJobID = "c20260726-120000-abcd1234"

const testBaseURI = "gs://bucket/jobs"

const testStatusPath = testBaseURI + "/" + testJobID + "/status.json"

// appStatus は、利用側がサービス固有のフィールドを足す典型例です。
type appStatus struct {
	jobstatus.Status
	OutputDir string `json:"output_dir,omitempty"`
}

func newStore(store *memStore) *jobstatus.Store[appStatus] {
	return jobstatus.NewStore[appStatus](store, jobstatus.UnderJobDir(testBaseURI))
}

// 状態は成果物と同じジョブディレクトリ配下に置き、履歴削除（プレフィックス一括削除）で
// 自動的に片付くようにする。
func TestSaveWritesInsideJobDirectory(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, appStatus{
		Command: "generate", State: jobstatus.StateQueued,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if !store.has(testStatusPath) {
		t.Fatalf("status.json が %q に書かれていない。書き込み済み: %v", testStatusPath, store.keys())
	}
}

func TestSaveAndGetRoundTrip(t *testing.T) {
	t.Parallel()

	store := newStore(newMemStore())
	original := appStatus{
		Command:   "generate",
		State:     jobstatus.StateSucceeded,
		Title:     "テスト作品",
		Attempts:  3,
		OutputDir: testBaseURI + "/" + testJobID,
	}
	if err := store.Save(context.Background(), testJobID, original); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != jobstatus.StateSucceeded {
		t.Errorf("State = %q", got.State)
	}
	if got.Title != "テスト作品" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", got.Attempts)
	}
	if got.OutputDir != original.OutputDir {
		t.Errorf("OutputDir = %q", got.OutputDir)
	}
	// Save 側で必ず打刻されること（呼び出し側が設定し忘れても記録が残るように）。
	if got.JobID != testJobID {
		t.Errorf("JobID = %q, want %q", got.JobID, testJobID)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt が設定されていない")
	}
}

// 埋め込みによって JSON がフラットなまま保たれること。
// 既存の status.json を読み書きし続けるための前提なので、ネストした形へ変えられません。
func TestStoredJSONStaysFlat(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, appStatus{
		Command: "generate", State: jobstatus.StateRunning,
		OutputDir: "gs://bucket/jobs/x",
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(store.get(t, testStatusPath), &raw); err != nil {
		t.Fatalf("保存された JSON が不正: %v", err)
	}

	for _, key := range []string{"job_id", "command", "state", "updated_at", "output_dir"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("トップレベルに %q が無い: %v", key, raw)
		}
	}
	if _, ok := raw["Status"]; ok {
		t.Error("埋め込みが Status キーとしてネストしている")
	}
}

// 未記録は障害と区別できるよう ErrNotFound を返すこと。
// 呼び出し側（ハンドラー）はこれを 404 に、それ以外を 500 にマップします。
func TestGetReturnsNotFoundForUnknownJob(t *testing.T) {
	t.Parallel()

	_, err := newStore(newMemStore()).Get(context.Background(), testJobID)
	if !errors.Is(err, jobstatus.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

// 未存在以外の読み取り失敗は ErrNotFound と区別すること。
//
// 両者を同じ扱いにすると、ストレージ障害中に完了済みジョブが「未記録」と読めてしまい、
// Recorder.AlreadySucceeded が生成をやり直します。
func TestGetDistinguishesUnavailableFromNotFound(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.openErr = errors.New("permission denied")

	_, err := newStore(store).Get(context.Background(), testJobID)
	if !errors.Is(err, jobstatus.ErrUnavailable) {
		t.Fatalf("Get() error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("読み取り失敗が ErrNotFound にも一致しています。呼び出し側が 404 と取り違えます")
	}
}

// 壊れた JSON は「未記録」ではなくデコード失敗として扱うこと。
// 未記録と同じ扱いにすると、破損に気づかないまま再生成が走り続けます。
func TestGetReturnsErrorForCorruptedJSON(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte("{ broken"))

	_, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatal("Get() error = nil, want a decode error")
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("壊れた JSON が未記録として扱われている")
	}
}

// 途中で切れた書き込みに次の書き込みが続いた status.json を、黙って読まないこと。
//
// 従来の json.Decoder は JSON 値のあとに残ったバイト列を無視するため、
// 1 つ目だけを読んで成功を返していました。破損に気づけないまま、その値で
// 判断が進みます。
func TestGetRejectsTrailingData(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte(
		`{"job_id":"`+testJobID+`","state":"succeeded"}{"state":"running"}`))

	_, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatal("Get() error = nil, want a decode error（末尾のバイト列を無視している）")
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("壊れた JSON が未記録として扱われている")
	}
}

// **重複したキーを拒否すること。**
//
// 従来の json.Decoder は後勝ちで読むため、succeeded を書いた記録に running が
// 続いた形の破損が running として読めていました。再実行ガードが防いでいるはずの
// 巻き戻しが、記録ではなく読み取りの側から入ってくる経路です。
func TestGetRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte(
		`{"job_id":"`+testJobID+`","state":"succeeded","state":"running"}`))

	got, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatalf("Get() error = nil, want a decode error（state = %q として読めている）", got.State)
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("壊れた JSON が未記録として扱われている")
	}
}

// 題目や失敗理由に & < > が含まれても、そのまま読める形で保存すること。
// v1 の \u0026 形式も引き続き読めます（JSON として同値なため）。
func TestSaveDoesNotEscapeHTML(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, appStatus{
		State: jobstatus.StateFailed,
		Title: "A & B <tag>",
		Error: `boom "x" & y`,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	written := string(store.get(t, testStatusPath))
	if strings.Contains(written, `\u0026`) || strings.Contains(written, `\u003c`) {
		t.Errorf("& や < が \\u00XX へ逃がされている: %s", written)
	}
	if !strings.Contains(written, "A & B <tag>") {
		t.Errorf("題目がそのまま書かれていない: %s", written)
	}

	// v1 が書いた既存の status.json も読めること。
	store.seed(t, testStatusPath, []byte(
		`{"job_id":"`+testJobID+`","state":"failed","title":"A \u0026 B \u003ctag\u003e"}`))
	got, err := newStore(store).Get(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Title != "A & B <tag>" {
		t.Errorf("Title = %q, want %q", got.Title, "A & B <tag>")
	}
}

// job_id を持たない古い記録でも、呼び出し側が ID 無しの値を受け取らないこと。
func TestGetBackfillsMissingJobID(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte(`{"state":"succeeded"}`))

	got, err := newStore(store).Get(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.JobID != testJobID {
		t.Errorf("JobID = %q, want %q", got.JobID, testJobID)
	}
}

// パス風のジョブ ID は正規化され、ジョブディレクトリの外へ書き出されないこと。
func TestSaveNormalizesPathTraversalJobID(t *testing.T) {
	t.Parallel()

	store := newMemStore()

	// "../../etc/passwd" は末尾要素 "passwd" へ正規化され、baseURI 配下に収まる。
	err := newStore(store).Save(context.Background(), "../../etc/passwd", appStatus{
		State: jobstatus.StateQueued,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	for _, path := range store.keys() {
		if !strings.HasPrefix(path, testBaseURI+"/") || strings.Contains(path, "..") {
			t.Fatalf("ジョブディレクトリの外へ書き込まれている: %q", path)
		}
	}
}

// 形式そのものが不正なジョブ ID は書き込みを試みずに拒否すること。
func TestSaveRejectsInvalidJobID(t *testing.T) {
	t.Parallel()

	store := newMemStore()

	err := newStore(store).Save(context.Background(), "日本語", appStatus{
		State: jobstatus.StateQueued,
	})
	if err == nil {
		t.Fatal("Save() error = nil, want an error")
	}
	if len(store.keys()) != 0 {
		t.Fatalf("不正なジョブ ID で書き込みが発生している: %v", store.keys())
	}
}

// 状態は上書きで更新され、履歴が積み上がらないこと。
func TestSaveOverwritesPreviousStatus(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	s := newStore(store)
	ctx := context.Background()

	for _, state := range []jobstatus.State{jobstatus.StateQueued, jobstatus.StateRunning, jobstatus.StateSucceeded} {
		if err := s.Save(ctx, testJobID, appStatus{State: state}); err != nil {
			t.Fatalf("Save(%q) error = %v", state, err)
		}
	}

	if got := len(store.keys()); got != 1 {
		t.Fatalf("オブジェクト数 = %d, want 1（上書きされていない）: %v", got, store.keys())
	}

	var stored appStatus
	if err := json.Unmarshal(store.get(t, testStatusPath), &stored); err != nil {
		t.Fatalf("保存された JSON が不正: %v", err)
	}
	if stored.State != jobstatus.StateSucceeded {
		t.Errorf("State = %q, want succeeded（最後の書き込みが残っていない）", stored.State)
	}
}

// 引数として渡した値を書き換えないこと。呼び出し側が同じ構造体を再利用しても
// 打刻が漏れ出さないようにするためです。
func TestSaveDoesNotMutateArgument(t *testing.T) {
	t.Parallel()

	status := appStatus{State: jobstatus.StateQueued}

	if err := newStore(newMemStore()).Save(context.Background(), testJobID, status); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if status.JobID != "" || !status.UpdatedAt.IsZero() {
		t.Errorf("引数が変更された: JobID=%q UpdatedAt=%v", status.JobID, status.UpdatedAt)
	}
}

// キャッシュさせない指定を付けて書くこと。状態は頻繁に変わるため、
// CDN・ブラウザに保持されると古い進行状況が表示されます。
func TestSavePassesWriteOptions(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, appStatus{
		State: jobstatus.StateQueued,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// 状態は頻繁に変わるため、CDN やブラウザにキャッシュさせてはいけません。
	// 以前は「オプションが 2 つ渡された」という数だけを見ていましたが、
	// memio が解決後の設定を返せるので中身で確かめます。
	opts, ok := store.h.Options(testStatusPath)
	if !ok {
		t.Fatalf("status.json が書かれていない: %v", store.keys())
	}
	if opts.ContentType != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", opts.ContentType)
	}
	if opts.CacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", opts.CacheControl)
	}
}

func TestDeleteRemovesStatus(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	s := newStore(store)
	ctx := context.Background()

	if err := s.Save(ctx, testJobID, appStatus{State: jobstatus.StateFailed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := s.Delete(ctx, testJobID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if len(store.keys()) != 0 {
		t.Errorf("削除されていない: %v", store.keys())
	}
}

// Delete の失敗も、どの URI で失敗したかが分かる形で返すこと（Save と同じ形式）。
func TestDeleteWrapsError(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.deleteErr = errors.New("permission denied")

	err := newStore(store).Delete(context.Background(), testJobID)
	if !errors.Is(err, store.deleteErr) {
		t.Fatalf("Delete() error = %v, want wrapping %v", err, store.deleteErr)
	}
	if !strings.Contains(err.Error(), testStatusPath) {
		t.Errorf("エラーに URI が含まれていない: %v", err)
	}
}

// Status を埋め込んでいない型は打刻されず、そのまま保存されること。
func TestSaveWithoutEmbeddedStatus(t *testing.T) {
	t.Parallel()

	type bare struct {
		Note string `json:"note"`
	}

	store := newMemStore()
	s := jobstatus.NewStore[bare](store, jobstatus.UnderJobDir(testBaseURI))
	if err := s.Save(context.Background(), testJobID, bare{Note: "hello"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(store.get(t, testStatusPath), &raw); err != nil {
		t.Fatalf("保存された JSON が不正: %v", err)
	}
	if _, ok := raw["updated_at"]; ok {
		t.Error("Status を埋め込んでいないのに打刻されている")
	}
}

func TestUnderJobDirRejectsEmptyBase(t *testing.T) {
	t.Parallel()

	s := jobstatus.NewStore[appStatus](newMemStore(), jobstatus.UnderJobDir("  "))
	if err := s.Save(context.Background(), testJobID, appStatus{}); err == nil {
		t.Fatal("Save() error = nil, want an error for empty base URI")
	}
}

// memStore は memio を jobstatus のテスト向けに包んだものです。
//
// 以前はここに Open / Write / Delete の手書き実装があり、「未存在は os.ErrNotExist で
// 返す」といった約束を fake 側でも書き直していました。ずれても気づけない形なので、
// remoteio が配っている memio へ寄せています（本物と同じ適合性スイートを通ります）。
// 残しているのは、障害を注入するための可変フィールドと検証用のヘルパーだけです。
type memStore struct {
	remoteio.Store
	h *memio.Handler

	// openErr は未存在以外の読み取り失敗（権限不足・障害）を再現します。
	openErr error
	// deleteErr は削除の失敗を再現します。
	deleteErr error
}

func newMemStore() *memStore {
	m := &memStore{}
	m.h = memio.New(
		memio.WithScheme(remoteio.SchemeGCS),
		memio.WithFailure(func(op, _ string) error {
			switch op {
			case "open":
				return m.openErr
			case "delete":
				return m.deleteErr
			}
			return nil
		}),
	)
	m.Store = remoteio.NewStore(m.h)
	return m
}

// seed は前提となる内容を書き込みます（障害注入の影響を受けません）。
func (m *memStore) seed(t *testing.T, uri string, data []byte) {
	t.Helper()
	if err := m.h.Seed(uri, data); err != nil {
		t.Fatalf("seed(%s) error = %v", uri, err)
	}
}

// get は保存されている内容を返します。
func (m *memStore) get(t *testing.T, uri string) []byte {
	t.Helper()
	data, err := remoteio.ReadAll(context.Background(), m.Store, uri)
	if err != nil {
		t.Fatalf("read(%s) error = %v", uri, err)
	}
	return data
}

// has は対象が保存されているかを返します。
func (m *memStore) has(uri string) bool {
	ok, err := m.Exists(context.Background(), uri)
	return err == nil && ok
}

func (m *memStore) keys() []string { return m.h.URIs() }

// 型引数にポインタ型を渡した場合、データを書く前に落とすこと。
//
// Store[*T] は Go として自然に書けてしまいますが、Save が any(&status) を Stamper へ
// アサートする都合上、対象が **T になって一致しません。その結果 job_id が空で
// updated_at がゼロ値の状態ファイルが、エラーもログも無しに書かれます。
// 静かに壊れるより、構築時に落ちるほうが被害が小さいためガードしています。
func TestNewStoreRejectsPointerTypeParameter(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewStore[*appStatus] が panic しませんでした。silent no-op になります")
		}
		if !strings.Contains(fmt.Sprint(r), "NewStore") {
			t.Errorf("panic メッセージに関数名が含まれていません: %v", r)
		}
	}()

	_ = jobstatus.NewStore[*appStatus](newMemStore(), jobstatus.UnderJobDir("gs://b/jobs"))
}

// 埋め込みの無い型は従来どおり許容すること（打刻・引き継ぎが行われないだけ）。
func TestNewStoreAllowsTypeWithoutEmbeddedStatus(t *testing.T) {
	t.Parallel()

	type bare struct {
		Note string `json:"note"`
	}
	if got := jobstatus.NewStore[bare](newMemStore(), jobstatus.UnderJobDir("gs://b/jobs")); got == nil {
		t.Fatal("NewStore[bare]() = nil")
	}
}
