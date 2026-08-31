package jobstatus_test

import (
	json "encoding/json/v2"
	"strings"
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

// CarryOver が Title だけでなく Command も引き継ぐことを固定します。ここを
// 留めていなかったあいだに doc コメントの方が実装から取り残されていました。
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

	// 空の組み立てには前回の題目とコマンドが入る。
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
		t.Errorf("State = %q, Error = %q, want どちらも空（今回の記録を表す値は引き継がない）", blank.State, blank.Error)
	}

	// 今回の組み立てが持っている値は上書きされない。生成の途中で題目が確定する
	// サービスがあるため、新しく判明した値を古い記録で潰してはいけません。
	fresh := jobstatus.Status{Title: "今回の題目", Command: "regenerate"}
	fresh.CarryOver(prev)

	if fresh.Title != "今回の題目" {
		t.Errorf("Title = %q, want 今回の題目（前回の値で上書きされている）", fresh.Title)
	}
	if fresh.Command != "regenerate" {
		t.Errorf("Command = %q, want regenerate（前回の値で上書きされている）", fresh.Command)
	}
}

// Attempts のタグが omitzero であることを、Store と同じ encoding/json/v2 で固定します。
// v2 の omitempty は数値の 0 を空とみなさないので、omitempty へ戻すと
// まだ動き出していないジョブの status.json に attempts:0 が現れます。
func TestAttemptsIsOmittedWhileZero(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(jobstatus.Status{JobID: "job-1", State: jobstatus.StateQueued})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "attempts") {
		t.Errorf("attempts が出ています: %s", encoded)
	}

	encoded, err = json.Marshal(jobstatus.Status{JobID: "job-1", State: jobstatus.StateRunning, Attempts: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"attempts":1`) {
		t.Errorf("attempts が落ちています: %s", encoded)
	}
}
