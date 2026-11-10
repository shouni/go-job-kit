package jobstatus

import (
	"context"
	"errors"
	"log/slog"
)

// StatusStore is the minimal read and write Recorder needs.
// *Store[T] satisfies it as is.
//
// Recorder takes an interface rather than the Store type so callers can swap where status
// is stored (fakes in tests, or recording somewhere other than storage).
//
// When an app defines its own port for saving status, aligning it with this shape is
// recommended: *Store[T] then implements the port directly. Without alignment all it takes
// is one thin adapter, but the same adapter multiplies with every consumer.
type StatusStore[T any] interface {
	Get(ctx context.Context, jobID string) (T, error)
	Save(ctx context.Context, jobID string, status T) error
}

// Carrier is the interface Recorder uses to carry common fields over from the previous
// record.
//
// Types embedding Status satisfy it automatically through method promotion. Callers do not
// need to implement it explicitly. When a type that does not embed it is passed, nothing is
// carried over and the value is saved as given.
type Carrier interface {
	Common() Status
	CarryOver(prev Status)
}

// Recorder is a thin layer for a worker to record status as a job progresses.
//
// The three services this was ported from all layered the same three behaviours on top of
// Store. There was no reason for each to keep its own, so they are gathered here.
//
//  1. A re-run guard for finished jobs (AlreadySucceeded)
//  2. Carrying common fields over from the previous record (Record)
//  3. A failed record never stops the generation itself (a warning log only)
//
// 3 is because status is only a record for observation, and aborting a generation because
// it could not be written does more harm.
//
// At the worker's entry point 1 and 2 are always called together, so that pair is gathered
// into Begin.
type Recorder[T any] struct {
	store  StatusStore[T]
	logger *slog.Logger
}

type recorderOptions struct {
	logger *slog.Logger
}

// RecorderOption changes how Recorder behaves.
type RecorderOption func(*recorderOptions)

// WithLogger replaces the logger that records failures to record.
// The default is slog.Default().
func WithLogger(logger *slog.Logger) RecorderOption {
	return func(o *recorderOptions) { o.logger = logger }
}

// NewRecorder builds a Recorder.
//
// When store is nil, nothing is recorded (Enabled returns false and the other methods do
// nothing). This lets status recording be wired in as an optional feature, without callers
// checking for nil on every call.
func NewRecorder[T any](store StatusStore[T], opts ...RecorderOption) *Recorder[T] {
	mustNotBePointer[T]("NewRecorder")

	cfg := recorderOptions{logger: slog.Default()}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	return &Recorder[T]{store: store, logger: cfg.logger}
}

// Enabled reports whether a destination for status records is configured.
func (r *Recorder[T]) Enabled() bool {
	return r != nil && r.store != nil
}

