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
//
// ワーカーの入口では 1 と 2 が必ず並んで呼ばれ、同じ status.json を 2 度読むことに
// なります。その組み合わせは Begin にまとめてあるので、新しい呼び出しはそちらを
// 使ってください。AlreadySucceeded は、記録を伴わない打ち切り判定のために残して
// あります。
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
// 前回が終了済み（IsTerminal）で今回が running・failed の場合は保存しません。
// Cloud Tasks の再配信で完了済みのジョブが巻き戻ると、ポーリング中の画面も
// 再実行ガードも「まだ終わっていない」と読むためです。**queued は例外で、
// そのまま保存します。** 再配信されたタスクが書くのは running か failed であり、
// queued を書くのは新しい依頼だけなので、同じジョブ ID での作り直しをここで
// 止めてしまわないためです。判定に使う前回の記録は引き継ぎのために既に
// 読んでいるので、ストレージへの往復は増えません。
//
// 保存に失敗しても呼び出し側へは伝えず、警告ログに留めます。
func (r *Recorder[T]) Record(ctx context.Context, jobID string, status T, apply ...func(next, prev *T)) {
	if !r.Enabled() {
		return
	}

	prev, prevCommon, err := r.previous(ctx, jobID)
	if err != nil {
		// 未記録は初回の記録なので previous が握り潰している。ここへ来るのは
		// 読み取り失敗・壊れた JSON で、引き継ぎが失われ Attempts・QueuedAt が
		// この記録でリセットされるため、静かに進めず原因を残す。記録そのものは
		// 行う（観測の欠けを理由に失敗を増やさない）。
		r.logger.WarnContext(ctx, "failed to read previous job status; carry-over skipped",
			"job_id", jobID, "error", err)
	}

	r.save(ctx, jobID, status, prev, prevCommon, apply)
}

// Begin は、再実行ガードと処理開始の記録を 1 回の読み取りで行います。
// 完了済みで何もしなかった場合に true を返します。
//
// ワーカーの入口は AlreadySucceeded で打ち切りを判定してから Record で running を
// 書く、という 2 段でした。Record は引き継ぎのために前回の記録を読み直すので、
// 同じ status.json を 1 回のタスクで 2 度取得していたことになります。判定に必要な
// 情報は引き継ぎ元とまったく同じなので、ここでまとめます。
//
//	done, err := rec.Begin(ctx, task.JobID, newStatus(task, StateRunning),
//	    func(next, _ *JobStatus) { next.Attempts++ })
//	if err != nil {
//	    return err // 判定できないので再配信に委ねる
//	}
//	if done {
//	    return nil
//	}
//
// 打ち切りの判断は AlreadySucceeded と同じです。未記録は「完了していない」として
// false を返し、読めなかった場合はエラーを返します（理由は AlreadySucceeded を
// 参照）。エラーを返すときは記録も行いません。引き継ぎ元を失ったまま Attempts と
// QueuedAt をリセットした running を書き残しても、呼び出し側はそのまま再配信へ
// 委ねるだけだからです。
//
// 渡す status は処理開始（running）を表すものにしてください。投入（queued）の記録は
// 再実行ガードの対象ではないので Record を使います。
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

// previous は前回の記録を読み、値と共通フィールドを返します。
//
// 未記録（ErrNotFound）は初回の記録として正常なので、エラーではなく「前回なし」
// （いずれも nil）として返します。Status を埋め込んでいない型では共通フィールドが
// nil になり、引き継ぎも巻き戻しの判定も行われません。
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

// save は引き継ぎ・apply・巻き戻しの判定を通してから保存します。
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

	// 巻き戻しの判定は apply の後で行います。apply は状態を書き換えられるため、
	// 実際に保存される値で見ないと素通りします。
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

// rolledBack は、前回が終了済みなのに今回がそうでない記録かどうかを返します。
//
// Cloud Tasks の再配信で完了済みのジョブが running や failed へ巻き戻ると、
// ポーリング中の画面も再実行ガードも「まだ終わっていない」と読みます。
// AlreadySucceeded が守ろうとしているものを、記録の側から崩さないためのものです。
//
// queued は例外です。再配信されたタスクが書くのは running か failed であり、
// queued を書くのはハンドラーを通った新しい依頼だけだからです。同じジョブ ID での
// 再投入は移植元で普通に起こります（呼び出し元が job_id を指定する経路と、履歴から
// 同じ ID で作り直す経路の両方があります）。ここで queued まで弾くと、記録は
// succeeded のまま残り、ワーカーの再実行ガードがその作り直しを「完了済み」と読んで
// 一度も実行しないまま打ち切ります。防ぐはずの取りこぼしを、ガード自身が作ります。
func rolledBack(prev *Status, next Status, hasCommon bool) bool {
	if prev == nil || !hasCommon || !prev.IsTerminal() {
		return false
	}
	return !next.IsTerminal() && next.State != StateQueued
}

// common は、Status を埋め込んだ値から共通フィールドを取り出します。
// 埋め込んでいない型では ok が false になり、引き継ぎも巻き戻しの判定も行われません。
func common[T any](status *T) (Status, bool) {
	carrier, ok := any(status).(Carrier)
	if !ok {
		return Status{}, false
	}
	return carrier.Common(), true
}
