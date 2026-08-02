// Package jobstatus は、非同期ジョブの進行状況をリモートストレージ上の JSON として
// 読み書きします。
//
// 生成の成否がこれまで Slack 通知にしか残らず、失敗したジョブが UI から消えていた
// 問題を解消するための記録層です。あわせて、Cloud Tasks の at-least-once 配信に対する
// 再実行ガードの根拠にもなります。
package jobstatus

import "time"

// State はジョブのライフサイクル上の状態です。
type State string

const (
	// StateQueued はキューへ投入済みで、まだワーカーが処理を始めていない状態です。
	StateQueued State = "queued"
	// StateRunning はワーカーが処理中の状態です。
	// 継続タスクへ分割して実行される処理では、引き継がれている間もこの状態です。
	StateRunning State = "running"
	// StateSucceeded は成果物の保存まで完了した状態です。
	StateSucceeded State = "succeeded"
	// StateFailed は処理が失敗した状態です。Cloud Tasks による再試行の対象になり得ます。
	StateFailed State = "failed"
)

// Status は、アプリ間で共通するジョブ進行状況のフィールドです。
//
// 成果物の保存先はサービスごとに形が違う（単一 URI・出力ディレクトリ・複数 URI）ため、
// ここには持たせません。利用側は本構造体を埋め込んだ型を定義してください。
// Go は埋め込み構造体を JSON でフラットに展開するため、既存の status.json を
// そのまま読み書きできます。
//
//	type JobStatus struct {
//	    jobstatus.Status
//	    OutputDir string `json:"output_dir,omitempty"`
//	}
type Status struct {
	JobID   string `json:"job_id"`
	Command string `json:"command,omitempty"`
	State   State  `json:"state"`
	// Title は生成対象の題目が確定した時点で埋まります。
	Title string `json:"title,omitempty"`
	// Error は State が failed のときの失敗理由です。
	Error string `json:"error,omitempty"`
	// Attempts はワーカーが処理を開始した回数です。2 以上なら再試行されています。
	Attempts  int       `json:"attempts,omitempty"`
	QueuedAt  time.Time `json:"queued_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsTerminal は、これ以上状態が変化しない（ポーリングを止めてよい）かどうかを返します。
//
// failed は Cloud Tasks が再試行しうるため終了とはみなしません。
func (s Status) IsTerminal() bool {
	return s.State == StateSucceeded
}

// Stamp は、Store が保存時にジョブ ID と更新時刻を打刻するために呼びます。
// 呼び出し側が UpdatedAt を設定し忘れても記録が残るようにするためのものです。
func (s *Status) Stamp(jobID string, now time.Time) {
	s.JobID = jobID
	s.UpdatedAt = now
}

// Common は、埋め込まれた共通フィールドをそのまま返します。
//
// Status を埋め込んだサービス固有の型から、共通部分だけを型引数なしで取り出すための
// ものです（埋め込みによりメソッドが昇格するため、利用側の実装は要りません）。
func (s Status) Common() Status {
	return s
}

// CarryOver は、前回の記録から引き継ぐべき共通フィールドを取り込みます。
//
// ワーカーは状態が変わるたびにタスクから状態を組み立て直すため、これが無いと
// 再試行のたびに試行回数と投入時刻が失われます。Title は、今回の組み立てで
// 埋まっていない場合にだけ引き継ぎます（生成の途中で題目が確定するサービスが
// あるため、新しく判明した題目を古い値で上書きしないようにするためです）。
//
// State・Error・UpdatedAt は引き継ぎません。いずれも「今回の記録」を表す値で、
// 引き継ぐと成功後に古い失敗理由が残り続けます。
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

// EnsureJobID は、JobID が空のときだけ補います。
// job_id を持たない古い記録を読んだときに、呼び出し側が ID 無しの構造体を
// 受け取らないようにするためのものです。
func (s *Status) EnsureJobID(jobID string) {
	if s.JobID == "" {
		s.JobID = jobID
	}
}

// Stamper は、Store が共通フィールドを維持するために使うインターフェースです。
//
// Status を埋め込んだ型は、ポインタメソッドの昇格によって自動的にこれを満たします。
// 利用側が明示的に実装する必要はありません。
// 逆に Status を埋め込んでいない型を Store に渡した場合、打刻は行われず
// 渡された値がそのまま保存されます。
type Stamper interface {
	Stamp(jobID string, now time.Time)
	EnsureJobID(jobID string)
}
