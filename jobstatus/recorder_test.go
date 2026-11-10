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

// fakeStore is an in-memory implementation of jobstatus.StatusStore.
type fakeStore struct {
	mu       sync.Mutex
	saved    map[string]appStatus
	saveErr  error
	getErr   error
	getCount int
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: map[string]appStatus{}}
}

func (f *fakeStore) Get(_ context.Context, jobID string) (appStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getCount++
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

// The worker rebuilds the status on every retry, so the attempt count and submission time
// must carry over from the previous record.
func TestRecordCarriesOverAttemptsAndQueuedAt(t *testing.T) {
	t.Parallel()

	queuedAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID:    testJobID,
		State:    jobstatus.StateRunning,
		Title:    "先に判明した題目",
		Attempts: 2,
		QueuedAt: queuedAt,
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateFailed, Error: "boom",
	}, func(next, _ *appStatus) {
		next.Attempts++
	})

	got := store.saved[testJobID]
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (the previous 2 carried over and incremented)", got.Attempts)
	}
	if !got.QueuedAt.Equal(queuedAt) {
		t.Errorf("QueuedAt = %v, want %v", got.QueuedAt, queuedAt)
	}
	if got.Title != "先に判明した題目" {
		t.Errorf("Title = %q (an empty one this time takes the previous)", got.Title)
	}
	if got.State != jobstatus.StateFailed {
		t.Errorf("State = %q, want failed (the state itself does not carry over)", got.State)
	}
}

// Some services settle the title mid-generation, so a newly known title must not be
// overwritten by the old value.
func TestRecordKeepsNewerTitle(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID: testJobID, Title: "古い題目",
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateSucceeded, Title: "確定した題目",
	})

	if got := store.saved[testJobID].Title; got != "確定した題目" {
		t.Errorf("Title = %q, want %q", got, "確定した題目")
	}
}

// An old failure reason does not linger after a success.
func TestRecordDoesNotCarryOverError(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID: testJobID, State: jobstatus.StateFailed, Error: "前回の失敗",
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateSucceeded,
	})

	if got := store.saved[testJobID].Error; got != "" {
		t.Errorf("Error = %q, want empty string", got)
	}
}

// Service-specific fields can be carried over from apply's prev.
func TestRecordExposesPreviousToApply(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID:     testJobID,
		OutputDir: "gs://bucket/jobs/" + testJobID,
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	}, func(next, prev *appStatus) {
		if prev != nil {
			next.OutputDir = prev.OutputDir
		}
	})

	if got := store.saved[testJobID].OutputDir; got != "gs://bucket/jobs/"+testJobID {
		t.Errorf("OutputDir = %q (apply did not carry it over)", got)
	}
}

// For a job with no record, prev is nil and the status is saved as is.
func TestRecordWithoutPreviousRecord(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	rec := jobstatus.NewRecorder(store)

	var sawPrev bool
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateQueued,
	}, func(_, prev *appStatus) {
		sawPrev = prev != nil
	})

	if sawPrev {
		t.Error("prev was passed although nothing is recorded")
	}
	if got := store.saved[testJobID].State; got != jobstatus.StateQueued {
		t.Errorf("State = %q, want queued", got)
	}
}

// A failed record never stops the generation itself (a warning log only).
func TestRecordSwallowsSaveFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saveErr = errors.New("storage down")

	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	})
}

// When the previous record cannot be read, a warning log says the carry-over was lost.
// Attempts and QueuedAt are reset by this record, so it must not proceed silently.
// The record itself is still written (a gap in observation is no reason to add failures).
func TestRecordWarnsWhenPreviousUnreadable(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: storage down", jobstatus.ErrUnavailable)

	var buf bytes.Buffer
	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	})

	// getErr does not affect Save, so the record was written.
	if got := store.saved[testJobID].State; got != jobstatus.StateRunning {
		t.Errorf("State = %q, want running (record even when unreadable)", got)
	}
	if log := buf.String(); !strings.Contains(log, "carry-over skipped") || !strings.Contains(log, testJobID) {
		t.Errorf("no warning about the lost carry-over: %q", log)
	}
}

