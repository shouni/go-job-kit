package jobstatus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shouni/go-job-kit/jobstatus"
)

// fakeStore は jobstatus.StatusStore のインメモリ実装です。
type fakeStore struct {
	mu      sync.Mutex
	saved   map[string]appStatus
	saveErr error
	getErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: map[string]appStatus{}}
}

func (f *fakeStore) Get(_ context.Context, jobID string) (appStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return appStatus{}, f.getErr
	}
	status, ok := f.saved[jobID]
	if !ok {
		return appStatus{}, jobstatus.ErrNotFound
	}
	return status, nil
}

func (f *fakeStore) Save(_ context.Context, jobID string, status appStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved[jobID] = status
	return nil
}

// 再試行のたびにワーカーは状態を組み立て直すため、試行回数と投入時刻は
// 前回の記録から引き継がれる必要がある。
func TestRecordCarriesOverAttemptsAndQueuedAt(t *testing.T) {
	t.Parallel()

	queuedAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		Status: jobstatus.Status{
			JobID:    testJobID,
			State:    jobstatus.StateRunning,
			Title:    "先に判明した題目",
			Attempts: 2,
			QueuedAt: queuedAt,
		},
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateFailed, Error: "boom"},
	}, func(next, _ *appStatus) {
		next.Attempts++
	})

	got := store.saved[testJobID]
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3（前回の 2 を引き継いで加算）", got.Attempts)
	}
	if !got.QueuedAt.Equal(queuedAt) {
		t.Errorf("QueuedAt = %v, want %v", got.QueuedAt, queuedAt)
	}
	if got.Title != "先に判明した題目" {
		t.Errorf("Title = %q（今回空なら前回を引き継ぐ）", got.Title)
	}
	if got.State != jobstatus.StateFailed {
		t.Errorf("State = %q, want failed（状態そのものは引き継がない）", got.State)
	}
}

// 生成の途中で題目が確定するサービスがあるため、新しく判明した題目を
// 古い値で上書きしてはいけない。
func TestRecordKeepsNewerTitle(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		Status: jobstatus.Status{JobID: testJobID, Title: "古い題目"},
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateSucceeded, Title: "確定した題目"},
	})

	if got := store.saved[testJobID].Title; got != "確定した題目" {
		t.Errorf("Title = %q, want %q", got, "確定した題目")
	}
}

// 成功後に古い失敗理由が残り続けないこと。
func TestRecordDoesNotCarryOverError(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateFailed, Error: "前回の失敗"},
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateSucceeded},
	})

	if got := store.saved[testJobID].Error; got != "" {
		t.Errorf("Error = %q, want 空文字", got)
	}
}

// サービス固有フィールドの引き継ぎは apply の prev から行える。
func TestRecordExposesPreviousToApply(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		Status:    jobstatus.Status{JobID: testJobID},
		OutputDir: "gs://bucket/jobs/" + testJobID,
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateRunning},
	}, func(next, prev *appStatus) {
		if prev != nil {
			next.OutputDir = prev.OutputDir
		}
	})

	if got := store.saved[testJobID].OutputDir; got != "gs://bucket/jobs/"+testJobID {
		t.Errorf("OutputDir = %q（apply で引き継げていない）", got)
	}
}

// 未記録のジョブでは prev が nil で、そのまま保存されること。
func TestRecordWithoutPreviousRecord(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	rec := jobstatus.NewRecorder(store)

	var sawPrev bool
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateQueued},
	}, func(_, prev *appStatus) {
		sawPrev = prev != nil
	})

	if sawPrev {
		t.Error("未記録なのに prev が渡っている")
	}
	if got := store.saved[testJobID].State; got != jobstatus.StateQueued {
		t.Errorf("State = %q, want queued", got)
	}
}

// 記録の失敗で生成そのものを止めないこと（警告ログに留める）。
func TestRecordSwallowsSaveFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saveErr = errors.New("storage down")

	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateRunning},
	})
}

// 前回の記録を読めなかったときは、引き継ぎが失われたことを警告ログに残すこと。
// Attempts・QueuedAt がこの記録でリセットされるため、静かには進めない。
// 記録そのものは行う（観測の欠けを理由に、失敗を増やさない）。
func TestRecordWarnsWhenPreviousUnreadable(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: storage down", jobstatus.ErrUnavailable)

	var buf bytes.Buffer
	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateRunning},
	})

	// getErr は Save には影響しないため、記録は行われている。
	if got := store.saved[testJobID].State; got != jobstatus.StateRunning {
		t.Errorf("State = %q, want running（読めなくても記録は行う）", got)
	}
	if log := buf.String(); !strings.Contains(log, "carry-over skipped") || !strings.Contains(log, testJobID) {
		t.Errorf("引き継ぎが失われた警告が無い: %q", log)
	}
}

