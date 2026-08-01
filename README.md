# ✍️ Go Job Kit

[![CI](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/shouni/go-job-kit/actions/workflows/ci.yml)
[![Language](https://img.shields.io/badge/Language-Go-blue)](https://golang.org/)
[![Go Version](https://img.shields.io/github/go-mod/go-version/shouni/go-job-kit)](https://golang.org/)
[![GitHub tag (latest by date)](https://img.shields.io/github/v/tag/shouni/go-job-kit)](https://github.com/shouni/go-job-kit/tags)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Reference](https://pkg.go.dev/badge/github.com/shouni/go-job-kit.svg)](https://pkg.go.dev/github.com/shouni/go-job-kit)
[![Status](https://img.shields.io/badge/Status-Draft-orange)](#)

## 🚀 概要 (About) - ジョブ管理を使った開発を最速の軌道へ

**`Go Job Kit`** は、**Cloud Tasks へ投入し、GCS へ成果物を書き出す非同期ジョブ**を扱うサービス向けの共通基盤です。

「ジョブを投入する → 進行状況を記録する → 履歴をページングして一覧する」という骨格は、生成物が何であれ同じ形になります。にもかかわらず各サービスがそれぞれの `internal/` に実装を抱えると、同じコードが少しずつ食い違ったまま増えていきます。本ライブラリはその共通部分だけを抜き出したもので、**成果物そのもののドメインには一切踏み込まない**ことを設計方針としています。

---

## ✨ 提供機能 (Features)

* **ジョブ状態の記録 (`jobstatus`)**
  投入直後 (`queued`) から完了 (`succeeded` / `failed`) までの状態を、リモートストレージ上の JSON として読み書きします。
  生成の成否が Slack 通知にしか残らず、失敗ジョブが UI から消えてしまう問題を解消するための記録層です。
  Cloud Tasks の at-least-once 配信に対する再実行ガードの根拠にもなります。

* **履歴のページング (`paging`)**
  ジョブ ID の一覧を新しい順に切り出し、画面表示に必要なメタデータ（前後ページ・表示範囲）を組み立てます。

* **TTL キャッシュ (`cache`)**
  ジョブ ID をキーとしたメタデータのキャッシュを提供します。

---

## 🧭 設計上の約束 (Invariants)

このライブラリが引き受ける、呼び出し側が意識しなくてよい前提です。

1. **ジョブ ID は必ず正規化してから使う**
   ジョブ ID は URL パスとストレージパスの双方に現れるため、検証はセキュリティ境界を兼ねます。
   ストレージパスの組み立てもキャッシュキーの生成も、ライブラリ内部で `go-utils/jobid` による正規化を通します。
   *（移植元では、この正規化を通す箇所と通さない箇所が混在していました）*

2. **並び順は「新しい順」に統一する**
   `paging` の既定は ID の降順です。これが時系列と一致するのは、ID の先頭が生成時刻で
   始まる場合（プレフィックスを付けないか、全件で同一のプレフィックスを使う場合）に限られます。
   ID の途中に時刻が埋まっている、あるいは複数のプレフィックスが混在する一覧では、
   `WithSortKey` でソートキーを渡してください。

3. **回収の開始は呼び出し側の手順にしない**
   `cache` は生成と同時に期限切れエントリの回収を始めます。開始を利用側の手順にすると、
   呼び忘れがそのままメモリの滞留になるためです。停止は `Close` に集約しています。

4. **状態ファイルは常に最新の 1 世代のみ**
   状態は上書きで更新し、履歴は残しません。CDN・ブラウザにキャッシュさせないため `no-store` を付与します。

---

## プロジェクトレイアウト (Project Layout)

```text    
go-job-kit/
├── jobstatus/       # ジョブ進行状況の記録・取得（リモートストレージ backed）
├── paging/          # ジョブ ID 一覧の 1 始まりページング
└── cache/           # ジョブ ID をキーとした TTL キャッシュ
```

---

## 📦 パッケージ構成 (Package Structure)

| パッケージ | 説明 | 主な提供機能 |
| --- | --- | --- |
| **`jobstatus`** | ジョブのライフサイクル状態を JSON としてストレージへ記録します。 | 状態定義 (`State`, `Status`)、終了判定 (`IsTerminal`)、保存・取得・削除 (`Store`) |
| **`paging`** | ジョブ ID の一覧を新しい順にページングします。 | ページ切り出し (`SelectIDs`)、ソートキーの差し替え (`WithSortKey`)、表示メタデータ (`PageMeta`)、実件数への補正 (`AdjustItemCount`) |
| **`cache`** | ジョブ ID をキーとした TTL 付きインメモリキャッシュです。 | キャッシュ生成 (`NewTTL`)、取得・保存・削除 (`Get` / `Set` / `Delete`)、停止 (`Close`) |

---

## 🚀 クイックスタート (Quick Start)

### ジョブ状態の記録 (`jobstatus`)

サービス固有のフィールドは `jobstatus.Status` を埋め込んだ構造体に持たせます。
埋め込みによって JSON はフラットなまま保たれるため、既存の状態ファイルをそのまま読み書きできます。

```go
type JobStatus struct {
    jobstatus.Status
    OutputDir string `json:"output_dir,omitempty"`
}

// reader / writer には remoteio.InputReader / remoteio.OutputWriter をそのまま渡せます。
store := jobstatus.NewStore[JobStatus](
    reader, writer,
    jobstatus.UnderJobDir("gs://"+bucket+"/jobs"), // → .../jobs/{jobID}/status.json
)

// 投入直後に queued を記録する。JobID と UpdatedAt は Save が打刻します。
_ = store.Save(ctx, jobID, JobStatus{
    Status: jobstatus.Status{State: jobstatus.StateQueued, Command: "generate"},
})

// UI・M2M クライアントから進行状況を追う
st, err := store.Get(ctx, jobID)
if errors.Is(err, jobstatus.ErrNotFound) {
    // 記録前の投入、またはこの機能より前に作られたジョブ
}
```

### 履歴のページング (`paging`)

```go
ids, meta := paging.SelectIDs(jobIDs, page, perPage) // 新しい順に切り出す（引数は変更しません）
items := loadHistories(ctx, ids)                     // 一部の読み込み失敗はスキップ
meta = paging.AdjustItemCount(meta, len(items))      // 実件数に合わせて From/To を補正
```

ジョブ ID にプレフィックスが複数混在する一覧では、ID の文字列比較がプレフィックス順に
なってしまいます。埋め込まれたタイムスタンプなど、別のソートキーを渡してください。

```go
ids, meta := paging.SelectIDs(jobIDs, page, perPage, paging.WithSortKey(embeddedTimestamp))
```

### TTL キャッシュ (`cache`)

`NewTTL` は期限切れエントリの回収まで開始します。使い終わったら `Close` してください。

```go
histories := cache.NewTTL[HistoryItem](10 * time.Minute)
defer histories.Close()

if h, ok := histories.Get(jobID); ok {
    return h, nil
}
```

---

## 主な依存関係 (Dependencies)

| パッケージ | 説明 |
| --- | --- |
| [shouni/go-remote-io](https://github.com/shouni/go-remote-io) | 状態ファイルの読み書き（GCS / S3 / ローカルを透過的に扱う） |
| [shouni/go-utils](https://github.com/shouni/go-utils) | ジョブ ID の検証・正規化 (`jobid`) |
| [jellydator/ttlcache](https://github.com/jellydator/ttlcache) | TTL 付きインメモリキャッシュ |

---

## 収録基準 (What belongs here)

「ジョブ管理」も何でも受け入れてしまう名前のため、収録の可否は以下で判断します。

1. **2 つ以上のサービスから使われる** — 単一サービスでしか使わないものは、そのサービスの `internal/` に置いてください。
2. **サービス固有のドメインを持ち込まない** — 生成物そのものを表す型はここには置きません。状態と ID だけを扱います。
3. **ジョブのライフサイクルに関わる** — 認証・HTTP・通知はそれぞれ `gcp-kit` / `go-http-kit` / `go-notifier` の担当です。

---

## 📜 ライセンス (License)

このプロジェクトは [MIT License](https://opensource.org/licenses/MIT) の下で公開されています。