// No record (ErrNotFound) is normal for a first record, so no warning is logged.
func TestRecordStaysQuietWhenNotRecorded(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: not recorded", jobstatus.ErrNotFound)

	var buf bytes.Buffer
	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateQueued,
	})

	if got := buf.String(); got != "" {
		t.Errorf("logged although this is the first record: %q", got)
	}
}

// With no destination configured, it can still be called and nothing happens.
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
		// failed may be retried by Cloud Tasks, so it is not treated as finished.
		{name: "failed は続行", state: jobstatus.StateFailed, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newFakeStore()
			store.saved[testJobID] = appStatus{State: tt.state}

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

// No record continues as "not completed". It is a normal state, seen for submissions before
// their first record and for jobs older than this feature, so it is not an error.
func TestAlreadySucceededWhenNotRecorded(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: not recorded", jobstatus.ErrNotFound)

	rec := jobstatus.NewRecorder(store)
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("AlreadySucceeded() error = %v, want nil (no record is normal)", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false")
	}
}

// When the status cannot be read, an error is returned.
//
// Falling to false would regenerate a finished job: the guard itself would incur the cost it
// exists to prevent. Falling to true would mean an unfinished job never runs again.
// It falls neither way, so the caller can leave it to redelivery.
func TestAlreadySucceededOnReadFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: storage down", jobstatus.ErrUnavailable)

	rec := jobstatus.NewRecorder(store)
	done, err := rec.AlreadySucceeded(context.Background(), testJobID)
	if err == nil {
		t.Fatal("AlreadySucceeded() error = nil, want error (hand the decision back when unreadable)")
	}
	if !errors.Is(err, jobstatus.ErrUnavailable) {
		t.Errorf("error = %v, want wrapping ErrUnavailable", err)
	}
	if done {
		t.Error("AlreadySucceeded() = true, want false")
	}
}

// A type that does not embed Status gets no carry-over and is saved as given.
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
		t.Error("AlreadySucceeded() = true, want false (a type with no terminal check)")
	}
}

// bare is a type that does not embed Status.
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

// A Store can be passed to Recorder directly (callers need not write an adapter).
var _ jobstatus.StatusStore[appStatus] = (*jobstatus.Store[appStatus])(nil)

// A redelivered finished job is not overwritten with running or failed.
// Rolled back, both a polling screen and the re-run guard would read "not done yet".
func TestRecordDoesNotRollBackFinishedJob(t *testing.T) {
	t.Parallel()

	for _, state := range []jobstatus.State{jobstatus.StateRunning, jobstatus.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			store := newFakeStore()
			store.saved[testJobID] = appStatus{
				JobID: testJobID, State: jobstatus.StateSucceeded, Title: "完了済み",
			}

			rec := jobstatus.NewRecorder(store,
				jobstatus.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			rec.Record(context.Background(), testJobID, appStatus{
				JobID: testJobID, State: state, Error: "再配信された",
			})

			if got := store.saved[testJobID].State; got != jobstatus.StateSucceeded {
				t.Errorf("State = %q, want succeeded (rolled back by redelivery)", got)
			}
		})
	}
}

// The rollback is stopped even when apply rewrites the state.
// Checking before apply would disagree with the value actually saved and let it slip through.
func TestRecordChecksRollbackAfterApply(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{JobID: testJobID, State: jobstatus.StateSucceeded}

	rec := jobstatus.NewRecorder(store,
		jobstatus.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateSucceeded,
	}, func(next, _ *appStatus) {
		next.State = jobstatus.StateRunning // apply rolls it back
	})

	if got := store.saved[testJobID].State; got != jobstatus.StateSucceeded {
		t.Errorf("State = %q, want succeeded (not checked against the value after apply)", got)
	}
}

