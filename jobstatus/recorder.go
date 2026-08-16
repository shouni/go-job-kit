package jobstatus

import (
	"context"
	"errors"
	"log/slog"
)

// StatusStore は、Recorder が必要とする最小限の読み書きです。
// *Store[T] がそのまま満たします。
//
// Recorder が Store 型ではなくインターフェースを受けるのは、利用側が状態の保存先を
// 差し替えられるようにするためです（テストの偽実装や、ストレージ以外への記録）。
type StatusStore[T any] interface {
	Get(ctx context.Context, jobID string) (T, error)
	Save(ctx context.Context, jobID string, status T) error
}

// Carrier は、前回の記録から共通フィールドを引き継ぐために Recorder が使う
// インターフェースです。
//
// Status を埋め込んだ型は、メソッドの昇格によって自動的にこれを満たします。
// 利用側が明示的に実装する必要はありません。埋め込んでいない型を渡した場合、
// 引き継ぎは行われず、渡された値がそのまま保存されます。
type Carrier interface {
	Common() Status
	CarryOver(prev Status)
}

// Recorder は、ワーカーがジョブの進行に合わせて状態を記録するための薄い層です。
//
// 移植元の 3 サービスは、いずれも Store の上に同じ 3 つの振る舞いを重ねていました。
// 個別に持つ必然性がないため、ここへ集約しています。
//
//  1. 完了済みジョブの再実行ガード（AlreadySucceeded）
//  2. 前回記録からの共通フィールドの引き継ぎ（Record）
//  3. 記録の失敗で生成そのものを止めない（警告ログに留める）
//
// 3 は、状態はあくまで観測のための記録であり、書けなかったことを理由に生成を
// 中断するほうが害が大きいためです。
type Recorder[T any] struct {
	store  StatusStore[T]
	logger *slog.Logger
}

type recorderOptions struct {
	logger *slog.Logger
}

// RecorderOption は Recorder の挙動を変更します。
type RecorderOption func(*recorderOptions)

// WithLogger は、記録の失敗を書き出すロガーを差し替えます。
// 既定は slog.Default() です。
func WithLogger(logger *slog.Logger) RecorderOption {
	return func(o *recorderOptions) { o.logger = logger }
}

// NewRecorder は Recorder を構築します。
//
// store が nil の場合、記録は行われません（Enabled が false を返し、他のメソッドは
// 何もしません）。状態の記録を任意機能として組み込めるようにするためのもので、
// 利用側が呼び出しのたびに nil を確かめる必要はありません。
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

// Enabled は、状態の記録先が設定されているかどうかを返します。
func (r *Recorder[T]) Enabled() bool {
	return r != nil && r.store != nil
}

// AlreadySucceeded は、そのジョブが既に完了しているかどうかを返します。
//
// Cloud Tasks は at-least-once 配信なので、通知の失敗などでワーカーがエラーを返すと
// 同じタスクが再配信されます。生成をまるごと呼び直すと生成コストがそのまま二重に
// 発生するため、完了済みならワーカー側でここで打ち切ってください。
//
//	done, err := rec.AlreadySucceeded(ctx, task.JobID)
//	if err != nil {
//	    return err // 判定できないので再配信に委ねる
//	}
//	if done {
//	    return nil
//	}
//
// 未記録（ErrNotFound）は「完了していない」として false を返します。記録が無いのは
// 記録前の投入やこの機能より前のジョブでも起こる、正常な状態だからです。
//
// 状態を読めなかった場合（ErrUnavailable）はエラーを返します。ここで false に倒すと
// 完了済みのジョブを未完了と誤認して生成をやり直し、このガードが防ぐはずのコストを
// ガード自身が発生させます。逆に true に倒すと、未完了のジョブを完了扱いにして
// タスクを ACK してしまい、そのジョブは二度と実行されません。どちらへ倒しても
// 誤りうるので判断を返し、呼び出し側がエラーを返して再配信に委ねられるようにします。
//
// これはジョブ単位のガードです。処理を継続タスクへ分割して実行する場合、その途中
// （state=running）での再配信までは防げません。
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

// Record は、前回の記録から共通フィールドを引き継いだうえで status を保存します。
//
// 引き継ぐのは Attempts・QueuedAt と、status 側が空のときの Title です
// （CarryOver を参照）。ワーカーは毎回タスクから状態を組み立て直すため、これが
// 無いと再試行のたびに試行回数と投入時刻が失われます。
//
// apply は引き継ぎの後、保存の前に呼ばれます。試行回数の加算や、サービス固有
// フィールドの引き継ぎに使ってください。prev は前回の記録で、読めなかったときは
// nil です。未記録（ErrNotFound）は初回の記録として黙って進み、それ以外の読み取り
// 失敗は引き継ぎ元を失っている（Attempts・QueuedAt がこの記録でリセットされる）ため、
// 警告ログを残します。
//
//	// 処理開始を記録し、試行回数を 1 つ進める
//	rec.Record(ctx, task.JobID, newStatus(task, StateRunning), func(next, _ *JobStatus) {
//	    next.Attempts++
//	})
//
// 保存に失敗しても呼び出し側へは伝えず、警告ログに留めます。
func (r *Recorder[T]) Record(ctx context.Context, jobID string, status T, apply ...func(next, prev *T)) {
	if !r.Enabled() {
		return
	}

	var prev *T
	if previous, err := r.store.Get(ctx, jobID); err == nil {
		prev = &previous
		if next, ok := any(&status).(Carrier); ok {
			if old, ok := any(&previous).(Carrier); ok {
				next.CarryOver(old.Common())
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		// 未記録は初回の記録なので正常。それ以外（読み取り失敗・壊れた JSON）は
		// 引き継ぎが失われ、Attempts・QueuedAt がこの記録でリセットされるため、
		// 静かに進めず原因を残す。記録そのものは行う（観測の欠けを理由に増やさない）。
		r.logger.WarnContext(ctx, "failed to read previous job status; carry-over skipped",
			"job_id", jobID, "error", err)
	}

	for _, fn := range apply {
		if fn != nil {
			fn(&status, prev)
		}
	}

	if err := r.store.Save(ctx, jobID, status); err != nil {
		attrs := []any{"job_id", jobID, "error", err}
		if carrier, ok := any(&status).(Carrier); ok {
			attrs = append(attrs, "state", carrier.Common().State)
		}
		r.logger.WarnContext(ctx, "failed to record job status", attrs...)
	}
}
