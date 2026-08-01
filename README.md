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

`ap-comp` / `ap-mv` / `ap-comic` は、いずれも「ジョブを投入する → 進行状況を記録する → 履歴をページングして一覧する」という同じ骨格を持ちながら、その実装を各リポジトリの `internal/` に重複して抱えていました。本ライブラリはその共通部分だけを抜き出し、**アプリ固有のドメイン（楽曲・動画・漫画）には一切踏み込まない**ことを設計方針としています。

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
   *（各アプリのコピー実装では、この正規化を通す箇所と通さない箇所が混在していました）*

2. **ジョブ ID は辞書順で時系列に並ぶ**
   `paging` は「ジョブ ID の降順 = 新しい順」を前提に一覧を切り出します。
   `jobid.New` が生成する時刻プレフィックス付き ID を使う限り成立します。

3. **状態ファイルは常に最新の 1 世代のみ**
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
| **`paging`** | ジョブ ID の一覧を新しい順にページングします。 | ページ切り出し (`SelectIDs`)、表示メタデータ (`PageMeta`)、実件数への補正 (`AdjustItemCount`) |
| **`cache`** | ジョブ ID をキーとした TTL 付きインメモリキャッシュです。 | キャッシュ生成 (`NewTTL`)、取得・保存・削除 (`Get` / `Set` / `Delete`) |

---

## 🚀 クイックスタート (Quick Start)

> ⚠️ 以下は設計の下書きです。API は実装時に変わる可能性があります。

### ジョブ状態の記録 (`jobstatus`)

アプリ固有のフィールドは `jobstatus.Status` を埋め込んだ構造体に持たせます。
埋め込みによって JSON はフラットなまま保たれるため、既存の状態ファイルをそのまま読み書きできます。

```go
type ComicJobStatus struct {
    jobstatus.Status
    OutputDir string `json:"output_dir,omitempty"`
}

store := jobstatus.NewStore[ComicJobStatus](reader, writer, locator)

// 投入直後に queued を記録する
_ = store.Save(ctx, jobID, ComicJobStatus{
    Status: jobstatus.Status{State: jobstatus.StateQueued, Command: "generate_comic"},
})

// UI・M2M クライアントから進行状況を追う
st, err := store.Get(ctx, jobID)
if errors.Is(err, jobstatus.ErrNotFound) {
    // 記録前の投入、またはこの機能より前に作られたジョブ
}
```

### 履歴のページング (`paging`)

```go
ids, meta := paging.SelectIDs(jobIDs, page, perPage) // 新しい順に切り出す
items := loadHistories(ctx, ids)                     // 一部の読み込み失敗はスキップ
meta = paging.AdjustItemCount(meta, len(items))      // 実件数に合わせて From/To を補正
```

### TTL キャッシュ (`cache`)

```go
histories := cache.NewTTL[ComicHistory](10 * time.Minute)

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
2. **アプリ固有のドメインを持ち込まない** — 楽曲・動画・漫画といった成果物の型はここには置きません。状態と ID だけを扱います。
3. **ジョブのライフサイクルに関わる** — 認証・HTTP・通知はそれぞれ `gcp-kit` / `go-http-kit` / `go-notifier` の担当です。

---

## 📜 ライセンス (License)

このプロジェクトは [MIT License](https://opensource.org/licenses/MIT) の下で公開されています。