// **Writing queued onto a finished job is a rebuild submission, so it goes through.**
//
// A redelivered task writes running or failed; only a new request that went through a
// handler writes queued. Blocking it here would leave the record at succeeded, and the
// worker's re-run guard would read the rebuild as "already done" and never run it.
func TestRecordAllowsRequeueAfterSuccess(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID:     testJobID,
		State:     jobstatus.StateSucceeded,
		Title:     "前回の成果",
		Attempts:  1,
		OutputDir: "gs://bucket/jobs/" + testJobID,
	}

	rec := jobstatus.NewRecorder(store)
	rec.Record(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateQueued,
	})

	got := store.saved[testJobID]
	if got.State != jobstatus.StateQueued {
		t.Fatalf("State = %q, want queued (the rebuild submission was dropped)", got.State)
	}
	// Even for a rebuild, the submission time, attempt count and title still carry over.
	if got.Attempts != 1 || got.Title != "前回の成果" {
		t.Errorf("Attempts = %d, Title = %q (carry-over is not working)", got.Attempts, got.Title)
	}
}

// Begin performs the stop check and the running record with a single read.
func TestBeginRecordsAndReadsOnce(t *testing.T) {
	t.Parallel()

	queuedAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID: testJobID, State: jobstatus.StateQueued, Attempts: 1, QueuedAt: queuedAt,
	}

	rec := jobstatus.NewRecorder(store)
	done, err := rec.Begin(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	}, func(next, _ *appStatus) {
		next.Attempts++
	})
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	if done {
		t.Error("Begin() = true, want false (an unfinished job)")
	}

	got := store.saved[testJobID]
	if got.State != jobstatus.StateRunning {
		t.Errorf("State = %q, want running", got.State)
	}
	if got.Attempts != 2 || !got.QueuedAt.Equal(queuedAt) {
		t.Errorf("Attempts = %d, QueuedAt = %v (carry-over is not working)", got.Attempts, got.QueuedAt)
	}
	// AlreadySucceeded + Record used to read the same status.json twice.
	if store.getCount != 1 {
		t.Errorf("Get count = %d, want 1", store.getCount)
	}
}

// A finished job returns true and nothing is recorded.
func TestBeginSkipsFinishedJob(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.saved[testJobID] = appStatus{
		JobID: testJobID, State: jobstatus.StateSucceeded, Title: "完了済み",
	}

	rec := jobstatus.NewRecorder(store)
	done, err := rec.Begin(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	})
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	if !done {
		t.Fatal("Begin() = false, want true (already finished)")
	}
	if got := store.saved[testJobID].State; got != jobstatus.StateSucceeded {
		t.Errorf("State = %q, want succeeded (must not record)", got)
	}
}

// No record continues as "not completed" and writes the first record.
func TestBeginWhenNotRecorded(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	rec := jobstatus.NewRecorder(store)

	done, err := rec.Begin(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	})
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil (no record is normal)", err)
	}
	if done {
		t.Error("Begin() = true, want false")
	}
	if got := store.saved[testJobID].State; got != jobstatus.StateRunning {
		t.Errorf("State = %q, want running", got)
	}
}

// When the status cannot be read, an error is returned and nothing is recorded.
// The caller just leaves it to redelivery, so there is no point writing a running record
// that has lost its carry-over source.
func TestBeginOnReadFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.getErr = fmt.Errorf("%w: storage down", jobstatus.ErrUnavailable)

	rec := jobstatus.NewRecorder(store)
	done, err := rec.Begin(context.Background(), testJobID, appStatus{
		JobID: testJobID, State: jobstatus.StateRunning,
	})
	if !errors.Is(err, jobstatus.ErrUnavailable) {
		t.Fatalf("Begin() error = %v, want wrapping ErrUnavailable", err)
	}
	if done {
		t.Error("Begin() = true, want false")
	}
	if _, ok := store.saved[testJobID]; ok {
		t.Error("recorded although the status could not be read")
	}
}

// With no destination configured, it can still be called and processing continues.
func TestBeginWithoutStore(t *testing.T) {
	t.Parallel()

	rec := jobstatus.NewRecorder[appStatus](nil)
	done, err := rec.Begin(context.Background(), testJobID, appStatus{})
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	if done {
		t.Error("Begin() = true, want false")
	}
}
