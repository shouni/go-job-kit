// Package jobstatus reads and writes the progress of asynchronous jobs as JSON in remote
// storage.
//
// It is the record layer that fixes a problem where whether a generation succeeded was left
// only in Slack notifications and failed jobs vanished from the UI. It also gives the re-run
// guard against Cloud Tasks' at-least-once delivery something to stand on.
package jobstatus

import "time"

// State is a job's state in its lifecycle.
type State string

const (
	// StateQueued means the job is on the queue and no worker has started on it yet.
	StateQueued State = "queued"
	// StateRunning means a worker is processing the job.
	// Work split into continuation tasks stays in this state while it is being handed over.
	StateRunning State = "running"
	// StateSucceeded means everything up to saving the artifacts has completed.
	StateSucceeded State = "succeeded"
	// StateFailed means processing failed. It may be retried by Cloud Tasks.
	StateFailed State = "failed"
)

// Status holds the job progress fields shared across apps.
//
// Where artifacts are stored differs in shape per service (a single URI, an output directory,
// several URIs), so it is not kept here. Callers define a type that embeds this struct.
// Go flattens embedded structs in JSON, so existing status.json files are read and written
// as they are.
//
//	type JobStatus struct {
//	    jobstatus.Status
//	    OutputDir string `json:"output_dir,omitempty"`
//	}
//
// The stored status.json is flat. Stopping the embedding makes status files already stored
// in this shape unreadable.
//
//	{
//	  "job_id": "c20260726-120000-abcd1234",
//	  "command": "generate",
//	  "state": "running",
//	  "title": "Work title",
//	  "attempts": 2,
//	  "queued_at": "2026-07-26T12:00:00Z",
//	  "updated_at": "2026-07-26T12:00:31Z",
//	  "output_dir": "gs://bucket/jobs/c20260726-120000-abcd1234"
//	}
//
// So do not introduce a nested payload (a shape that gathers the common fields into a
// separate object).
type Status struct {
	JobID   string `json:"job_id"`
	Command string `json:"command,omitempty"`
	State   State  `json:"state"`
	// Title is filled in once the subject of the generation is settled.
	Title string `json:"title,omitempty"`
	// Error is the reason for failure when State is failed.
	Error string `json:"error,omitempty"`
	// Attempts is how many times a worker has started processing. 2 or more means it was retried.
	//
	// It is omitzero rather than omitempty because Store writes through encoding/json/v2.
	// v2's omitempty does not treat a numeric 0 as empty, so with omitempty the status.json
	// of a job that has not started yet would keep attempts:0.
	Attempts  int       `json:"attempts,omitzero"`
	QueuedAt  time.Time `json:"queued_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsTerminal reports whether the state is one the re-run guard (Recorder.Begin) treats as
// "no need to run again". Only succeeded is true.
//
// failed may be retried by Cloud Tasks, so in the guard's sense it is not terminal.
// Do not use it to decide "can polling stop" or "should the list show it as in progress";
// those are Finished / InFlight (if the queue has max_attempts = 1, failed is the end too).
func (s Status) IsTerminal() bool {
	return s.State == StateSucceeded
}

// Finished reports whether the job reached succeeded or failed (the outcome is known and
// polling can stop). Unlike IsTerminal it includes failed.
func (s Status) Finished() bool {
	return s.State == StateSucceeded || s.State == StateFailed
}

// InFlight reports whether the job is queued or running (no outcome yet).
func (s Status) InFlight() bool {
	return s.State == StateQueued || s.State == StateRunning
}

// Stamp is called by Store to stamp the job ID and update time on save.
// It keeps a record even when the caller forgets to set UpdatedAt.
//
// If QueuedAt is unset it gets now. Listings sort by queued_at descending, so a job missing
// the stamp sinks to the bottom. A job whose submission record alone failed gets this filled
// by its next record (Recorder carries over the previous value, so an existing one is not
// overwritten).
func (s *Status) Stamp(jobID string, now time.Time) {
	s.JobID = jobID
	s.UpdatedAt = now
	if s.QueuedAt.IsZero() {
		s.QueuedAt = now
	}
}

// Common returns the embedded common fields as they are.
//
// It extracts just the common part from a service-specific type embedding Status without a
// type parameter (the method is promoted by embedding, so callers implement nothing).
func (s Status) Common() Status {
	return s
}

// CarryOver takes in the common fields that should carry over from the previous record.
//
// The worker rebuilds the status from the task at every state change, so without this the
// attempt count and submission time would be lost on every retry. Title and Command carry
// over only when this build left them empty (some services settle the title mid-generation,
// and a newly known title must not be overwritten by the old value).
//
// State, Error and UpdatedAt do not carry over. They all describe "this record", and carrying
// them over would leave an old failure reason in place after a success.
func (s *Status) CarryOver(prev Status) {
	s.Attempts = prev.Attempts
	s.QueuedAt = prev.QueuedAt
	if s.Title == "" {
		s.Title = prev.Title
	}
	if s.Command == "" {
		s.Command = prev.Command
	}
}

// EnsureJobID fills in JobID only when it is empty.
// It keeps callers from receiving a struct without an ID when reading an old record that
// has no job_id.
func (s *Status) EnsureJobID(jobID string) {
	if s.JobID == "" {
		s.JobID = jobID
	}
}

// Stamper is the interface Store uses to maintain the common fields.
//
// Types embedding Status satisfy it automatically through pointer-method promotion.
// Callers do not need to implement it explicitly.
// Conversely, when a type that does not embed Status is passed to Store, no stamping
// happens and the value is saved as given.
type Stamper interface {
	Stamp(jobID string, now time.Time)
	EnsureJobID(jobID string)
}
