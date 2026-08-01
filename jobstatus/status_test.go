package jobstatus_test

import (
	"testing"
	"time"

	"github.com/shouni/go-job-kit/jobstatus"
)

// failed を終了扱いにしないこと。Cloud Tasks が再試行しうるため、
// ここで打ち切ると再試行後の成功をクライアントが取り逃がします。
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
}

func TestEnsureJobIDKeepsExistingValue(t *testing.T) {
	t.Parallel()

	status := jobstatus.Status{JobID: "original"}
	status.EnsureJobID("other")

	if status.JobID != "original" {
		t.Errorf("JobID = %q, want original（既存の値が上書きされている）", status.JobID)
	}
}