// AlreadySucceeded reports whether the job has already completed.
//
// Cloud Tasks delivers at least once, so when a worker returns an error (because a
// notification failed, say) the same task is delivered again. Calling the whole generation
// again would double its cost, so the worker should stop here when the job is complete.
//
//	done, err := rec.AlreadySucceeded(ctx, task.JobID)
//	if err != nil {
//	    return err // cannot decide, so leave it to redelivery
//	}
//	if done {
//	    return nil
//	}
//
// No record (ErrNotFound) returns false, "not completed". A missing record is a normal
// state, seen for submissions before their first record and for jobs older than this feature.
//
// When the status could not be read (ErrUnavailable) it returns an error. Falling to false
// here would mistake a finished job for an unfinished one and redo the generation: the guard
// itself would incur the cost it exists to prevent. Falling to true would treat an unfinished
// job as finished and ACK the task, so that job would never run. Either way can be wrong, so
// the decision is handed back, letting the caller return an error and leave it to redelivery.
//
// This is a per-job guard. When the work is split into continuation tasks, it does not
// prevent redelivery partway through (state=running).
func (r *Recorder[T]) AlreadySucceeded(ctx context.Context, jobID string) (bool, error) {
	if !r.Enabled() {
		return false, nil
	}

	status, err := r.store.Get(ctx, jobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if terminal, ok := any(&status).(interface{ IsTerminal() bool }); ok {
		return terminal.IsTerminal(), nil
	}
	return false, nil
}

// Record saves status after carrying common fields over from the previous record.
//
// What carries over is Attempts, QueuedAt, and Title and Command when status leaves them
// empty (see CarryOver). The worker rebuilds the status from the task every time, so without
// this the attempt count and submission time would be lost on every retry.
//
// apply is called after the carry-over and before saving. Use it to increment the attempt
// count or to carry over service-specific fields. prev is the previous record, nil when it
// could not be read. No record (ErrNotFound) proceeds silently as the first record; any
// other read failure has lost the source of the carry-over (Attempts and QueuedAt are reset
// by this record), so it leaves a warning log.
//
//	// Record the submission and keep where the previous artifacts are
//	rec.Record(ctx, req.JobID, newStatus(req, StateQueued), func(next, prev *JobStatus) {
//	    if prev != nil {
//	        next.OutputDir = prev.OutputDir
//	    }
//	})
//
// When the previous record is finished (IsTerminal) and this one is running or failed, it is
// not saved (queued is the exception; see rolledBack for why). The previous record used for
// the check has already been read for the carry-over, so there is no extra storage round trip.
//
// Use Begin to record the start of processing (running); it shares the same read as the
// re-run guard.
//
// A failed save is not reported to the caller; it stays a warning log.
func (r *Recorder[T]) Record(ctx context.Context, jobID string, status T, apply ...func(next, prev *T)) {
	if !r.Enabled() {
		return
	}

	prev, prevCommon, err := r.previous(ctx, jobID)
	if err != nil {
		// No record is the first record, which previous swallows. Getting here means a read
		// failure or corrupt JSON: the carry-over is lost and Attempts and QueuedAt are reset by
		// this record, so leave the cause rather than proceeding silently. The record itself is
		// still written (a gap in observation is no reason to add failures).
		r.logger.WarnContext(ctx, "failed to read previous job status; carry-over skipped",
			"job_id", jobID, "error", err)
	}

	r.save(ctx, jobID, status, prev, prevCommon, apply)
}

// Begin performs the re-run guard and the record of starting work with a single read.
// It returns true when the job was already finished and nothing was done.
//
// Checking with AlreadySucceeded and then writing running with Record makes Record reread
// the previous record for the carry-over, fetching the same status.json twice in one task.
// The check needs exactly the same information as the carry-over source, so this does both.
// When only the stop check is needed, without a record, use AlreadySucceeded.
//
//	done, err := rec.Begin(ctx, task.JobID, newStatus(task, StateRunning),
//	    func(next, _ *JobStatus) { next.Attempts++ })
//	if err != nil {
//	    return err // cannot decide, so leave it to redelivery
//	}
//	if done {
//	    return nil
//	}
//
// The stop decision is the same as AlreadySucceeded's. No record returns false, "not
// completed", and a read failure returns an error (see AlreadySucceeded for why). When it
// returns an error it records nothing either: writing a running record with Attempts and
// QueuedAt reset after losing the carry-over source would serve no purpose, since the caller
// just leaves the task to redelivery.
//
// The status passed should describe the start of processing (running). Recording a
// submission (queued) is not subject to the re-run guard, so use Record for it.
func (r *Recorder[T]) Begin(ctx context.Context, jobID string, status T, apply ...func(next, prev *T)) (bool, error) {
	if !r.Enabled() {
		return false, nil
	}

	prev, prevCommon, err := r.previous(ctx, jobID)
	if err != nil {
		return false, err
	}
	if prevCommon != nil && prevCommon.IsTerminal() {
		return true, nil
	}

	r.save(ctx, jobID, status, prev, prevCommon, apply)
	return false, nil
}

// previous reads the previous record and returns the value and its common fields.
//
// No record (ErrNotFound) is normal for a first record, so it is returned as "no previous"
// (both nil) rather than an error. For a type that does not embed Status the common fields
// are nil, and neither carry-over nor the rollback check happens.
func (r *Recorder[T]) previous(ctx context.Context, jobID string) (*T, *Status, error) {
	status, err := r.store.Get(ctx, jobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	if c, ok := common(&status); ok {
		return &status, &c, nil
	}
	return &status, nil, nil
}

// save saves after going through the carry-over, apply and the rollback check.
func (r *Recorder[T]) save(ctx context.Context, jobID string, status T, prev *T, prevCommon *Status, apply []func(next, prev *T)) {
	if prevCommon != nil {
		if next, ok := any(&status).(Carrier); ok {
			next.CarryOver(*prevCommon)
		}
	}

	for _, fn := range apply {
		if fn != nil {
			fn(&status, prev)
		}
	}

	// The rollback check runs after apply. apply can rewrite the state, so checking anything
	// but the value actually saved would let it slip through.
	next, hasCommon := common(&status)
	if rolledBack(prevCommon, next, hasCommon) {
		r.logger.InfoContext(ctx, "skipped job status record; job already finished",
			"job_id", jobID, "state", next.State, "recorded_state", prevCommon.State)
		return
	}

	if err := r.store.Save(ctx, jobID, status); err != nil {
		attrs := []any{"job_id", jobID, "error", err}
		if hasCommon {
			attrs = append(attrs, "state", next.State)
		}
		r.logger.WarnContext(ctx, "failed to record job status", attrs...)
	}
}

// rolledBack reports whether the previous record is finished but this one is not.
//
// If Cloud Tasks redelivery rolled a finished job back to running or failed, both a polling
// screen and the re-run guard would read "not done yet". This keeps the record side from
// undermining what AlreadySucceeded protects.
//
// queued is the exception. A redelivered task writes running or failed; only a new request
// that went through a handler writes queued. Resubmission under the same job ID happens
// normally in the apps this was ported from (there are both a path where the caller
// specifies job_id and a path that rebuilds from history under the same ID). Blocking queued
// here too would leave the record at succeeded, and the worker's re-run guard would read the
// rebuild as "already done" and stop without ever running it: the guard itself would cause
// the very loss it is meant to prevent.
func rolledBack(prev *Status, next Status, hasCommon bool) bool {
	if prev == nil || !hasCommon || !prev.IsTerminal() {
		return false
	}
	return !next.IsTerminal() && next.State != StateQueued
}

// common extracts the common fields from a value embedding Status.
// For a type that does not embed it ok is false, and neither carry-over nor the rollback check happens.
func common[T any](status *T) (Status, bool) {
	carrier, ok := any(status).(Carrier)
	if !ok {
		return Status{}, false
	}
	return carrier.Common(), true
}
