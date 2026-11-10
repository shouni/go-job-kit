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

// appStatus is the typical case of a caller adding service-specific fields.
type appStatus struct {
	jobstatus.Status
	OutputDir string `json:"output_dir,omitempty"`
}

func newStore(store *memStore) *jobstatus.Store[appStatus] {
	return jobstatus.NewStore[appStatus](store, jobstatus.UnderJobDir(testBaseURI))
}

// The status sits in the same job directory as the artifacts, so deleting history
// (removing the prefix wholesale) cleans it up automatically.
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
		t.Fatalf("status.json was not written to %q; written: %v", testStatusPath, store.keys())
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
	// Save always stamps it (so a record remains even if the caller forgets to set it).
	if got.JobID != testJobID {
		t.Errorf("JobID = %q, want %q", got.JobID, testJobID)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is not set")
	}
}

// Embedding keeps the JSON flat.
// It is the premise for continuing to read and write existing status.json files, so it cannot change to a nested shape.
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
		t.Fatalf("stored JSON is invalid: %v", err)
	}

	for _, key := range []string{"job_id", "command", "state", "updated_at", "output_dir"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("%q missing at the top level: %v", key, raw)
		}
	}
	if _, ok := raw["Status"]; ok {
		t.Error("the embedding is nested under a Status key")
	}
}

// A missing record returns ErrNotFound so it can be told apart from a failure.
// Callers (handlers) map it to 404 and everything else to 500.
func TestGetReturnsNotFoundForUnknownJob(t *testing.T) {
	t.Parallel()

	_, err := newStore(newMemStore()).Get(context.Background(), testJobID)
	if !errors.Is(err, jobstatus.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

// Read failures other than not-found are kept apart from ErrNotFound.
//
// Treating the two alike would let a finished job read as "not recorded" during a storage
// outage, and Recorder.AlreadySucceeded would redo the generation.
func TestGetDistinguishesUnavailableFromNotFound(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.openErr = errors.New("permission denied")

	_, err := newStore(store).Get(context.Background(), testJobID)
	if !errors.Is(err, jobstatus.ErrUnavailable) {
		t.Fatalf("Get() error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("a read failure also matches ErrNotFound; callers would mistake it for a 404")
	}
}

// Corrupt JSON is treated as a decode failure, not as "not recorded".
// Treating it as not recorded would keep regenerating without anyone noticing the corruption.
func TestGetReturnsErrorForCorruptedJSON(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte("{ broken"))

	_, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatal("Get() error = nil, want a decode error")
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("corrupt JSON was treated as not recorded")
	}
}

// A status.json where a truncated write was followed by the next write is not read silently.
//
// The old json.Decoder ignored bytes left after the JSON value, so it read only the first
// one and reported success. Decisions then proceeded on that value without the corruption
// being noticed.
func TestGetRejectsTrailingData(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte(
		`{"job_id":"`+testJobID+`","state":"succeeded"}{"state":"running"}`))

	_, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatal("Get() error = nil, want a decode error (trailing bytes were ignored)")
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("corrupt JSON was treated as not recorded")
	}
}

// **Duplicate keys are rejected.**
//
// The old json.Decoder read last-wins, so a corruption where running followed a record of
// succeeded read as running. That is a path for exactly the rollback the re-run guard is
// meant to prevent, entering from the read side rather than from recording.
func TestGetRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.seed(t, testStatusPath, []byte(
		`{"job_id":"`+testJobID+`","state":"succeeded","state":"running"}`))

	got, err := newStore(store).Get(context.Background(), testJobID)
	if err == nil {
		t.Fatalf("Get() error = nil, want a decode error (read as state = %q)", got.State)
	}
	if errors.Is(err, jobstatus.ErrNotFound) {
		t.Error("corrupt JSON was treated as not recorded")
	}
}

// A title or failure reason containing & < > is saved in a form that reads as is.
// v1's \u0026 form still reads too (it is the same JSON value).
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
		t.Errorf("& or < was escaped to \\u00XX: %s", written)
	}
	if !strings.Contains(written, "A & B <tag>") {
		t.Errorf("title was not written as is: %s", written)
	}

	// Existing status.json files written by v1 still read.
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

// Even an old record without job_id never hands the caller a value without an ID.
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

// A path-like job ID is normalized and never written outside the job directory.
func TestSaveNormalizesPathTraversalJobID(t *testing.T) {
	t.Parallel()

	store := newMemStore()

	// "../../etc/passwd" normalizes to its last element "passwd" and stays under baseURI.
	err := newStore(store).Save(context.Background(), "../../etc/passwd", appStatus{
		State: jobstatus.StateQueued,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	for _, path := range store.keys() {
		if !strings.HasPrefix(path, testBaseURI+"/") || strings.Contains(path, "..") {
			t.Fatalf("written outside the job directory: %q", path)
		}
	}
}

// A job ID whose format is itself invalid is rejected without attempting a write.
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
		t.Fatalf("a write happened with an invalid job ID: %v", store.keys())
	}
}

