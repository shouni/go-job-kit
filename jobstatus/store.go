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

// The three classes Store returns. Each wraps its cause, so errors.Is checks work
// unchanged while the original failure reason stays in the logs.
//
// HTTP handlers should map them as follows. Each class calls for a different decision by
// the caller, so lumping them together always gets one of them wrong.
//
//	ErrInvalidJobID → 400  bad input; retrying will not fix it
//	ErrNotFound     → 404  not recorded; processing may go ahead
//	ErrUnavailable  → 503  just could not be read; it may be readable later
//	anything else   → 500  corrupt JSON and the like
var (
	// ErrNotFound means the job status has not been recorded yet.
	// It is a normal state, seen for submissions before their first record and for jobs
	// created before this feature existed.
	ErrNotFound = errors.New("job status not found")

	// ErrUnavailable means the job status "should exist but could not be read".
	//
	// It is separate from ErrNotFound because the two call for opposite decisions. With no
	// record, processing may go ahead; but treating a mere read failure as "absent" mistakes
	// a finished job for an unfinished one and redoes the whole generation
	// (see Recorder.AlreadySucceeded).
	//
	// The split uses the remoteio.ErrNotExist that remoteio returns, so no extra storage
	// round trip is needed.
	ErrUnavailable = errors.New("job status unavailable")

	// ErrInvalidJobID means the given job ID did not pass normalization.
	//
	// Without a separate error, handlers would answer an invalid ID slipped into a URL with
	// the same 5xx as a storage failure, inviting retries of a request that retrying cannot
	// fix. The cause stays as a jobid error, so errors.Is reaches jobid.ErrEmpty /
	// jobid.ErrTooLong / jobid.ErrInvalidFormat.
	ErrInvalidJobID = errors.New("invalid job id")
)

// contentType is the Content-Type of the job status JSON.
const contentType = "application/json; charset=utf-8"

// statusFile is the object name the job status is recorded under.
const statusFile = "status.json"

// Locator builds the status file's URI from a normalized job ID.
//
// The job ID passed in has already been through jobid.Sanitize in Store.
// The Locator does not need to validate it again.
type Locator func(jobID string) (string, error)

// UnderJobDir returns a Locator pointing at baseURI/{jobID}/status.json.
//
// The file sits in the same job directory as the artifacts, so deleting history (removing
// the prefix wholesale) cleans up the status file too.
func UnderJobDir(baseURI string) Locator {
	base := strings.TrimRight(strings.TrimSpace(baseURI), "/")
	return func(jobID string) (string, error) {
		if base == "" {
			return "", errors.New("jobstatus: base URI is empty")
		}
		return base + "/" + jobID + "/" + statusFile, nil
	}
}

// Storage is just the operations Store needs.
// remoteio.Store satisfies it as is.
//
// The composite interface from remoteio is not taken directly because this package is a
// hub referenced by five apps, and demanding more operations than needed would make the
// callers' tests provide implementations they never use.
type Storage interface {
	remoteio.Reader
	remoteio.Writer

	// Delete removes the status file. A missing file is not an error.
	Delete(ctx context.Context, name string) error
}

// Store reads and writes job status backed by remote storage.
//
// The type parameter T is the app-specific status type (a struct embedding Status).
type Store[T any] struct {
	storage Storage
	locate  Locator
	now     func() time.Time
}

// mustNotBePointer rejects, at construction, the misuse of passing a pointer type as the
// type parameter.
//
// The embedded Status is found through the method set of the value type T (Save asserts
// any(&status) to Stamper). If T is *X, the asserted value is **X, which satisfies neither
// Stamper nor Carrier nor IsTerminal. The result is status files written with no stamp, no
// carry-over and a re-run guard that is always false, with no error and no log.
//
// Store[*X] is natural Go to write and it breaks quietly, so it fails before any data is
// written. A type without the embedding (T = X not satisfying Stamper) is still allowed, as
// the doc says: it just gets no stamping and no carry-over, rather than breaking quietly by
// mistake.
func mustNotBePointer[T any](fn string) {
	if t := reflect.TypeFor[T](); t.Kind() == reflect.Pointer {
		panic(fmt.Sprintf(
			"jobstatus: %s[%s]: do not pass a pointer type as the type parameter; "+
				"stamping, carry-over and the re-run guard would be disabled (instantiate with a value type)", fn, t))
	}
}

// NewStore builds a Store.
//
// A remoteio.Store can be passed as storage directly. Reading and writing used to be taken
// as separate arguments, but remoteio folded them into one entry point, so this takes one
// as well.
//
// It panics when the type parameter is a pointer type (see mustNotBePointer).
func NewStore[T any](storage Storage, locate Locator) *Store[T] {
	mustNotBePointer[T]("NewStore")

	return &Store[T]{
		storage: storage,
		locate:  locate,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Save saves the job status.
//
// The status file always holds only the latest generation and is updated by overwriting.
// If status embeds Status, JobID and UpdatedAt are stamped here
// (the argument value is not modified).
//
// Writes carry Cache-Control: no-store. Status changes often, so letting a CDN or browser
// cache it would keep returning a stale status.
//
// Encoding uses encoding/json/v2 to match Get. Unlike v1 it does not escape &, < and > to
// \u00XX, so a status.json containing a title or failure reason reads as is
// (it is the same JSON value, and field order and embedding expansion match v1).
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
		// Status changes often, so do not let a CDN or browser cache it.
		remoteio.WithCacheControl("no-store"),
	); err != nil {
		return fmt.Errorf("jobstatus: write (%s): %w", uri, err)
	}
	return nil
}

// Get fetches the job status. It returns ErrNotFound when nothing is recorded.
//
// Corrupt JSON is returned as a decode failure, not as "not recorded". Treating it as not
// recorded would keep regenerating without anyone noticing the corruption.
//
// Decoding uses encoding/json/v2's UnmarshalRead, to reject bytes left after the JSON value
// and duplicate keys. The old Decoder silently accepted both. Duplicate keys in particular
// were last-wins: a status.json where a truncated write was followed by the next write could
// take the form {"state":"succeeded",...,"state":"running"} and read as running. That is a
// path for exactly the rollback the re-run guard is meant to prevent, entering from the
// read side rather than from recording.
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
		// remoteio wraps not-found in remoteio.ErrNotExist (GCS's storage.ErrObjectNotExist,
		// S3's NoSuchKey, os.Open's error as is for local). That separates "no record" from
		// "exists but unreadable" without an extra round trip.
		if errors.Is(err, remoteio.ErrNotExist) {
			return status, fmt.Errorf("%w: %s: %w", ErrNotFound, safeJobID, err)
		}
		// Missing permissions, storage outage, network failure. Mistaking this for "absent"
		// would treat a finished job as unfinished and redo the generation, so it is a separate error.
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

// Delete removes the job status. Use it to follow a history deletion.
//
// It does nothing when storage is not configured (nothing was recorded, so there is
// nothing to remove).
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

// resolve normalizes the job ID and builds the status file's URI.
//
// Job IDs appear in both URL paths and storage paths, so normalization doubles as a security
// boundary. Store always goes through here so callers cannot forget it.
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
