package jobstatus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/shouni/go-remote-io/remoteio"

	"github.com/shouni/go-job-kit/jobstatus"
)

const testJobID = "c20260726-120000-abcd1234"

const testBaseURI = "gs://bucket/comics"

const testStatusPath = testBaseURI + "/" + testJobID + "/status.json"

// comicStatus は、利用側がアプリ固有のフィールドを足す典型例です。
type comicStatus struct {
	jobstatus.Status
	OutputDir string `json:"output_dir,omitempty"`
}

func newStore(store *memStore) *jobstatus.Store[comicStatus] {
	return jobstatus.NewStore[comicStatus](store, store, jobstatus.UnderJobDir(testBaseURI))
}

// 状態は成果物と同じジョブディレクトリ配下に置き、履歴削除（プレフィックス一括削除）で
// 自動的に片付くようにする。
func TestSaveWritesInsideJobDirectory(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, comicStatus{
		Status: jobstatus.Status{Command: "compose_comic", State: jobstatus.StateQueued},
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, ok := store.files[testStatusPath]; !ok {
		t.Fatalf("status.json が %q に書かれていない。書き込み済み: %v", testStatusPath, store.keys())
	}
}

func TestSaveAndGetRoundTrip(t *testing.T) {
	t.Parallel()

	store := newStore(newMemStore())
	original := comicStatus{
		Status: jobstatus.Status{
			Command:  "compose_comic",
			State:    jobstatus.StateSucceeded,
			Title:    "テスト作品",
			Attempts: 3,
		},
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
	err := newStore(store).Save(context.Background(), testJobID, comicStatus{
		Status:    jobstatus.Status{Command: "compose_comic", State: jobstatus.StateRunning},
		OutputDir: "gs://bucket/comics/x",
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(store.files[testStatusPath], &raw); err != nil {
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

// 壊れた JSON は「未記録」ではなくデコード失敗として扱うこと。
// 未記録と同じ扱いにすると、破損に気づかないまま再生成が走り続けます。
func TestGetReturnsErrorForCorruptedJSON(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.files[testStatusPath] = []byte("{ broken")

	_, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatal("Get() error = nil, want a decode error")
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("壊れた JSON が未記録として扱われている")
	}
}

// job_id を持たない古い記録でも、呼び出し側が ID 無しの値を受け取らないこと。
func TestGetBackfillsMissingJobID(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.files[testStatusPath] = []byte(`{"state":"succeeded"}`)

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
	err := newStore(store).Save(context.Background(), "../../etc/passwd", comicStatus{
		Status: jobstatus.Status{State: jobstatus.StateQueued},
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

	err := newStore(store).Save(context.Background(), "日本語", comicStatus{
		Status: jobstatus.Status{State: jobstatus.StateQueued},
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
		if err := s.Save(ctx, testJobID, comicStatus{Status: jobstatus.Status{State: state}}); err != nil {
			t.Fatalf("Save(%q) error = %v", state, err)
		}
	}

	if got := len(store.keys()); got != 1 {
		t.Fatalf("オブジェクト数 = %d, want 1（上書きされていない）: %v", got, store.keys())
	}

	var stored comicStatus
	if err := json.Unmarshal(store.files[testStatusPath], &stored); err != nil {
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

	status := comicStatus{Status: jobstatus.Status{State: jobstatus.StateQueued}}

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
	err := newStore(store).Save(context.Background(), testJobID, comicStatus{
		Status: jobstatus.Status{State: jobstatus.StateQueued},
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// remoteio の writeConfig は非公開のため中身は検証できません。
	// Content-Type と Cache-Control の 2 つが渡されていることだけを見ます。
	if got := store.optCount[testStatusPath]; got != 2 {
		t.Errorf("write options = %d, want 2 (content type, cache control)", got)
	}
}

func TestDeleteRemovesStatus(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	s := newStore(store)
	ctx := context.Background()

	if err := s.Save(ctx, testJobID, comicStatus{Status: jobstatus.Status{State: jobstatus.StateFailed}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := s.Delete(ctx, testJobID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if len(store.keys()) != 0 {
		t.Errorf("削除されていない: %v", store.keys())
	}
}

// Status を埋め込んでいない型は打刻されず、そのまま保存されること。
func TestSaveWithoutEmbeddedStatus(t *testing.T) {
	t.Parallel()

	type bare struct {
		Note string `json:"note"`
	}

	store := newMemStore()
	s := jobstatus.NewStore[bare](store, store, jobstatus.UnderJobDir(testBaseURI))
	if err := s.Save(context.Background(), testJobID, bare{Note: "hello"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(store.files[testStatusPath], &raw); err != nil {
		t.Fatalf("保存された JSON が不正: %v", err)
	}
	if _, ok := raw["updated_at"]; ok {
		t.Error("Status を埋め込んでいないのに打刻されている")
	}
}

func TestUnderJobDirRejectsEmptyBase(t *testing.T) {
	t.Parallel()

	s := jobstatus.NewStore[comicStatus](newMemStore(), newMemStore(), jobstatus.UnderJobDir("  "))
	if err := s.Save(context.Background(), testJobID, comicStatus{}); err == nil {
		t.Fatal("Save() error = nil, want an error for empty base URI")
	}
}

// memStore は remoteio.Reader / remoteio.OutputWriter のインメモリ実装です。
type memStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	optCount map[string]int
}

func newMemStore() *memStore {
	return &memStore{files: map[string][]byte{}, optCount: map[string]int{}}
}

func (m *memStore) Open(_ context.Context, path string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, ok := m.files[path]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memStore) Write(_ context.Context, path string, r io.Reader, opts ...remoteio.WriteOption) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = data
	m.optCount[path] = len(opts)
	return nil
}

func (m *memStore) Delete(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, path)
	return nil
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	paths := make([]string, 0, len(m.files))
	for path := range m.files {
		paths = append(paths, path)
	}
	return paths
}