// Status is updated by overwriting; history does not pile up.
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
		t.Fatalf("object count = %d, want 1 (not overwritten): %v", got, store.keys())
	}

	var stored appStatus
	if err := json.Unmarshal(store.get(t, testStatusPath), &stored); err != nil {
		t.Fatalf("stored JSON is invalid: %v", err)
	}
	if stored.State != jobstatus.StateSucceeded {
		t.Errorf("State = %q, want succeeded (the last write did not remain)", stored.State)
	}
}

// The value passed as an argument is not rewritten, so stamps do not leak out when the
// caller reuses the same struct.
func TestSaveDoesNotMutateArgument(t *testing.T) {
	t.Parallel()

	status := appStatus{State: jobstatus.StateQueued}

	if err := newStore(newMemStore()).Save(context.Background(), testJobID, status); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if status.JobID != "" || !status.UpdatedAt.IsZero() {
		t.Errorf("argument was modified: JobID=%q UpdatedAt=%v", status.JobID, status.UpdatedAt)
	}
}

// Writes carry a no-cache directive. Status changes often, so if a CDN or browser held it,
// stale progress would be shown.
func TestSavePassesWriteOptions(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	err := newStore(store).Save(context.Background(), testJobID, appStatus{
		State: jobstatus.StateQueued,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Status changes often, so a CDN or browser must not cache it.
	// This used to check only the count ("two options were passed"), but memio can return
	// the resolved settings, so the contents are checked.
	opts, ok := store.h.Options(testStatusPath)
	if !ok {
		t.Fatalf("status.json was not written: %v", store.keys())
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
		t.Errorf("not deleted: %v", store.keys())
	}
}

// A Delete failure is also returned in a form that tells which URI failed (same format as Save).
func TestDeleteWrapsError(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	store.deleteErr = errors.New("permission denied")

	err := newStore(store).Delete(context.Background(), testJobID)
	if !errors.Is(err, store.deleteErr) {
		t.Fatalf("Delete() error = %v, want wrapping %v", err, store.deleteErr)
	}
	if !strings.Contains(err.Error(), testStatusPath) {
		t.Errorf("error does not include the URI: %v", err)
	}
}

// A type that does not embed Status is not stamped and is saved as is.
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
		t.Fatalf("stored JSON is invalid: %v", err)
	}
	if _, ok := raw["updated_at"]; ok {
		t.Error("stamped although Status is not embedded")
	}
}

func TestUnderJobDirRejectsEmptyBase(t *testing.T) {
	t.Parallel()

	s := jobstatus.NewStore[appStatus](newMemStore(), jobstatus.UnderJobDir("  "))
	if err := s.Save(context.Background(), testJobID, appStatus{}); err == nil {
		t.Fatal("Save() error = nil, want an error for empty base URI")
	}
}

// memStore wraps memio for the jobstatus tests.
//
// This used to hold hand-written Open / Write / Delete implementations that restated
// promises such as "not-found is returned as os.ErrNotExist" on the fake side. That could
// drift unnoticed, so it now builds on the memio remoteio ships (it passes the same
// conformance suite as the real backends). What remains is only the mutable fields for
// injecting failures and the helpers for checking.
type memStore struct {
	remoteio.Store
	h *memio.Handler

	// openErr reproduces a read failure other than not-found (missing permissions, outage).
	openErr error
	// deleteErr reproduces a delete failure.
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

// seed writes precondition content (unaffected by failure injection).
func (m *memStore) seed(t *testing.T, uri string, data []byte) {
	t.Helper()
	if err := m.h.Seed(uri, data); err != nil {
		t.Fatalf("seed(%s) error = %v", uri, err)
	}
}

// get returns the stored content.
func (m *memStore) get(t *testing.T, uri string) []byte {
	t.Helper()
	data, err := remoteio.ReadAll(context.Background(), m.Store, uri)
	if err != nil {
		t.Fatalf("read(%s) error = %v", uri, err)
	}
	return data
}

// has reports whether the target is stored.
func (m *memStore) has(uri string) bool {
	ok, err := m.Exists(context.Background(), uri)
	return err == nil && ok
}

func (m *memStore) keys() []string { return m.h.URIs() }

// A pointer type as the type parameter fails before any data is written.
//
// Store[*T] is natural Go to write, but because Save asserts any(&status) to Stamper, the
// asserted value becomes **T and does not match. The result is a status file with an empty
// job_id and a zero updated_at, written with no error and no log. Failing at construction
// does less harm than breaking quietly, hence the guard.
func TestNewStoreRejectsPointerTypeParameter(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewStore[*appStatus] did not panic; it would be a silent no-op")
		}
		if !strings.Contains(fmt.Sprint(r), "NewStore") {
			t.Errorf("panic message does not include the function name: %v", r)
		}
	}()

	_ = jobstatus.NewStore[*appStatus](newMemStore(), jobstatus.UnderJobDir("gs://b/jobs"))
}

// A type without the embedding is still allowed (it just gets no stamping or carry-over).
func TestNewStoreAllowsTypeWithoutEmbeddedStatus(t *testing.T) {
	t.Parallel()

	type bare struct {
		Note string `json:"note"`
	}
	if got := jobstatus.NewStore[bare](newMemStore(), jobstatus.UnderJobDir("gs://b/jobs")); got == nil {
		t.Fatal("NewStore[bare]() = nil")
	}
}
