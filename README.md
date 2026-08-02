# ✍️ Go Job Kit

[![CI](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml)
[![Language](https://img.shields.io/badge/Language-Go-blue)](https://golang.org/)
[![Go Version](https://img.shields.io/github/go-mod/go-version/shouni/go-job-kit)](https://golang.org/)
[![GitHub tag (latest by date)](https://img.shields.io/github/v/tag/shouni/go-job-kit)](https://github.com/shouni/go-job-kit/tags)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Reference](https://pkg.go.dev/badge/github.com/shouni/go-job-kit.svg)](https://pkg.go.dev/github.com/shouni/go-job-kit)

**Cloud Tasks へ投入し、GCS へ成果物を書き出す非同期ジョブ**を扱うサービス向けの共通基盤です。

「ジョブを投入する → 進行状況を記録する → 履歴をページングして一覧する」という骨格は、生成物が音楽であれ動画であれ漫画であれ同じ形になります。
にもかかわらず各サービスがそれぞれの `internal/` に実装を抱えると、同じコードが少しずつ食い違ったまま増えていきます。

本ライブラリはその共通部分だけを抜き出したもので、**成果物そのもののドメインには一切踏み込まない**ことを設計方針としています。

---

## 🗺 全体像 (Overview)

ジョブの一生と、それぞれの局面を担当するパッケージの対応です。

| 局面 | 起きること | 担当 |
| --- | --- | --- |
| **投入** | HTTP ハンドラーが Cloud Tasks へ積み、`queued` を記録する | `jobstatus.Store` |
| **実行** | ワーカーが `running` → `succeeded` / `failed` を記録する。再配信されたタスクはここで打ち切る | `jobstatus.Recorder` |
| **参照** | UI・M2M クライアントが進行状況をポーリングする | `jobstatus.Store` |
| **一覧** | 履歴画面がジョブ ID を集め、ページを切り出し、メタデータを読む | `paging` + `cache` |

---

## 🏗 プロジェクトレイアウト (Project Layout)

```text
go-job-kit/
├── jobstatus/   # ジョブ進行状況の記録・取得（Status / Store / Recorder）
├── paging/      # ジョブ ID 一覧の 1 始まりページング（SelectIDs / LoadPage / PageMeta）
└── cache/       # インメモリキャッシュ（TTL / IDList）
```

---

## 📦 `jobstatus` — ジョブ状態の記録

生成の成否がこれまで Slack 通知にしか残らず、失敗したジョブが UI から完全に消えていた問題を解消するための記録層です。あわせて、Cloud Tasks の at-least-once 配信に対する再実行ガードの根拠にもなります。

### 1. 状態の型を定義する

成果物の保存先はサービスごとに形が違う（単一 URI・出力ディレクトリ・複数 URI）ため、共通フィールドだけを `jobstatus.Status` が持ちます。サービス固有のフィールドは、これを**埋め込んだ**構造体に足してください。

```go
type JobStatus struct {
    jobstatus.Status
    OutputDir string `json:"output_dir,omitempty"`
}
```

Go は埋め込み構造体を JSON でフラットに展開するため、保存される形は次のようになります。既存の `status.json` をそのまま読み書きできるのはこのためです。

```json
{
  "job_id": "c20260726-120000-abcd1234",
  "command": "generate",
  "state": "running",
  "title": "作品名",
  "attempts": 2,
  "queued_at": "2026-07-26T12:00:00Z",
  "updated_at": "2026-07-26T12:00:31Z",
  "output_dir": "gs://bucket/jobs/c20260726-120000-abcd1234"
}
```

> **入れ子のペイロードを導入しないでください。** 埋め込みをやめた時点で JSON の形が変わり、保存済みの状態ファイルが読めなくなります。

### 2. Store で読み書きする

```go
// reader / writer には remoteio.InputReader / remoteio.OutputWriter をそのまま渡せます。
store := jobstatus.NewStore[JobStatus](
    reader, writer,
    jobstatus.UnderJobDir("gs://"+bucket+"/jobs"), // → .../jobs/{jobID}/status.json
)

// 投入直後に queued を記録する。JobID と UpdatedAt は Save が打刻します。
err := store.Save(ctx, jobID, JobStatus{
    Status: jobstatus.Status{State: jobstatus.StateQueued, Command: "generate"},
})

// UI・M2M クライアントから進行状況を追う
status, err := store.Get(ctx, jobID)
if errors.Is(err, jobstatus.ErrNotFound) {
    // 未記録（記録前の投入、この機能より前に作られたジョブ）とは限らず、
    // 読み取り失敗もここへ来ます。404 を返す前に、包まれた原因を残しておきます。
    slog.WarnContext(ctx, "job status unavailable", "job_id", jobID, "error", err)
}
```

ここで注意が要るのは `ErrNotFound` の範囲です。`remoteio` は「未存在」を型付きで返さないため、`Get` は**読み取りに失敗した時点で未記録とみなします**。つまり状態ファイルがまだ無い場合と、権限不足や GCS 障害で読めなかった場合が、どちらも `ErrNotFound` になります。素直に 404 へマップすると、**障害中も 404 が返ります**。

これは意図した設計です。状態が読めないことを理由に処理を止めるより、記録が無いものとして先へ進めるほうが安全なためで、`Recorder.AlreadySucceeded` も同じ理由で「読めない = 未完了」に倒します。切り分けができるよう、原因のエラーは `ErrNotFound` と一緒に包んであります。`errors.Is(err, jobstatus.ErrNotFound)` は真のまま、`err` の文字列には元の失敗理由が残るので、上の例のようにログへ流してください。

配置を自分で決めたい場合は `Locator` を渡します。`UnderJobDir` は「成果物と同じジョブディレクトリ配下に置く」という既定の配置で、履歴削除（プレフィックスの一括削除）で状態ファイルも自動的に片付きます。

```go
locate := func(jobID string) (string, error) {
    return remoteio.BuildGCSURI(bucket, layout.JobStatusPath(jobID)), nil
}
```

ジョブ ID の正規化は `Store` の内部で必ず行われます（`Locator` が受け取るのは正規化済みの ID です）。呼び出し側で `jobid.Sanitize` を通す必要はありません。

### 3. Recorder でワーカーから記録する

ワーカーは状態が変わるたびにタスクから状態を組み立て直します。素朴に書くと、再試行のたびに試行回数と投入時刻が失われます。`Recorder` は前回の記録から共通フィールドを引き継いだうえで保存します。

```go
rec := jobstatus.NewRecorder(store) // store が nil なら記録は行われない

// ── 再実行ガード ──
// Cloud Tasks は at-least-once 配信です。通知の失敗などでワーカーがエラーを返すと
// 同じタスクが再配信され、生成コストがそのまま二重に発生します。
if rec.AlreadySucceeded(ctx, task.JobID) {
    return nil
}

// ── 記録 ──
// Attempts・QueuedAt は前回の記録から引き継がれます（apply はその後に呼ばれます）。
rec.Record(ctx, task.JobID, newStatus(task, jobstatus.StateRunning), func(next, prev *JobStatus) {
    next.Attempts++
    if prev != nil {
        next.OutputDir = prev.OutputDir // サービス固有フィールドの引き継ぎ
    }
})
```

引き継ぎの規則は `Status.CarryOver` にまとまっています。

| フィールド | 引き継ぎ | 理由 |
| --- | --- | --- |
| `Attempts` / `QueuedAt` | する | 組み立て直しのたびに失わせない |
| `Title` / `Command` | 今回が空のときだけ | 生成の途中で確定した題目を、古い値で上書きしない |
| `State` / `Error` / `UpdatedAt` | しない | 「今回の記録」を表す値。成功後に古い失敗理由が残ってしまう |

`Recorder` が受け取るのは `StatusStore[T]` インターフェースです。`*jobstatus.Store[T]` はそのまま渡せます。独自の port（`Save(ctx, status)` など）を保ちたい場合は、薄いアダプタを挟んでください。

### API 一覧

| 種別 | シンボル | 説明 |
| --- | --- | --- |
| 型 | `State` / `StateQueued` `StateRunning` `StateSucceeded` `StateFailed` | ライフサイクル上の状態 |
| 型 | `Status` | 共通フィールド。サービス固有の型へ埋め込んで使う |
| メソッド | `Status.IsTerminal()` | ポーリングを止めてよいか（`succeeded` のみ true） |
| メソッド | `Status.CarryOver(prev)` / `Status.Common()` | 前回記録からの引き継ぎ（`Recorder` が使う） |
| 型 | `Store[T]` / `NewStore` | 保存 (`Save`)・取得 (`Get`)・削除 (`Delete`) |
| 型 | `Locator` / `UnderJobDir` | 状態ファイルの配置 |
| 型 | `Recorder[T]` / `NewRecorder` | 再実行ガード (`AlreadySucceeded`)・引き継ぎ付き記録 (`Record`) |
| エラー | `ErrNotFound` | 未記録**および読み取り失敗**。404 へマップする前に、包まれた原因をログへ残す |

---

## 📄 `paging` — 履歴のページング

ジョブ ID の一覧を新しい順に切り出し、画面表示に必要なメタデータを組み立てます。ページ番号は 1 始まり、`perPage` が 0 以下のときはページングせず全件を返します。

### ページを切り出す

```go
ids, meta := paging.SelectIDs(jobIDs, page, perPage) // 引数のスライスは変更しません
items := loadHistories(ctx, ids)                     // 一部の読み込み失敗はスキップ
meta = paging.AdjustItemCount(meta, len(items))      // 実件数に合わせて From/To を補正
```

`AdjustItemCount` があるのは、「1〜10 件目を表示」と出しながら 8 件しか並ばない、というズレを防ぐためです。一覧は ID を並べてからメタデータ本体を読みにいくため、一部の読み込みに失敗すると表示件数が `SelectIDs` の想定より少なくなります。

`PageMeta` の JSON タグは、移植元のサービスが返している既存のレスポンスと同じ形です。画面と M2M クライアントの双方が依存しているため、変更するときは利用側の追随が要ります。

```go
type PageMeta struct {
    Page       int  `json:"page"`        // 1 始まり。範囲外は最終ページへ丸められる
    PerPage    int  `json:"per_page"`
    Total      int  `json:"total"`       // 一覧全体の件数
    TotalPages int  `json:"total_pages"`
    HasPrev    bool `json:"has_prev"`
    HasNext    bool `json:"has_next"`
    PrevPage   int  `json:"prev_page"`
    NextPage   int  `json:"next_page"`
    From       int  `json:"from"`        // 「n〜m 件目を表示」の n
    To         int  `json:"to"`          // 同じく m
}
```

### 並び順

既定は **ID の降順**です。これが時系列と一致するのは、ID の先頭が生成時刻で始まる場合——プレフィックスを付けないか、全件で同一のプレフィックスを使う場合——に限られます。

用途ごとに異なるプレフィックスを付けた ID が同じ一覧に混在すると、文字列比較はプレフィックス順になってしまいます（時刻部分より前に差が出るため）。その場合は埋め込まれたタイムスタンプを取り出す関数を渡してください。

`go-utils/jobid` の `SortKey` がそのまま使えます。`jobid.New` が生成する形式に加えて、各サービスが独自採番していた形式も読めるため、発行元の違う ID が混在する一覧でも並び順が崩れません。

```go
ids, meta := paging.SelectIDs(jobIDs, page, perPage, paging.WithSortKey(jobid.SortKey))
```

時刻を取り出せない ID では `SortKey` が空文字を返し、降順では末尾に回ります。

### ページ分を並行に読み込む

1 ページ分のメタデータ取得は、直列にするとストレージ読み取りが数十回並びます。かといって並行にすると順序が崩れます。`LoadPage` はこの組み立て（切り出し → 並行読み込み → 件数補正）をまとめて行い、`SelectIDs` が返した並び順をそのまま保ちます。

```go
items, meta, err := paging.LoadPage(ctx, jobIDs, page, perPage, repo.loadHistory,
    paging.WithSortKey(jobid.SortKey), // SelectIDs と同じオプションが使えます
    paging.WithConcurrency(10),            // 既定は 10
)
```

読み込みに失敗した ID は警告ログを残して一覧から取り除かれます。代わりにジョブ ID だけの行を残したい場合は、`load` の中でフォールバック値を返してください（メッセージや代替値の形はサービスごとに違うため、ライブラリ側では持ちません）。

```go
load := func(ctx context.Context, jobID string) (History, error) {
    h, err := repo.load(ctx, jobID)
    if err != nil {
        return History{JobID: jobID, Title: jobID}, nil // 一覧には残す
    }
    return h, nil
}
```

### API 一覧

| 種別 | シンボル | 説明 |
| --- | --- | --- |
| 関数 | `SelectIDs(jobIDs, page, perPage, opts...)` | 新しい順に並べ替えてページを切り出す |
| 関数 | `LoadPage(ctx, jobIDs, page, perPage, load, opts...)` | 切り出し + 並行読み込み + 件数補正 |
| 関数 | `AdjustItemCount(meta, itemCount)` | 実際に読めた件数へ `From`/`To` を補正 |
| 型 | `PageMeta` | 画面がページネーションを描画するためのメタデータ |
| オプション | `WithSortKey(fn)` | 並べ替えのキーを差し替える（両関数で有効） |
| オプション | `WithConcurrency(n)` / `WithLogger(l)` | 同時実行数・ロガー（`LoadPage` でのみ有効） |

---

## ⚡ `cache` — インメモリキャッシュ

履歴一覧はジョブ ID を並べてから 1 件ずつメタデータを読みにいくため、ページをめくるたびに同じオブジェクトを読み直します。それを避けるための薄いキャッシュです。

### メタデータのキャッシュ (`TTL`)

キーはジョブ ID です。`"abc"` と `"dir/abc"` が別エントリになると更新した値が読まれないキャッシュミスになるため、キーはストレージパスと同じ正規化を通して揃えられます。

```go
histories := cache.NewTTL[HistoryItem](10 * time.Minute)
defer histories.Close()

if h, ok := histories.Get(jobID); ok {
    return h, nil
}
```

`NewTTL` は期限切れエントリの**回収まで開始します**。開始を利用側の手順にすると、呼び忘れがそのままメモリの滞留になるためです。使い終わったら `Close` を呼んでください（何度呼んでも、複数のゴルーチンから同時に呼んでも安全です）。

### 一覧走査のキャッシュ (`IDList`)

一覧走査そのもの（プレフィックス配下全体の `List`）は、これが無いと履歴画面を開くたびに走ります。保持期間はメタデータ本体より大幅に短く取り、新しく完成したジョブが一覧に現れるまでの遅延を最小にします。削除・追加は `Invalidate` で即座に反映させます。

```go
jobIDCache := cache.NewIDList(time.Minute)

jobIDs, err := jobIDCache.Load(ctx, prefix, r.collectJobIDs) // 返るのは常に複製
...
jobIDCache.Invalidate(prefix)
```

`TTL` と違い、こちらは回収を開始せず `Close` もありません。保持するのは一覧の単位ごとに 1 エントリだけで、同じキーへの書き込みで上書きされていくためです。逆に言えば、**キーには一覧の単位（走査対象のプレフィックスなど）だけを使ってください。** ジョブ ID のように増え続ける値をキーにすると、期限切れのエントリが回収されないまま溜まります。

### API 一覧

| 種別 | シンボル | 説明 |
| --- | --- | --- |
| 型 | `TTL[T]` / `NewTTL(ttl)` | ジョブ ID をキーとしたメタデータのキャッシュ |
| メソッド | `Get` / `Set` / `Delete` / `Len` / `Close` | 取得・保存・削除・件数・回収の停止 |
| 型 | `IDList` / `NewIDList(ttl)` | ジョブ ID 一覧の短期キャッシュ |
| メソッド | `Load(ctx, key, collect)` / `Invalidate(key)` / `Len` | 走査結果の取得・破棄・件数 |
| 定数 | `DefaultTTL`（10 分）/ `DefaultIDListTTL`（1 分） | `ttl` に 0 以下を渡したときの既定値 |

---

## 🧭 設計上の約束 (Invariants)

このライブラリが引き受ける、呼び出し側が意識しなくてよい前提です。

1. **ジョブ ID は必ず正規化してから使う** — ジョブ ID は URL パスとストレージパスの双方に現れるため、検証はセキュリティ境界を兼ねます。ストレージパスの組み立てもキャッシュキーの生成も、ライブラリ内部で `go-utils/jobid` による正規化を通します。*（移植元では、この正規化を通す箇所と通さない箇所が混在していました）*

2. **並び順は「新しい順」に統一する** — `paging` の既定は ID の降順です。時刻が ID の途中に埋まっている、あるいは複数のプレフィックスが混在する一覧では `WithSortKey` を渡してください。

3. **回収の開始は呼び出し側の手順にしない** — `cache.TTL` は生成と同時に期限切れエントリの回収を始めます。停止は `Close` に集約しています。

4. **状態ファイルは常に最新の 1 世代のみ** — 状態は上書きで更新し、履歴は残しません。CDN・ブラウザにキャッシュさせないため `no-store` を付与します。

5. **状態の記録に失敗しても生成は止めない** — `Recorder` は保存の失敗を警告ログに留め、呼び出し側へは伝えません。状態はあくまで観測のための記録であり、書けなかったことを理由に生成を中断するほうが害が大きいためです。同じ理由で、状態を*読めなかった*ときの再実行ガードは「未完了」側に倒します。

6. **一覧の一部が読めなくてもページ全体は返す** — `LoadPage` は読み込みに失敗した ID を一覧から取り除き、`PageMeta` を実件数へ補正します。ただし `ctx` のキャンセル・期限切れはエラーとして返します。読み込みが軒並み失敗した結果の「0 件」を、正常な空一覧と取り違えさせないためです。

---

## 📥 収録基準 (What belongs here)

「ジョブ管理」も何でも受け入れてしまう名前のため、収録の可否は以下で判断します。

1. **2 つ以上のサービスから使われる** — 単一サービスでしか使わないものは、そのサービスの `internal/` に置いてください。
2. **サービス固有のドメインを持ち込まない** — 生成物そのものを表す型はここには置きません。状態と ID だけを扱います。
3. **ジョブのライフサイクルに関わる** — 認証・HTTP・通知はそれぞれ `gcp-kit` / `go-http-kit` / `go-notifier` の担当です。

---

## 🔗 主な依存関係 (Dependencies)

| パッケージ | 用途 |
| --- | --- |
| [shouni/go-remote-io](https://github.com/shouni/go-remote-io) | 状態ファイルの読み書き（GCS / S3 / ローカルを透過的に扱う） |
| [shouni/go-utils](https://github.com/shouni/go-utils) | ジョブ ID の検証・正規化 (`jobid`) |
| [jellydator/ttlcache](https://github.com/jellydator/ttlcache) | TTL 付きインメモリキャッシュ |

`paging` は標準ライブラリのみに依存します。

---

## 📜 ライセンス (License)

このプロジェクトは [MIT License](https://opensource.org/licenses/MIT) の下で公開されています。
