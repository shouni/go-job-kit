package jobstatus_test

import (
	json "encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/shouni/go-job-kit/jobstatus"
)

// failed is not treated as finished. Cloud Tasks may retry it, so stopping here would
// make the client miss a success after the retry.
func TestIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state jobstatus.State
		want  bool
	}{
		{jobstatus.StateQueued, false},
		{jobstatus.StateRunning, false},
		{jobstatus.StateSucceeded, true},
		{jobstatus.StateFailed, false},
	}

	for _, tt := range tests {
		if got := (jobstatus.Status{State: tt.state}).IsTerminal(); got != tt.want {
			t.Errorf("Status{State: %q}.IsTerminal() = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestStampSetsJobIDAndUpdatedAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var status jobstatus.Status
	status.Stamp("job-1", now)

	if status.JobID != "job-1" {
		t.Errorf("JobID = %q, want job-1", status.JobID)
	}
	if !status.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", status.UpdatedAt, now)
	}
	// Listings sort by queued_at descending, so a job missing the stamp sinks to the bottom. Fill it when unset.
	if !status.QueuedAt.Equal(now) {
		t.Errorf("QueuedAt = %v, want %v (filled when zero)", status.QueuedAt, now)
	}

	queued := now.Add(-time.Hour)
	status = jobstatus.Status{QueuedAt: queued}
	status.Stamp("job-1", now)
	if !status.QueuedAt.Equal(queued) {
		t.Errorf("QueuedAt = %v, want the existing %v kept", status.QueuedAt, queued)
	}
}

// IsTerminal is for the re-run guard, Finished / InFlight for display and polling, and they
// treat failed differently (not terminal for the former, terminal for the latter). Same rule as gcp-kit's copy.
func TestStatusPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state                        jobstatus.State
		terminal, finished, inFlight bool
	}{
		{jobstatus.StateQueued, false, false, true},
		{jobstatus.StateRunning, false, false, true},
		{jobstatus.StateSucceeded, true, true, false},
		{jobstatus.StateFailed, false, true, false},
	}
	for _, tt := range tests {
		s := jobstatus.Status{State: tt.state}
		if s.IsTerminal() != tt.terminal || s.Finished() != tt.finished || s.InFlight() != tt.inFlight {
			t.Errorf("%s: IsTerminal=%v Finished=%v InFlight=%v, want %v %v %v",
				tt.state, s.IsTerminal(), s.Finished(), s.InFlight(), tt.terminal, tt.finished, tt.inFlight)
		}
	}
}

func TestEnsureJobIDKeepsExistingValue(t *testing.T) {
	t.Parallel()

	status := jobstatus.Status{JobID: "original"}
	status.EnsureJobID("other")

	if status.JobID != "original" {
		t.Errorf("JobID = %q, want original (the existing value was overwritten)", status.JobID)
	}
}

// Pins that CarryOver carries over Command as well as Title. While this was
// unpinned, the doc comment drifted away from the implementation.
func TestCarryOverFillsOnlyEmptyTitleAndCommand(t *testing.T) {
	t.Parallel()

	prev := jobstatus.Status{
		Command:  "generate",
		Title:    "前回の題目",
		State:    jobstatus.StateFailed,
		Error:    "前回の失敗理由",
		Attempts: 2,
		QueuedAt: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	}

	// An empty build gets the previous title and command.
	var blank jobstatus.Status
	blank.CarryOver(prev)

	if blank.Attempts != prev.Attempts {
		t.Errorf("Attempts = %d, want %d", blank.Attempts, prev.Attempts)
	}
	if !blank.QueuedAt.Equal(prev.QueuedAt) {
		t.Errorf("QueuedAt = %v, want %v", blank.QueuedAt, prev.QueuedAt)
	}
	if blank.Title != prev.Title {
		t.Errorf("Title = %q, want %q", blank.Title, prev.Title)
	}
	if blank.Command != prev.Command {
		t.Errorf("Command = %q, want %q", blank.Command, prev.Command)
	}
	if blank.State != "" || blank.Error != "" {
		t.Errorf("State = %q, Error = %q, want both empty (values describing this record do not carry over)", blank.State, blank.Error)
	}

	// Values this build already holds are not overwritten. Some services settle the title
	// mid-generation, so a newly known value must not be clobbered by the old record.
	fresh := jobstatus.Status{Title: "今回の題目", Command: "regenerate"}
	fresh.CarryOver(prev)

	if fresh.Title != "今回の題目" {
		t.Errorf("Title = %q, want 今回の題目 (overwritten by the previous value)", fresh.Title)
	}
	if fresh.Command != "regenerate" {
		t.Errorf("Command = %q, want regenerate (overwritten by the previous value)", fresh.Command)
	}
}

// Pins that Attempts is tagged omitzero, using encoding/json/v2 as Store does.
// v2's omitempty does not treat a numeric 0 as empty, so reverting to omitempty would make
// attempts:0 appear in the status.json of a job that has not started yet.
func TestAttemptsIsOmittedWhileZero(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(jobstatus.Status{JobID: "job-1", State: jobstatus.StateQueued})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "attempts") {
		t.Errorf("attempts is present: %s", encoded)
	}

	encoded, err = json.Marshal(jobstatus.Status{JobID: "job-1", State: jobstatus.StateRunning, Attempts: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"attempts":1`) {
		t.Errorf("attempts is missing: %s", encoded)
	}
}