// 未記録（ErrNotFound）は初回の記録として正常なので、警告ログを出さないこと。
func TestRecordStaysQuietWhenNotRecorded(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: 未記録", jobstatus.ErrNotFound)

	var buf bytes.Buffer
	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		Status: jobstatus.Status{JobID: testJobID, State: jobstatus.StateQueued},
	})

	if got := buf.String(); got != "" {
		t.Errorf("初回の記録なのにログが出ている: %q", got)
	}
}

// 記録先が未設定でも呼び出せ、何も起きないこと。
func TestRecorderWithoutStore(t *testing.T) {
	t.Parallel()

	rec := jobstatus.NewRecorder[appStatus](nil)
	if rec.Enabled() {
		t.Error("Enabled() = true, want false")
	}
	rec.Record(context.Background(), testJobID, appStatus{})
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err != nil {
		t.Errorf("AlreadySucceeded() error = %v, want nil", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false")
	}
}

func TestAlreadySucceeded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state jobstatus.State
		want  bool
	}{
		{name: "succeeded なら打ち切る", state: jobstatus.StateSucceeded, want: true},
		{name: "running は続行", state: jobstatus.StateRunning, want: false},
		// failed は Cloud Tasks が再試行しうるため終了とはみなさない。
		{name: "failed は続行", state: jobstatus.StateFailed, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newFakeStore()
			store.saved[testJobID] = appStatus{Status: jobstatus.Status{State: tt.state}}

			rec := jobstatus.NewRecorder(store)
			got, err := rec.AlreadySucceeded(context.Background(), testJobID)
			if err != nil {
				t.Fatalf("AlreadySucceeded() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("AlreadySucceeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// 未記録は「完了していない」として続行できること。記録前の投入やこの機能より前の
// ジョブでも起こる正常な状態なので、エラーにはしない。
func TestAlreadySucceededWhenNotRecorded(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: 未記録", jobstatus.ErrNotFound)

	rec := jobstatus.NewRecorder(store)
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("AlreadySucceeded() error = %v, want nil（未記録は正常）", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false")
	}
}

// 状態を読めなかった場合はエラーを返すこと。
//
// false に倒すと完了済みジョブを再生成してしまい、このガードが防ぐはずのコストを
// ガード自身が発生させます。true に倒すと未完了のジョブが二度と実行されません。
// どちらへも倒さず、呼び出し側が再配信に委ねられるようにします。
func TestAlreadySucceededOnReadFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: storage down", jobstatus.ErrUnavailable)

	rec := jobstatus.NewRecorder(store)
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err == nil {
		t.Fatal("AlreadySucceeded() error = nil, want error（読めないなら判断を返す）")
	}
	if !errors.Is(err, jobstatus.ErrUnavailable) {
		t.Errorf("error = %v, want wrapping ErrUnavailable", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false")
	}
}

// Status を埋め込んでいない型では引き継ぎが行われず、渡した値がそのまま保存されること。
func TestRecordWithoutEmbeddedStatus(t *testing.T) {
	t.Parallel()

	store := &bareStore{saved: map[string]bare{}}
	store.saved[testJobID] = bare{Note: "前回"}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, bare{Note: "今回"})

	if got := store.saved[testJobID].Note; got != "今回" {
		t.Errorf("Note = %q, want %q", got, "今回")
	}
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("AlreadySucceeded() error = %v, want nil", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false（終了判定を持たない型）")
	}
}

// bare は Status を埋め込んでいない型です。
type bare struct {
	Note string
}

type bareStore struct {
	saved map[string]bare
}

func (b *bareStore) Get(_ context.Context, jobID string) (bare, error) {
	status, ok := b.saved[jobID]
	if !ok {
		return bare{}, jobstatus.ErrNotFound
	}
	return status, nil
}

func (b *bareStore) Save(_ context.Context, jobID string, status bare) error {
	b.saved[jobID] = status
	return nil
}

// Store をそのまま Recorder へ渡せること（利用側がアダプタを書かずに済むこと）。
var _ jobstatus.StatusStore[appStatus] = (*jobstatus.Store[appStatus])(nil)
